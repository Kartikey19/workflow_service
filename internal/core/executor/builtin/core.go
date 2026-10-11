// Package builtin implements the engine's built-in node types: noop, transform, branch,
// sleep, wait_event and human_task. Control flow is expressed by returning executor.Control,
// never by touching engine state (03 §3).
package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/pkg/jsonschema"
)

// All returns the built-in executors.
func All() []executor.Executor {
	return []executor.Executor{Noop{}, Transform{}, Branch{}, Sleep{}, WaitEvent{}, HumanTask{}}
}

// Transform returns its resolved value: pure CEL glue, no I/O (02 §4.2).
type Transform struct{}

func (Transform) Type() string { return "transform" }

func (Transform) Describe() executor.Description {
	return executor.Description{
		Title: "Transform", Category: "data",
		Description: "Reshapes data with expressions. Output is the resolved value.",
		Example: `- id: shape
  type: transform
  needs: [fetch]
  config:
    value:
      customer: ${nodes.fetch.output.body.customer.name}
      total: ${nodes.fetch.output.body.items.map(i, i.price).sum()}`,
		Output: "the resolved value",
	}
}

func (Transform) ValidateConfig(raw json.RawMessage) error {
	return executor.CheckFields(raw, []executor.Field{{Name: "value", Kind: "any", Required: true}})
}

func (Transform) Execute(ctx context.Context, req executor.Request) (executor.Result, error) {
	var config struct {
		Value json.RawMessage `json:"value"`
	}
	if err := executor.DecodeConfig(req.Config, &config); err != nil {
		return executor.Result{}, err
	}
	if len(config.Value) == 0 {
		config.Value = json.RawMessage(`null`)
	}
	return executor.Result{Output: config.Value}, ctx.Err()
}

// Branch chooses one successor; the resolver skips the other arms (02 §4.3).
type Branch struct{}

func (Branch) Type() string { return "branch" }

func (Branch) Describe() executor.Description {
	return executor.Description{
		Title: "Branch", Category: "control",
		Description: "Routes to the first case whose `when` is true, else `default`. Arms not taken are skipped; merge them later with join: any.",
		Example: `- id: route
  type: branch
  needs: [classify]
  config:
    cases:
      - when: ${nodes.classify.output.severity == "P0"}
        goto: page_oncall
    default: auto_reply`,
		Output: `{"chosen": "<node id>"}`,
	}
}

type branchConfig struct {
	Cases []struct {
		When any    `json:"when"`
		Goto string `json:"goto"`
	} `json:"cases"`
	Default string `json:"default"`
}

func (Branch) ValidateConfig(raw json.RawMessage) error {
	if err := executor.CheckFields(raw, []executor.Field{
		{Name: "cases", Kind: "array", Required: true},
		{Name: "default", Kind: "string"},
	}); err != nil {
		return err
	}
	var config branchConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		return errors.New("cases must be a list of {when, goto}")
	}
	if len(config.Cases) == 0 {
		return errors.New("cases must contain at least one case")
	}
	for i, c := range config.Cases {
		if c.Goto == "" {
			return fmt.Errorf("cases[%d].goto is required", i)
		}
		if c.When == nil {
			return fmt.Errorf("cases[%d].when is required", i)
		}
	}
	return nil
}

func (Branch) Execute(ctx context.Context, req executor.Request) (executor.Result, error) {
	var config branchConfig
	if err := executor.DecodeConfig(req.Config, &config); err != nil {
		return executor.Result{}, err
	}
	chosen := config.Default
	for i, c := range config.Cases {
		matched, ok := c.When.(bool)
		if !ok {
			return executor.Result{}, executor.Permanent("bad_config", fmt.Errorf("cases[%d].when must evaluate to a boolean", i))
		}
		if matched {
			chosen = c.Goto
			break
		}
	}
	output, err := executor.JSON(map[string]any{"chosen": chosen})
	return executor.Result{Output: output, Control: &executor.Control{Goto: chosen}}, err
}

// Sleep parks the node until a duration elapses or an instant passes. A parked
// node holds one row and no goroutine (02 §4.4).
type Sleep struct{}

func (Sleep) Type() string { return "sleep" }

func (Sleep) Describe() executor.Description {
	return executor.Description{
		Title: "Sleep", Category: "control",
		Description: "Waits for a duration or until a timestamp without holding any worker.",
		Example: `- id: wait_a_day
  type: sleep
  needs: [send_welcome]
  config: {duration: 24h}`,
		Output: `{"until": "<RFC 3339>"}`,
	}
}

func (Sleep) ValidateConfig(raw json.RawMessage) error {
	if err := executor.CheckFields(raw, []executor.Field{
		{Name: "duration", Kind: "duration"}, {Name: "until", Kind: "string"},
	}); err != nil {
		return err
	}
	var config map[string]any
	_ = json.Unmarshal(raw, &config)
	if (config["duration"] == nil) == (config["until"] == nil) {
		return errors.New("set exactly one of duration or until")
	}
	return nil
}

func (Sleep) Execute(ctx context.Context, req executor.Request) (executor.Result, error) {
	var config struct {
		Duration string `json:"duration"`
		Until    string `json:"until"`
	}
	if err := executor.DecodeConfig(req.Config, &config); err != nil {
		return executor.Result{}, err
	}
	var until time.Time
	if config.Until != "" {
		parsed, err := time.Parse(time.RFC3339, config.Until)
		if err != nil {
			return executor.Result{}, executor.Permanent("bad_config", errors.New("until must be an RFC 3339 timestamp"))
		}
		until = parsed
	} else {
		duration, err := time.ParseDuration(config.Duration)
		if err != nil || duration <= 0 {
			return executor.Result{}, executor.Permanent("bad_config", errors.New("duration must be positive, such as 10m"))
		}
		until = time.Now().Add(duration)
	}
	output, err := executor.JSON(map[string]any{"until": until.UTC().Format(time.RFC3339)})
	return executor.Result{Output: output, Control: &executor.Control{Sleep: &until}}, err
}

// WaitEvent parks the node until POST /v1/signals delivers its key, or the
// timeout routes to on_timeout (02 §4.5). The signal payload is the output.
type WaitEvent struct{}

const defaultWaitTimeout = 24 * time.Hour

func (WaitEvent) Type() string { return "wait_event" }

func (WaitEvent) Describe() executor.Description {
	return executor.Description{
		Title: "Wait for event", Category: "control",
		Description: "Pauses until an external system sends a signal with this key (POST /v1/signals), e.g. an approval. Output is the signal payload.",
		Example: `- id: await_approval
  type: wait_event
  needs: [request_approval]
  on_timeout: escalate
  config:
    key: "approval:${input.request_id}"
    timeout: 48h
    schema: {type: object, required: [approved]}`,
		Output: "the signal payload",
	}
}

func (WaitEvent) ValidateConfig(raw json.RawMessage) error {
	if err := executor.CheckFields(raw, []executor.Field{
		{Name: "key", Kind: "string", Required: true},
		{Name: "timeout", Kind: "duration"},
		{Name: "schema", Kind: "object"},
	}); err != nil {
		return err
	}
	var config map[string]any
	_ = json.Unmarshal(raw, &config)
	if schema, ok := config["schema"]; ok {
		if err := jsonschema.Check(schema); err != nil {
			return fmt.Errorf("schema: %w", err)
		}
	}
	return nil
}

func (WaitEvent) Execute(ctx context.Context, req executor.Request) (executor.Result, error) {
	var config struct {
		Key     string          `json:"key"`
		Timeout string          `json:"timeout"`
		Schema  json.RawMessage `json:"schema"`
	}
	if err := executor.DecodeConfig(req.Config, &config); err != nil {
		return executor.Result{}, err
	}
	if config.Key == "" {
		return executor.Result{}, executor.Permanent("bad_config", errors.New("key resolved to an empty string"))
	}
	timeout := defaultWaitTimeout
	if config.Timeout != "" {
		parsed, err := time.ParseDuration(config.Timeout)
		if err != nil || parsed <= 0 {
			return executor.Result{}, executor.Permanent("bad_config", errors.New("timeout must be positive, such as 45m"))
		}
		timeout = parsed
	}
	return executor.Result{Control: &executor.Control{Wait: &executor.WaitSpec{
		Key: config.Key, Expires: time.Now().Add(timeout), Schema: config.Schema,
	}}}, nil
}
