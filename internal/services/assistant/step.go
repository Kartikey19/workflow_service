package assistant

import (
	"encoding/json"
	"errors"

	"github.com/kartikey/workflow_service/internal/spec"
	"go.yaml.in/yaml/v3"
)

// CreatorModel is deliberately fixed; account/model selection belongs to running agents.
const CreatorModel = "z-ai/glm-5.3-flash"

// schemaStructure keeps the shape needed for authoring, never sample or default values.
func schemaStructure(raw []byte) any {
	var schema map[string]any
	if json.Unmarshal(raw, &schema) != nil {
		return nil
	}
	return structuralSchema(schema)
}
func structuralSchema(schema map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"type", "required"} {
		if v, ok := schema[key]; ok {
			out[key] = v
		}
	}
	if properties, ok := schema["properties"].(map[string]any); ok {
		fields := map[string]any{}
		for name, value := range properties {
			if child, ok := value.(map[string]any); ok {
				fields[name] = structuralSchema(child)
			}
		}
		out["properties"] = fields
	}
	for _, key := range []string{"items", "additionalProperties"} {
		switch value := schema[key].(type) {
		case map[string]any:
			out[key] = structuralSchema(value)
		case bool:
			out[key] = value
		}
	}
	for _, key := range []string{"oneOf", "anyOf", "allOf"} {
		if alternatives, ok := schema[key].([]any); ok {
			shapes := []any{}
			for _, value := range alternatives {
				if child, ok := value.(map[string]any); ok {
					shapes = append(shapes, structuralSchema(child))
				}
			}
			out[key] = shapes
		}
	}
	return out
}

// approvedStructure excludes config, instructions, conditions and input defaults.
func approvedStructure(doc *spec.Document) string {
	if doc == nil {
		return "No steps accepted yet."
	}
	inputs := map[string]any{}
	for name, field := range doc.Inputs {
		inputs[name] = map[string]any{"type": field.Type, "required": field.Required}
	}
	steps := []map[string]any{}
	for _, n := range doc.Nodes {
		steps = append(steps, map[string]any{"id": n.ID, "type": n.Type, "needs": n.Needs})
	}
	raw, _ := json.Marshal(map[string]any{"name": doc.Name, "inputs": inputs, "accepted_steps": steps})
	return string(raw)
}

// appendStep preserves accepted values on the server. The model only writes one new node.
func appendStep(approved, proposal string) (string, *spec.Node, error) {
	fragment, err := spec.Decode([]byte(proposal))
	if err != nil {
		return "", nil, errors.New("return one workflow/v1 document with exactly one node")
	}
	if fragment.APIVersion != "workflow/v1" || len(fragment.Nodes) != 1 {
		return "", nil, errors.New("return one workflow/v1 document with exactly one node")
	}
	node := fragment.Nodes[0]
	if approved == "" {
		return proposal, &node, nil
	}
	current, err := spec.Decode([]byte(approved))
	if err != nil {
		return "", nil, errors.New("accepted workflow is unreadable")
	}
	for _, old := range current.Nodes {
		if old.ID == node.ID {
			return "", nil, errors.New("choose a new step ID; accepted steps cannot be replaced")
		}
	}
	var base, next yaml.Node
	if yaml.Unmarshal([]byte(approved), &base) != nil || yaml.Unmarshal([]byte(proposal), &next) != nil {
		return "", nil, errors.New("workflow is unreadable")
	}
	nodes := func(doc *yaml.Node) *yaml.Node {
		if len(doc.Content) == 0 {
			return nil
		}
		root := doc.Content[0]
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value == "nodes" {
				return root.Content[i+1]
			}
		}
		return nil
	}
	existing, addition := nodes(&base), nodes(&next)
	if existing == nil || addition == nil || existing.Kind != yaml.SequenceNode || addition.Kind != yaml.SequenceNode {
		return "", nil, errors.New("nodes must be a list")
	}
	existing.Content = append(existing.Content, addition.Content[0])
	raw, err := yaml.Marshal(&base)
	if err != nil {
		return "", nil, errors.New("encode proposed workflow")
	}
	return string(raw), &node, nil
}

// Compiler messages can quote accepted config values. Only structural feedback is sent back.
func repairDiagnostics(diagnostics []spec.Diagnostic) string {
	safe := []map[string]string{}
	for _, d := range diagnostics {
		safe = append(safe, map[string]string{"code": d.Code, "path": d.Path, "severity": d.Severity})
	}
	raw, _ := json.Marshal(safe)
	return string(raw)
}
