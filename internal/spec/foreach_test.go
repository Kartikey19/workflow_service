package spec

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kartikey/workflow_service/internal/core/expr"
)

const foreachBase = `
apiVersion: workflow/v1
name: fanout
inputs:
  ids: {type: array}
nodes:
  - {id: load, type: noop, config: {}}
  - id: each
    type: http
    needs: [load]
    foreach: FOREACH
    config: {url: "https://example.com/${item.id}", body: {n: '${index}'}}
  - id: summary
    type: noop
    needs: [each]
    config: {count: '${size(nodes.each.output)}'}
`

func foreachSource(foreach string) string {
	return strings.Replace(foreachBase, "FOREACH", foreach, 1)
}

func codes(diagnostics []Diagnostic) []string {
	var out []string
	for _, d := range diagnostics {
		if d.Severity == "error" {
			out = append(out, d.Code+" "+d.Path)
		}
	}
	return out
}

func TestForeachCompilesAndNormalises(t *testing.T) {
	compiled, diagnostics := Compile([]byte(foreachSource(`{items: '${input.ids}', max_items: 25}`)))
	if compiled == nil || hasErrors(diagnostics) {
		t.Fatalf("diagnostics = %v", diagnostics)
	}
	var each NodeSpec
	for _, node := range compiled.Spec.Nodes {
		if node.ID == "each" {
			each = node
		}
	}
	if each.Foreach == nil || each.Foreach.Items != "${input.ids}" || each.Foreach.MaxItems != 25 {
		t.Fatalf("foreach = %+v", each.Foreach)
	}
	if compiled.Programs.Nodes["each"].Items == nil || compiled.Programs.Nodes["load"].Items != nil {
		t.Fatal("items program should exist for the foreach node only")
	}

	// The default cap applies when max_items is omitted.
	compiled, _ = Compile([]byte(foreachSource(`{items: '${input.ids}'}`)))
	for _, node := range compiled.Spec.Nodes {
		if node.ID == "each" && node.Foreach.MaxItems != DefaultForeachMaxItems {
			t.Fatalf("default max_items = %d", node.Foreach.MaxItems)
		}
	}
}

func TestForeachRoundTripsThroughLoad(t *testing.T) {
	compiled, diagnostics := Compile([]byte(foreachSource(`{items: '${input.ids}'}`)))
	if compiled == nil {
		t.Fatal(diagnostics)
	}
	encoded := mustJSON(t, compiled.Spec)
	loaded, err := Load(encoded, newTestEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Programs.Nodes["each"].Items == nil {
		t.Fatal("Load did not rebuild the items program")
	}
}

func TestForeachRejections(t *testing.T) {
	tests := []struct {
		name, source, want string
	}{
		{"missing items", foreachSource(`{max_items: 5}`), "E_FOREACH nodes[1].foreach.items"},
		{"max_items too small", foreachSource(`{items: '${input.ids}', max_items: 0}`), "E_FOREACH nodes[1].foreach.max_items"},
		{"max_items too large", foreachSource(`{items: '${input.ids}', max_items: 10001}`), "E_FOREACH nodes[1].foreach.max_items"},
		{"items is not one expression", foreachSource(`{items: 'a ${input.ids}'}`), "E_EXPR_TYPE nodes[1].foreach.items"},
		{"items must be a list", foreachSource(`{items: '${1 + 1}'}`), "E_EXPR_TYPE nodes[1].foreach.items"},
		{"items cannot use item", foreachSource(`{items: '${[item]}'}`), "E_EXPR_SCOPE nodes[1].foreach.items"},
		{"items cannot read a later node", foreachSource(`{items: '${nodes.summary.output}'}`), "E_EXPR_SCOPE nodes[1].foreach.items"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			compiled, diagnostics := Compile([]byte(tt.source))
			if compiled != nil {
				t.Fatal("compiled although it should be rejected")
			}
			found := false
			for _, got := range codes(diagnostics) {
				if got == tt.want {
					found = true
				}
			}
			if !found {
				t.Fatalf("diagnostics = %v, want %q", codes(diagnostics), tt.want)
			}
		})
	}
}

func TestItemAndIndexAreForeachOnly(t *testing.T) {
	// item and index outside a foreach node are a compile error...
	_, diagnostics := Compile([]byte(`
apiVersion: workflow/v1
name: plain
nodes:
  - {id: a, type: noop, config: {x: '${item}'}}
  - {id: b, type: noop, needs: [a], if: '${index > 1}', config: {}}
outputs: {y: '${item}'}
`))
	got := strings.Join(codes(diagnostics), "|")
	for _, want := range []string{`E_EXPR_SCOPE nodes[0].config["x"]`, "E_EXPR_SCOPE nodes[1].if", `E_EXPR_SCOPE outputs["y"]`} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnostics %q lack %q", got, want)
		}
	}
	// ...and a foreach node may not be a branch.
	_, diagnostics = Compile([]byte(`
apiVersion: workflow/v1
name: bad
nodes:
  - id: r
    type: branch
    foreach: {items: '${input.ids}'}
    config: {cases: [], default: r}
`))
	if !strings.Contains(strings.Join(codes(diagnostics), "|"), "E_FOREACH nodes[0].foreach") {
		t.Errorf("a foreach branch was accepted: %v", codes(diagnostics))
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func newTestEnv(t *testing.T) *expr.Env {
	t.Helper()
	env, err := expr.NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	return env
}
