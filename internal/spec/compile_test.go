package spec

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kartikey/workflow_service/internal/core/expr"
)

func TestCompileAndResolveWorkflow(t *testing.T) {
	compiled, diagnostics := Compile([]byte(`
apiVersion: workflow/v1
name: example-flow
timeout: 1h
correlation: 'order-${input.id}'
defaults:
  timeout: 10s
nodes:
  - id: notify
    type: noop
    needs: [fetch]
    if: '${nodes.fetch.status == "succeeded"}'
    config:
      body: '${nodes.fetch.output}'
      count: '${input.count + 1}'
  - id: fetch
    type: noop
    config: {}
outputs:
  result: '${nodes.notify.output}'
`))
	if hasErrors(diagnostics) || compiled == nil {
		t.Fatalf("compile: %+v", diagnostics)
	}
	if !reflect.DeepEqual(compiled.Spec.TopoOrder, []string{"fetch", "notify"}) {
		t.Fatal(compiled.Spec.TopoOrder)
	}
	if compiled.Spec.TimeoutNS != int64(time.Hour) || *compiled.Spec.Nodes[0].TimeoutNS != int64(10*time.Second) {
		t.Fatal("durations not normalized")
	}
	bindings := expr.Bindings{Input: map[string]any{"id": "123", "count": int64(4)}, Nodes: map[string]expr.NodeView{
		"fetch": {Output: map[string]any{"id": "123"}, Status: "succeeded"}, "notify": {Output: "sent"},
	}}
	result, err := compiled.Programs.Nodes["notify"].Config.Resolve(context.Background(), bindings)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"body": map[string]any{"id": "123"}, "count": int64(5)}
	if !reflect.DeepEqual(result, want) {
		t.Fatalf("result = %#v", result)
	}
	if ok, err := compiled.Programs.Nodes["notify"].If.EvalBool(context.Background(), bindings); err != nil || !ok {
		t.Fatalf("condition: %v %v", ok, err)
	}
	if result, err := compiled.Programs.Correlation.Resolve(context.Background(), bindings); err != nil || result != "order-123" {
		t.Fatalf("correlation: %v %v", result, err)
	}
	if _, err := compiled.Programs.Outputs.Resolve(context.Background(), bindings); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(compiled)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "Programs") || strings.Contains(string(encoded), "programs") {
		t.Fatal("serialized CEL programs")
	}
}

func TestCompileCollectsScopedExpressionDiagnostics(t *testing.T) {
	compiled, diagnostics := Compile([]byte(`
apiVersion: workflow/v1
name: example-flow
correlation: '${nodes.first.output}'
nodes:
  - id: first
    type: noop
    if: '${1}'
    config:
      self: '${nodes.first.output}'
      future: '${nodes.second.output}'
      dynamic: '${nodes[input.id]}'
      syntax: '${input.count +}'
  - id: second
    type: noop
    needs: [first]
    config: {}
outputs:
  invalid: '${nodes.unknown.output}'
`))
	if compiled != nil {
		t.Fatal("returned partial workflow")
	}
	expected := map[string]string{
		`nodes[0].config["self"]`:    "E_SELF_REF",
		`nodes[0].config["future"]`:  "E_EXPR_SCOPE",
		`nodes[0].config["dynamic"]`: "E_EXPR_SCOPE",
		`nodes[0].config["syntax"]`:  "E_EXPR_SYNTAX",
		"nodes[0].if":                "E_EXPR_TYPE", "correlation": "E_EXPR_SCOPE",
		`outputs["invalid"]`: "E_EXPR_SCOPE",
	}
	for path, code := range expected {
		found := false
		for _, d := range diagnostics {
			if d.Path == path && d.Code == code {
				found = true
				if strings.HasPrefix(path, "nodes[0]") && d.NodeID != "first" {
					t.Fatal("lost node ID")
				}
			}
		}
		if !found {
			t.Errorf("missing %s at %s; got %+v", code, path, diagnostics)
		}
	}
}

func TestCompileStopsInvalidStructureAndReportsCycle(t *testing.T) {
	for _, tt := range []struct{ source, code string }{
		{"nodes: [", "E_YAML"},
		{"apiVersion: workflow/v1\nname: example-flow\nnodes: []", "E_NO_NODES"},
		{"apiVersion: workflow/v1\nname: example-flow\nnodes:\n- {id: a, type: noop, config: {}, needs: [missing]}", "E_UNKNOWN_NODE"},
		{"apiVersion: workflow/v1\nname: example-flow\nnodes:\n- {id: a, type: noop, config: {}, needs: [b]}\n- {id: b, type: noop, config: {}, needs: [a]}", "E_CYCLE"},
	} {
		compiled, diagnostics := Compile([]byte(tt.source))
		if compiled != nil {
			t.Fatal("accepted invalid workflow")
		}
		found := false
		for _, d := range diagnostics {
			if d.Code == tt.code {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing %s: %+v", tt.code, diagnostics)
		}
	}
}

func TestCompilePreservesWarnings(t *testing.T) {
	compiled, diagnostics := Compile([]byte("apiVersion: workflow/v1\nname: example-flow\nnodes:\n- {id: request, type: http, config: {}}"))
	if compiled == nil || len(diagnostics) != 1 || diagnostics[0].Code != "W_NO_TIMEOUT" {
		t.Fatalf("%+v", diagnostics)
	}
}
