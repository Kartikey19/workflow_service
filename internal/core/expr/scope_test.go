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

func TestItemAndIndexOnlyInsideForeach(t *testing.T) {
	env, err := NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	outside := env.WithScope(Scope{NodeID: "n", AllowedNodes: []string{"fetch"}})
	inside := env.WithScope(Scope{NodeID: "n", AllowedNodes: []string{"fetch"}, Foreach: true})
	for _, source := range []string{"item", "item.id", "index + 1", `[1, 2].map(x, x + index)`} {
		if _, err := inside.Compile(source); err != nil {
			t.Errorf("inside foreach %q: %v", source, err)
		}
		var compileErr *CompileError
		if _, err := outside.Compile(source); !errors.As(err, &compileErr) || compileErr.Code != "E_EXPR_SCOPE" {
			t.Errorf("outside foreach %q: got %v, want E_EXPR_SCOPE", source, err)
		}
	}
	// A comprehension variable called item is a local binding, valid anywhere,
	// and does not leak past its loop.
	for _, source := range []string{`input.xs.map(item, item.id)`, `input.xs.exists(index, index > 1)`} {
		if _, err := outside.Compile(source); err != nil {
			t.Errorf("local binding %q: %v", source, err)
		}
	}
	if _, err := outside.Compile(`input.xs.map(item, item) + [item]`); err == nil {
		t.Error("item after its loop must still be rejected outside foreach")
	}
}

func TestEvalListAndForeachBindings(t *testing.T) {
	env, err := NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	items, err := env.CompileList("${input.users.filter(u, u.active)}")
	if err != nil {
		t.Fatal(err)
	}
	bindings := Bindings{Input: map[string]any{"users": []any{
		map[string]any{"id": "a", "active": true}, map[string]any{"id": "b", "active": false}, map[string]any{"id": "c", "active": true},
	}}}
	list, err := items.EvalList(context.Background(), bindings)
	if err != nil || len(list) != 2 || list[1].(map[string]any)["id"] != "c" {
		t.Fatalf("list = %v, %v", list, err)
	}
	for _, bad := range []string{"input.users", "${1}", "${input.n}x", "${'a'}"} {
		if _, err := env.CompileList(bad); err == nil {
			t.Errorf("CompileList accepted %q", bad)
		}
	}
	scalar, err := env.CompileList("${input.n}") // dynamic: checked at evaluation
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scalar.EvalList(context.Background(), Bindings{Input: map[string]any{"n": int64(3)}}); err == nil {
		t.Fatal("a non-list result was accepted")
	}
	program, err := env.WithScope(Scope{NodeID: "n", Foreach: true}).Compile("item.id + string(index)")
	if err != nil {
		t.Fatal(err)
	}
	value, err := program.Eval(context.Background(), Bindings{Item: map[string]any{"id": "x"}, Index: 7})
	if err != nil || value.Value() != "x7" {
		t.Fatalf("item/index eval = %v, %v", value, err)
	}
}
