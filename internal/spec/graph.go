package spec

import (
	"fmt"
	"sort"
	"strings"
)

type Graph struct {
	Children     map[string][]string
	Dependencies map[string][]string
	Roots        []string
	Order        []string
	Transitive   map[string][]string
}

func BuildGraph(document *Document) (*Graph, []Diagnostic) {
	graph := &Graph{
		Children:     make(map[string][]string),
		Dependencies: make(map[string][]string),
		Roots:        []string{},
		Order:        []string{},
		Transitive:   make(map[string][]string),
	}

	remaining := make(map[string]int)

	// initialise every node in graph
	for _, node := range document.Nodes {
		graph.Children[node.ID] = []string{}

		dependencies := append([]string{}, node.Needs...)
		sort.Strings(dependencies)
		graph.Dependencies[node.ID] = dependencies

		remaining[node.ID] = len(dependencies)

		if len(dependencies) == 0 {
			graph.Roots = append(graph.Roots, node.ID)
		}

	}

	// now we need to build the childrens
	// "B needs A" creates the edge A → B.
	for _, node := range document.Nodes {
		for _, depedency := range node.Needs {
			graph.Children[depedency] = append(graph.Children[depedency], node.ID)
		}
	}

	sort.Strings(graph.Roots)

	for id := range graph.Children {
		sort.Strings(graph.Children[id])
	}

	// kahn's algorithim
	ready := append([]string{}, graph.Roots...)

	for len(ready) > 0 {
		current := ready[0]
		ready = ready[1:]

		graph.Order = append(graph.Order, current)

		for _, child := range graph.Children[current] {
			remaining[child]--

			if remaining[child] == 0 {
				ready = append(ready, child)
			}
		}

		// Stable output when several nodes are available.
		sort.Strings(ready)
	}

	if len(graph.Order) != len(document.Nodes) {
		cycle := findCycle(graph.Children)

		return nil, []Diagnostic{
			{
				Severity: "error",
				Code:     "E_CYCLE",
				Message: fmt.Sprintf(
					"dependency cycle: %s",
					strings.Join(cycle, " → "),
				),
				Path: "nodes",
			},
		}
	}

	// Dependencies appear before their dependents in topological order.
	// Therefore each dependency's transitive set is already available.
	for _, id := range graph.Order {
		ancestors := make(map[string]bool)

		for _, dependency := range graph.Dependencies[id] {
			ancestors[dependency] = true

			for _, ancestor := range graph.Transitive[dependency] {
				ancestors[ancestor] = true
			}
		}

		graph.Transitive[id] = []string{}

		for ancestor := range ancestors {
			graph.Transitive[id] = append(
				graph.Transitive[id],
				ancestor,
			)
		}

		sort.Strings(graph.Transitive[id])
	}

	return graph, nil

}

// findCycle returns a closed path such as [a, b, c, a].
func findCycle(children map[string][]string) []string {
	const (
		unvisited = 0
		visiting  = 1
		finished  = 2
	)

	state := make(map[string]int)
	position := make(map[string]int)
	stack := []string{}

	var visit func(string) []string

	visit = func(id string) []string {
		state[id] = visiting
		position[id] = len(stack)
		stack = append(stack, id)

		for _, child := range children[id] {
			switch state[child] {
			case unvisited:
				if cycle := visit(child); len(cycle) > 0 {
					return cycle
				}

			case visiting:
				// The child is already on our current traversal path.
				// Everything from that child to here forms the cycle.
				start := position[child]

				cycle := append([]string{}, stack[start:]...)
				cycle = append(cycle, child)

				return cycle
			}
		}

		stack = stack[:len(stack)-1]
		delete(position, id)
		state[id] = finished

		return nil
	}

	// Map iteration order is unspecified, so sort traversal starting points.
	ids := make([]string, 0, len(children))

	for id := range children {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	for _, id := range ids {
		if state[id] == unvisited {
			if cycle := visit(id); len(cycle) > 0 {
				return cycle
			}
		}
	}

	return nil
}
