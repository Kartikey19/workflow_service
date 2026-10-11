package connectors

import (
	"encoding/json"
	"fmt"
	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/pkg/jsonschema"
	"slices"
)

// Validate unresolved workflow fields, then validate resolved values on execution.
func checkActionConfig(a Action, raw json.RawMessage) error {
	var schema struct {
		Required   []string
		Properties map[string]struct{ Type string }
	}
	if err := json.Unmarshal(a.InputSchema, &schema); err != nil {
		return err
	}
	fields := []executor.Field{{Name: "connection", Kind: "string", Required: true}}
	for name, s := range schema.Properties {
		kind := s.Type
		if kind == "integer" {
			kind = "number"
		}
		if kind == "boolean" {
			kind = "bool"
		}
		fields = append(fields, executor.Field{Name: name, Kind: kind, Required: slices.Contains(schema.Required, name)})
	}
	return executor.CheckFields(raw, fields)
}
func decodeAction(a Action, raw json.RawMessage) (string, map[string]any, error) {
	var c map[string]any
	if err := executor.DecodeConfig(raw, &c); err != nil {
		return "", nil, err
	}
	name, _ := c["connection"].(string)
	delete(c, "connection")
	if name == "" {
		return "", nil, executor.Permanent("bad_config", fmt.Errorf("connection is required"))
	}
	var schema any
	_ = json.Unmarshal(a.InputSchema, &schema)
	if problems := jsonschema.Validate(schema, c); len(problems) > 0 {
		return "", nil, executor.Permanent("bad_config", fmt.Errorf("invalid %s arguments: %v", a.Name, problems))
	}
	return name, c, nil
}
