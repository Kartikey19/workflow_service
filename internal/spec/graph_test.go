package spec

import (
	"reflect"
	"testing"
)

func TestBuildGraphDiamond(t *testing.T) {
	document := &Document{
		// Deliberately not in execution order.
		Nodes: []Node{
			{ID: "d", Needs: []string{"b", "c"}},
			{ID: "c", Needs: []string{"a"}},
			{ID: "a"},
			{ID: "b", Needs: []string{"a"}},
		},
	}

	graph, diagnostics := BuildGraph(document)

	if len(diagnostics) > 0 {
		t.Fatalf("unexpected diagnostics: %+v", diagnostics)
	}

	if !reflect.DeepEqual(graph.Roots, []string{"a"}) {
		t.Errorf("roots = %v, want [a]", graph.Roots)
	}

	if !reflect.DeepEqual(
		graph.Order,
		[]string{"a", "b", "c", "d"},
	) {
		t.Errorf("order = %v, want [a b c d]", graph.Order)
	}

	if !reflect.DeepEqual(
		graph.Children["a"],
		[]string{"b", "c"},
	) {
		t.Errorf("children of a = %v, want [b c]", graph.Children["a"])
	}

	if !reflect.DeepEqual(
		graph.Dependencies["d"],
		[]string{"b", "c"},
	) {
		t.Errorf(
			"dependencies of d = %v, want [b c]",
			graph.Dependencies["d"],
		)
	}

	if !reflect.DeepEqual(
		graph.Transitive["d"],
		[]string{"a", "b", "c"},
	) {
		t.Errorf(
			"transitive dependencies of d = %v, want [a b c]",
			graph.Transitive["d"],
		)
	}
}

func TestBuildGraphReportsCyclePath(t *testing.T) {
	document := &Document{
		Nodes: []Node{
			{ID: "a", Needs: []string{"c"}},
			{ID: "b", Needs: []string{"a"}},
			{ID: "c", Needs: []string{"b"}},
			// Depends on the cycle but does not belong to it.
			{ID: "d", Needs: []string{"c"}},
		},
	}

	graph, diagnostics := BuildGraph(document)

	if graph != nil {
		t.Fatal("expected no graph for a cyclic workflow")
	}

	if len(diagnostics) != 1 {
		t.Fatalf("diagnostics = %+v, want one", diagnostics)
	}

	if diagnostics[0].Code != "E_CYCLE" {
		t.Errorf("code = %q, want E_CYCLE", diagnostics[0].Code)
	}

	want := "dependency cycle: a → b → c → a"

	if diagnostics[0].Message != want {
		t.Errorf(
			"message = %q, want %q",
			diagnostics[0].Message,
			want,
		)
	}
}
