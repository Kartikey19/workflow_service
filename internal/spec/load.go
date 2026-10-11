package spec

import (
	"encoding/json"
	"fmt"

	"github.com/kartikey/workflow_service/internal/core/expr"
)

// Load rebuilds programs from a published spec. Scope and type checks already
// ran at publish time, so an unscoped environment is sufficient; compiling from
// the stored spec, not the YAML, keeps a version's behaviour fixed even when the
// compiler changes later.
func Load(data []byte, env *expr.Env) (*Compiled, error) {
	var s Spec
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("decode published spec: %w", err)
	}
	programs := Programs{Nodes: make(map[string]NodePrograms, len(s.Nodes))}
	for _, node := range s.Nodes {
		config, err := env.CompileTemplate(node.Config)
		if err != nil {
			return nil, fmt.Errorf("compile config of node %q: %w", node.ID, err)
		}
		compiled := NodePrograms{Config: config}
		if node.Foreach != nil {
			if compiled.Items, err = env.CompileList(node.Foreach.Items); err != nil {
				return nil, fmt.Errorf("compile foreach items of node %q: %w", node.ID, err)
			}
		}
		if node.If != "" {
			compiled.If, err = env.CompileCondition(node.If)
			if err != nil {
				return nil, fmt.Errorf("compile condition of node %q: %w", node.ID, err)
			}
		}
		programs.Nodes[node.ID] = compiled
	}
	if s.Correlation != "" {
		correlation, err := env.CompileTemplate(s.Correlation)
		if err != nil {
			return nil, fmt.Errorf("compile correlation: %w", err)
		}
		programs.Correlation = correlation
	}
	outputs, err := env.CompileTemplate(s.Outputs)
	if err != nil {
		return nil, fmt.Errorf("compile outputs: %w", err)
	}
	programs.Outputs = outputs
	return &Compiled{Spec: s, Programs: programs}, nil
}
