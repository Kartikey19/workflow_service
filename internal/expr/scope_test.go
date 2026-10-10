package expr

import (
	"context"
	"errors"
	"testing"
)

func TestScopeChecksAST(t *testing.T) {
	env, err := NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	scoped := env.WithScope(Scope{NodeID: "current", AllowedNodes: []string{"fetch"}})
	tests := []struct{ source, code string }{
		{"nodes.fetch.output", ""}, {`nodes["fetch"].output`, ""},
		{"has(nodes.fetch.output)", ""}, {".nodes.fetch.output", ""},
		{`"nodes.forbidden.output"`, ""},
		{`[1,2].map(nodes, nodes + 1)`, ""},
		{`[nodes.fetch.output].map(x, x)`, ""},
		{"nodes.current.output", "E_SELF_REF"}, {`nodes["current"]`, "E_SELF_REF"},
		{"nodes.other.output", "E_EXPR_SCOPE"}, {".nodes.other.output", "E_EXPR_SCOPE"},
		{`nodes[input.id].output`, "E_EXPR_SCOPE"},
		{`nodes["fet" + "ch"]`, "E_EXPR_SCOPE"},
		{`nodes`, "E_EXPR_SCOPE"}, {`size(nodes)`, "E_EXPR_SCOPE"},
		{`nodes.map(x,x)`, "E_EXPR_SCOPE"},
		{`[1].map(nodes,.nodes.other.output)`, "E_EXPR_SCOPE"},
		{`[nodes.other.output].map(nodes,nodes)`, "E_EXPR_SCOPE"},
		{`[1].exists(x, nodes.other.status == "succeeded")`, "E_EXPR_SCOPE"},
		{`true ? nodes.fetch.output : nodes.other.output`, "E_EXPR_SCOPE"},
	}
	for _, tt := range tests {
		t.Run(tt.source, func(t *testing.T) {
			_, err := scoped.Compile(tt.source)
			if tt.code == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var compileErr *CompileError
			if !errors.As(err, &compileErr) || compileErr.Code != tt.code {
				t.Fatalf("got %v, want %s", err, tt.code)
			}
		})
	}
	// Constructing a scoped environment must not change its parent.
	if _, err := env.Compile("nodes.other.output"); err != nil {
		t.Fatal(err)
	}
}

func TestConditionTypeChecks(t *testing.T) {
	env, err := NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"true", "${1}", "prefix ${true}", "${true}${false}"} {
		if _, err := env.CompileCondition(source); err == nil {
			t.Fatalf("accepted %s", source)
		}
	}
	program, err := env.CompileCondition("${input.enabled}")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := program.EvalBool(context.Background(), Bindings{Input: map[string]any{"enabled": true}}); err != nil || !got {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := program.EvalBool(context.Background(), Bindings{Input: map[string]any{"enabled": "yes"}}); err == nil {
		t.Fatal("accepted non-boolean dynamic result")
	}
}
