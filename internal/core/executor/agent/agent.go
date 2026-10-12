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
	defs    Definitions
	dynamic DynamicTools
}

var _ executor.Executor = (*Executor)(nil)

// New builds the executor. A nil clients means no provider is configured; a
// nil defs means a node cannot refer to a stored agent by name (only inline
// configuration works). Tools come from WithDynamicTools.
func New(clients Clients, defs Definitions) *Executor {
	if clients == nil {
		clients = Unconfigured{}
	}
	return &Executor{clients: clients, defs: defs}
}

func (*Executor) Type() string { return "agent" }

func (e *Executor) Describe() executor.Description {
	return executor.Description{
		Title: "Agent", Category: "ai",
		Description: "Calls a language model in a loop capped by max_iterations. `tools` lists saved connectors the model may " +
			"search with three read-only tools: glob lists what a connector holds, grep searches it, read opens one item " +
			"(and mcp:<server> adds a saved MCP server's tools). " +
			"Run a stored agent with `agent: <name>` and `input`; optional connection and model override its defaults for this step only. " +
			"Or configure connection, model, prompt and limits inline. " +
			"With an output schema the reply must be JSON that matches it; a violation fails the node permanently. " +
			"429 and 5xx responses are retried.",
		Example: `- id: classify
  type: agent
  needs: [fetch_ticket]
  config:
    agent: ticket-classifier          # defined under /be/v1/agents
    # connection: client-two         # optional account override
    # model: another-model          # optional model override
    input: {text: '${nodes.fetch_ticket.output.body.description}'}

# or, inline:
- id: classify
  type: agent
  config:
    connection: acme-anthropic       # omit to use the platform provider
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

type config struct {
	// Agent mode: run a stored definition.
	Agent string          `json:"agent"`
	Input json.RawMessage `json:"input"`
	// A workflow can choose its own account and model without editing the agent.
	Connection *string `json:"connection"`
	Model      *string `json:"model"`

	// Inline mode: configure the call here.
	System        string          `json:"system"`
	Prompt        json.RawMessage `json:"prompt"`
	Tools         []string        `json:"tools"`
	MaxIterations int             `json:"max_iterations"`
	MaxTokens     int             `json:"max_tokens"`
	Temperature   *float64        `json:"temperature"`
	OutputSchema  json.RawMessage `json:"output_schema"`
}

// inlineOnly are the fields that, with `agent`, would conflict with the stored
// definition.
var inlineOnly = []string{"system", "tools", "max_iterations", "max_tokens", "temperature", "output_schema"}

// ValidateConfig checks the shape at publish time. A node either names a stored
// agent (`agent`, with optional `input`, `prompt`, `connection` and `model`)
// or configures the call inline. Inline max_iterations is mandatory and must be a literal: the run-level
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
		{Name: "connection", Kind: "string"},
		{Name: "model", Kind: "string"},
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
	if model, present := c["model"].(string); present && strings.TrimSpace(model) == "" {
		return errors.New("model must not be empty; omit it to use the agent's default")
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
		{Name: "connection", Kind: "string"},
		{Name: "system", Kind: "string"},
		{Name: "prompt", Kind: "any", Required: true},
		{Name: "tools", Kind: "array"},
		{Name: "max_iterations", Kind: "integer", Required: true},
		{Name: "max_tokens", Kind: "integer"},
		{Name: "temperature", Kind: "number"},
		{Name: "output_schema", Kind: "object"},
	}); err != nil {
		return err
	}
	var problems []error
	if model, ok := c["model"].(string); ok && strings.TrimSpace(model) == "" {
		problems = append(problems, errors.New("model is required"))
	}
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
				problems = append(problems, errors.New("tools must be a list of literal connector names"))
				break
			}
			if !IsToolReference(name) {
				problems = append(problems, fmt.Errorf("tool %q must name a saved connector, or mcp:<server>", name))
			}
		}
	}
	if schema, ok := c["output_schema"]; ok {
		if err := jsonschema.Check(schema); err != nil {
			problems = append(problems, fmt.Errorf("output_schema: %w", err))
		}
	}
	var names []string
	if entries, ok := c["tools"].([]any); ok {
		for _, entry := range entries {
			if name, ok := entry.(string); ok {
				names = append(names, name)
			}
		}
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
func (c *config) validateResolved() error {
	model := ""
	if c.Model != nil {
		model = *c.Model
	}
	return validateLimits(model, c.Prompt, true, c.MaxIterations, c.MaxTokens, c.Temperature, c.Tools, c.OutputSchema)
}

func validateLimits(model string, prompt json.RawMessage, requirePrompt bool, maxIterations, maxTokens int, temperature *float64, toolNames []string, outputSchema json.RawMessage) error {
	switch {
	case strings.TrimSpace(model) == "":
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
		if !IsToolReference(name) {
			return fmt.Errorf("tool %q must name a saved connector, or mcp:<server>", name)
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
	// A definition has no prompt of its own: each node supplies input.
	if err := validateLimits(d.Model, nil, false, d.MaxIterations, d.MaxTokens, d.Temperature, d.Tools, d.OutputSchema); err != nil {
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
