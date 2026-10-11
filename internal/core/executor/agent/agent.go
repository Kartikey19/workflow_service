// Package agent implements the agent node type (02 §4.6): a model call with a
// bounded tool-calling loop, run as one durable step. The loop is deliberately
// inside the executor; a half-finished model turn is not resumable, so it is
// not modelled as DAG nodes.
package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/pkg/jsonschema"
)

const (
	maxIterationsLimit = 50
	defaultMaxTokens   = 4096
	maxTokensLimit     = 64000
)

// Executor is the agent node type.
type Executor struct {
	clients Clients
	tools   *Tools
	defs    Definitions
	dynamic DynamicTools
}

var _ executor.Executor = (*Executor)(nil)

// New builds the executor. A nil tools registry means no tools are available; a
// nil clients means no provider is configured; a nil defs means a node cannot
// refer to a stored agent by name (only inline configuration works).
func New(clients Clients, tools *Tools, defs Definitions) *Executor {
	if clients == nil {
		clients = Unconfigured{}
	}
	return &Executor{clients: clients, tools: tools, defs: defs}
}

func (*Executor) Type() string { return "agent" }

func (e *Executor) Describe() executor.Description {
	return executor.Description{
		Title: "Agent", Category: "ai",
		Description: "Calls a language model, optionally letting it use registered tools in a loop capped by max_iterations. " +
			"Run a stored agent with `agent: <name>` and `input`, or configure model, prompt and limits inline. " +
			"With an output schema the reply must be JSON that matches it; a violation fails the node permanently. " +
			"429 and 5xx responses are retried. Available tools: " + toolList(e.tools) + ".",
		Example: `- id: classify
  type: agent
  needs: [fetch_ticket]
  config:
    agent: ticket-classifier          # defined under /v1/agents
    input: {text: '${nodes.fetch_ticket.output.body.description}'}

# or, inline:
- id: classify
  type: agent
  config:
    model: claude-sonnet-5-5
    system: "Classify the ticket. Respond with JSON only."
    prompt: ${nodes.fetch_ticket.output.body.description}
    max_iterations: 6
    output_schema:
      type: object
      required: [severity]
      properties:
        severity: {type: string, enum: [P0, P1, P2]}`,
		Output: `the validated JSON reply when there is an output schema, otherwise {"text": "<reply>"}; ` +
			`metrics: iterations, tokens_in, tokens_out, cost_usd, model, stop_reason, tool_calls (and agent, agent_revision)`,
	}
}

func toolList(tools *Tools) string {
	names := tools.Names()
	if len(names) == 0 {
		return "none"
	}
	out := ""
	for i, name := range names {
		if i > 0 {
			out += ", "
		}
		out += name
	}
	return out
}

type config struct {
	// Agent mode: run a stored definition.
	Agent string          `json:"agent"`
	Input json.RawMessage `json:"input"`

	// Inline mode: configure the call here.
	Model              string          `json:"model"`
	System             string          `json:"system"`
	Prompt             json.RawMessage `json:"prompt"`
	Tools              []string        `json:"tools"`
	DatabaseConnection string          `json:"database_connection"`
	MaxIterations      int             `json:"max_iterations"`
	MaxTokens          int             `json:"max_tokens"`
	Temperature        *float64        `json:"temperature"`
	OutputSchema       json.RawMessage `json:"output_schema"`
}

// inlineOnly are the fields that, with `agent`, would conflict with the stored
// definition.
var inlineOnly = []string{"model", "system", "tools", "max_iterations", "max_tokens", "temperature", "output_schema", "database_connection"}

// ValidateConfig checks the shape at publish time. A node either names a stored
// agent (`agent`, with optional `input` and `prompt`) or configures the call
// inline. Inline max_iterations is mandatory and must be a literal: the run-level
// timeout is far too coarse to bound agent spend. (A stored agent's limits are
// checked when the agent is saved.)
func (e *Executor) ValidateConfig(raw json.RawMessage) error {
	var c map[string]any
	if err := json.Unmarshal(raw, &c); err != nil || c == nil {
		return errors.New("config must be an object")
	}
	if _, named := c["agent"]; named {
		return e.validateNamed(raw, c)
	}
	return e.validateInline(raw, c)
}

func (e *Executor) validateNamed(raw json.RawMessage, c map[string]any) error {
	if err := executor.CheckFields(raw, []executor.Field{
		{Name: "agent", Kind: "string", Required: true},
		{Name: "input", Kind: "any"},
		{Name: "prompt", Kind: "any"},
	}); err != nil {
		for _, name := range inlineOnly {
			if _, present := c[name]; present {
				return fmt.Errorf("%s cannot be set together with agent: it comes from the agent definition", name)
			}
		}
		return err
	}
	if name, ok := c["agent"].(string); ok && !executor.IsExpression(name) && strings.TrimSpace(name) == "" {
		return errors.New("agent must name a stored agent")
	}
	_, hasInput := c["input"]
	_, hasPrompt := c["prompt"]
	if !hasInput && !hasPrompt {
		return errors.New("an agent node needs input or prompt")
	}
	return nil
}

func (e *Executor) validateInline(raw json.RawMessage, c map[string]any) error {
	if err := executor.CheckFields(raw, []executor.Field{
		{Name: "model", Kind: "string", Required: true},
		{Name: "system", Kind: "string"},
		{Name: "prompt", Kind: "any", Required: true},
		{Name: "tools", Kind: "array"},
		{Name: "database_connection", Kind: "string"},
		{Name: "max_iterations", Kind: "integer", Required: true},
		{Name: "max_tokens", Kind: "integer"},
		{Name: "temperature", Kind: "number"},
		{Name: "output_schema", Kind: "object"},
	}); err != nil {
		return err
	}
	var problems []error
	if err := literalInt(c, "max_iterations", 1, maxIterationsLimit); err != nil {
		problems = append(problems, err)
	}
	if err := literalInt(c, "max_tokens", 1, maxTokensLimit); err != nil {
		problems = append(problems, err)
	}
	if t, ok := c["temperature"].(float64); ok && (t < 0 || t > 1) {
		problems = append(problems, errors.New("temperature must be between 0 and 1"))
	}
	if tools, ok := c["tools"].([]any); ok {
		for _, entry := range tools {
			name, isString := entry.(string)
			if !isString || executor.IsExpression(name) {
				problems = append(problems, errors.New("tools must be a list of literal tool names"))
				break
			}
			if _, registered := e.tools.Get(name); !registered && !IsDynamicReference(name) {
				problems = append(problems, fmt.Errorf("unknown tool %q (available: %s)", name, toolList(e.tools)))
			}
		}
	}
	if schema, ok := c["output_schema"]; ok {
		if err := jsonschema.Check(schema); err != nil {
			problems = append(problems, fmt.Errorf("output_schema: %w", err))
		}
	}
	db, _ := c["database_connection"].(string)
	var names []string
	if entries, ok := c["tools"].([]any); ok {
		for _, entry := range entries {
			if name, ok := entry.(string); ok {
				names = append(names, name)
			}
		}
	}
	if err := validateDatabaseTools(names, db); err != nil {
		problems = append(problems, err)
	}
	if err := validateReferences(names); err != nil {
		problems = append(problems, err)
	}
	return errors.Join(problems...)
}

// literalInt requires key, when present, to be an integer within [lo, hi]
// written literally; an expression would defeat the purpose of a hard cap.
func literalInt(config map[string]any, key string, lo, hi int) error {
	value, present := config[key]
	if !present || value == nil {
		return nil
	}
	number, ok := value.(float64)
	if !ok {
		return fmt.Errorf("%s must be a literal integer, not an expression", key)
	}
	if number < float64(lo) || number > float64(hi) {
		return fmt.Errorf("%s must be between %d and %d", key, lo, hi)
	}
	return nil
}

// validateResolved checks an inline configuration after its expressions are
// resolved. Stored agents were validated when they were saved.
func (c *config) validateResolved(tools *Tools) error {
	if err := validateDatabaseTools(c.Tools, c.DatabaseConnection); err != nil {
		return err
	}
	return validateLimits(c.Model, c.Prompt, true, c.MaxIterations, c.MaxTokens, c.Temperature, c.Tools, c.OutputSchema, tools)
}

func validateLimits(model string, prompt json.RawMessage, requirePrompt bool, maxIterations, maxTokens int, temperature *float64, toolNames []string, outputSchema json.RawMessage, tools *Tools) error {
	switch {
	case model == "":
		return errors.New("model is required")
	case requirePrompt && (len(prompt) == 0 || string(prompt) == "null"):
		return errors.New("prompt is required")
	case maxIterations < 1 || maxIterations > maxIterationsLimit:
		return fmt.Errorf("max_iterations must be between 1 and %d", maxIterationsLimit)
	case maxTokens < 0 || maxTokens > maxTokensLimit:
		return fmt.Errorf("max_tokens must be between 1 and %d", maxTokensLimit)
	case temperature != nil && (*temperature < 0 || *temperature > 1):
		return errors.New("temperature must be between 0 and 1")
	}
	if err := validateReferences(toolNames); err != nil {
		return err
	}
	for _, name := range toolNames {
		if _, ok := tools.Get(name); !ok && !IsDynamicReference(name) {
			return fmt.Errorf("unknown tool %q", name)
		}
	}
	if len(outputSchema) > 0 && string(outputSchema) != "null" {
		var schema any
		if err := json.Unmarshal(outputSchema, &schema); err != nil {
			return fmt.Errorf("output_schema: %w", err)
		}
		if err := jsonschema.Check(schema); err != nil {
			return fmt.Errorf("output_schema: %w", err)
		}
	}
	return nil
}

// ValidateDefinition checks a stored agent before it is saved, with the same
// rules as an inline node plus the schemas. The agents service calls it.
func (e *Executor) ValidateDefinition(d Definition) error {
	if err := validateDatabaseTools(d.Tools, d.DatabaseConnection); err != nil {
		return err
	}
	// A definition has no prompt of its own: each node supplies input.
	if err := validateLimits(d.Model, nil, false, d.MaxIterations, d.MaxTokens, d.Temperature, d.Tools, d.OutputSchema, e.tools); err != nil {
		return err
	}
	if len(d.InputSchema) > 0 && string(d.InputSchema) != "null" {
		var schema any
		if err := json.Unmarshal(d.InputSchema, &schema); err != nil {
			return fmt.Errorf("input_schema: %w", err)
		}
		if err := jsonschema.Check(schema); err != nil {
			return fmt.Errorf("input_schema: %w", err)
		}
	}
	return nil
}

func hasTool(names []string, name string) bool { return slices.Contains(names, name) }
