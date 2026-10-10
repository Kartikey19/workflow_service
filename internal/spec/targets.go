package spec

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	"github.com/kartikey/workflow_service/internal/executor"
)

// validateTargets requires every control-transfer target to name its source in
// needs, so the graph stays a real DAG and the resolver has an edge to follow.
func validateTargets(document *Document) []Diagnostic {
	var diagnostics []Diagnostic
	needs := make(map[string][]string, len(document.Nodes))
	for _, node := range document.Nodes {
		needs[node.ID] = node.Needs
	}
	check := func(source, target, code, path string) {
		if target == "" {
			return
		}
		if _, ok := needs[target]; !ok {
			if code == "E_BRANCH_TARGET" {
				// needs, on_failure and on_timeout are checked by Validate.
				diagnostics = append(diagnostics, Diagnostic{
					Severity: "error", Code: "E_UNKNOWN_NODE", NodeID: source, Path: path,
					Message: fmt.Sprintf("referenced node %q does not exist", target),
				})
			}
			return
		}
		if !slices.Contains(needs[target], source) {
			diagnostics = append(diagnostics, Diagnostic{
				Severity: "error", Code: code, NodeID: source, Path: path,
				Message: fmt.Sprintf("target %q must list %q in needs", target, source),
			})
		}
	}
	for i, node := range document.Nodes {
		path := fmt.Sprintf("nodes[%d]", i)
		check(node.ID, node.OnFailure, "E_FAILURE_TARGET", path+".on_failure")
		check(node.ID, node.OnTimeout, "E_FAILURE_TARGET", path+".on_timeout")
		if node.Type == "branch" {
			for _, target := range branchTargetPaths(node.Config) {
				check(node.ID, target.id, "E_BRANCH_TARGET", path+".config."+target.path)
			}
		}
	}
	return diagnostics
}

type branchTarget struct{ id, path string }

// branchTargetPaths lists goto targets and the default of a branch config with
// their config paths.
func branchTargetPaths(config map[string]any) []branchTarget {
	var targets []branchTarget
	if cases, ok := config["cases"].([]any); ok {
		for i, item := range cases {
			if c, ok := item.(map[string]any); ok {
				if target, ok := c["goto"].(string); ok && target != "" {
					targets = append(targets, branchTarget{target, fmt.Sprintf("cases[%d].goto", i)})
				}
			}
		}
	}
	if target, ok := config["default"].(string); ok && target != "" {
		targets = append(targets, branchTarget{target, "default"})
	}
	return targets
}

// branchTargets lists goto targets and the default of a branch config.
func branchTargets(config map[string]any) []string {
	var ids []string
	for _, target := range branchTargetPaths(config) {
		ids = append(ids, target.id)
	}
	return ids
}

func validateExecutors(document *Document, registry *executor.Registry) []Diagnostic {
	if registry == nil {
		return nil
	}
	var diagnostics []Diagnostic
	for i, node := range document.Nodes {
		path := fmt.Sprintf("nodes[%d]", i)
		ex, ok := registry.Get(node.Type)
		if !ok {
			diagnostics = append(diagnostics, Diagnostic{
				Severity: "error", Code: "E_UNKNOWN_TYPE", NodeID: node.ID, Path: path + ".type",
				Message: fmt.Sprintf("node type %q is not available", node.Type),
			})
			continue
		}
		raw, err := json.Marshal(node.Config)
		if err == nil {
			err = ex.ValidateConfig(raw)
		}
		if err != nil {
			diagnostics = append(diagnostics, Diagnostic{
				Severity: "error", Code: "E_CONFIG", NodeID: node.ID, Path: path + ".config",
				Message: err.Error(),
			})
		}
	}
	return diagnostics
}

// branchJoinWarnings reports join: all nodes whose needs span different arms of
// one branch: one arm is always skipped, so the node is always skipped too.
func branchJoinWarnings(document *Document, graph *Graph) []Diagnostic {
	var diagnostics []Diagnostic
	descendants := func(start string) map[string]bool {
		seen := map[string]bool{start: true}
		queue := []string{start}
		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]
			for _, child := range graph.Children[current] {
				if !seen[child] {
					seen[child] = true
					queue = append(queue, child)
				}
			}
		}
		return seen
	}
	for _, branch := range document.Nodes {
		if branch.Type != "branch" {
			continue
		}
		targets := branchTargets(branch.Config)
		arms := make([]map[string]bool, len(targets))
		for i, target := range targets {
			arms[i] = descendants(target)
		}
		for i, node := range document.Nodes {
			if node.Join == nil || *node.Join != "all" || len(node.Needs) < 2 {
				continue
			}
			// A dependency counts for an arm only when it lies on that arm
			// alone; a node after an earlier merge belongs to every arm.
			hit := map[int]bool{}
			for _, dependency := range node.Needs {
				only := -1
				for arm, members := range arms {
					if members[dependency] {
						if only != -1 {
							only = -2
							break
						}
						only = arm
					}
				}
				if only >= 0 {
					hit[only] = true
				}
			}
			if len(hit) > 1 {
				names := make([]string, 0, len(hit))
				for arm := range hit {
					names = append(names, targets[arm])
				}
				sort.Strings(names)
				diagnostics = append(diagnostics, Diagnostic{
					Severity: "warning", Code: "W_JOIN_ALL_ACROSS_BRANCH", NodeID: node.ID,
					Path:    fmt.Sprintf("nodes[%d].join", i),
					Message: fmt.Sprintf("needs span branch %q arms %v; use join: any or this node is always skipped", branch.ID, names),
				})
			}
		}
	}
	return diagnostics
}
