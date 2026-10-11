package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/pkg/jsonschema"
	"github.com/kartikey/workflow_service/pkg/llm"
	"github.com/kartikey/workflow_service/pkg/obs"
)

// maxToolResultBytes bounds what a tool can push into the conversation.
const maxToolResultBytes = 32 << 10

// usage accumulates what the loop has spent so far. It is reported on every
// outcome, success or failure, because failed attempts cost money too.
type usage struct {
	agent      string
	revision   int
	model      string
	iterations int
	tokensIn   int
	tokensOut  int
	cost       float64
	costKnown  bool
	toolCalls  int
	stop       string
}

func (u *usage) metrics() map[string]any {
	m := map[string]any{
		"iterations": u.iterations, "tokens_in": u.tokensIn, "tokens_out": u.tokensOut,
		"cost_usd": u.cost, "cost_known": u.costKnown, "model": u.model,
		"stop_reason": u.stop, "tool_calls": u.toolCalls,
	}
	if u.agent != "" {
		m["agent"], m["agent_revision"] = u.agent, u.revision
	}
	return m
}

func (u *usage) add(resolved Resolved, response llm.Response) {
	u.tokensIn += response.Usage.InputTokens
	u.tokensOut += response.Usage.OutputTokens
	if response.Model != "" {
		u.model = response.Model
	}
	u.stop = response.StopReason
	obs.AgentTokens.WithLabelValues(resolved.Provider, "input").Add(float64(response.Usage.InputTokens))
	obs.AgentTokens.WithLabelValues(resolved.Provider, "output").Add(float64(response.Usage.OutputTokens))
	if cost, ok := resolved.Pricing.Cost(u.model, response.Usage); ok {
		u.cost += cost
		u.costKnown = true
		obs.AgentCost.WithLabelValues(resolved.Provider).Add(cost)
	}
}

// plan is everything one agent call needs, whether it came from inline
// configuration or from a stored definition.
type plan struct {
	ref           ProviderRef
	system        string
	prompt        string
	tools         []string
	maxIterations int
	maxTokens     int
	temperature   *float64
	outputSchema  json.RawMessage
	agent         string
	revision      int
}

// plan resolves the node's configuration into a plan. A stored agent is loaded
// and its input checked against the agent's input schema; failures here are
// permanent, because the same configuration fails the same way next time.
func (e *Executor) plan(ctx context.Context, req executor.Request, c config) (plan, error) {
	if c.Agent == "" {
		if err := c.validateResolved(); err != nil {
			return plan{}, executor.Permanent("bad_config", err)
		}
		return plan{
			ref: ProviderRef{Model: c.Model}, system: c.System, prompt: promptText(c.Prompt), tools: c.Tools,
			maxIterations: c.MaxIterations, maxTokens: c.MaxTokens, temperature: c.Temperature, outputSchema: c.OutputSchema,
		}, nil
	}
	if e.defs == nil {
		return plan{}, executor.Permanent("bad_config", errors.New("stored agents are not available on this worker"))
	}
	def, err := e.defs.Definition(ctx, req.TenantID, c.Agent)
	if errors.Is(err, ErrAgentNotFound) {
		return plan{}, executor.Permanent("E_AGENT_NOT_FOUND", fmt.Errorf("agent %q is not defined; create it under /v1/agents", c.Agent))
	}
	if err != nil {
		return plan{}, executor.Retryable("agent_lookup", err)
	}
	var input any
	if len(c.Input) > 0 && string(c.Input) != "null" {
		if err := json.Unmarshal(c.Input, &input); err != nil {
			return plan{}, executor.Permanent("bad_config", fmt.Errorf("input: %w", err))
		}
	}
	if len(def.InputSchema) > 0 && string(def.InputSchema) != "null" {
		var schema any
		if err := json.Unmarshal(def.InputSchema, &schema); err != nil {
			return plan{}, executor.Permanent("bad_config", fmt.Errorf("agent %q has an invalid input schema", def.Name))
		}
		if violations := jsonschema.Validate(schema, input); len(violations) > 0 {
			parts := make([]string, 0, len(violations))
			for _, v := range violations {
				parts = append(parts, v.Field+" "+v.Message)
			}
			return plan{}, executor.Permanent("invalid_input", fmt.Errorf("input does not match the agent's input schema: %s", strings.Join(parts, "; ")))
		}
	}
	return plan{
		ref: ProviderRef{Connection: def.Connection, Model: def.Model}, system: def.System,
		prompt: agentPrompt(c.Prompt, input), tools: def.Tools,
		maxIterations: def.MaxIterations, maxTokens: def.MaxTokens, temperature: def.Temperature,
		outputSchema: def.OutputSchema, agent: def.Name, revision: def.Revision,
	}, nil
}

// agentPrompt builds the user message of a stored agent: the node's own prompt
// (if any) followed by the input as JSON.
func agentPrompt(prompt json.RawMessage, input any) string {
	var parts []string
	if len(prompt) > 0 && string(prompt) != "null" {
		parts = append(parts, promptText(prompt))
	}
	if input != nil {
		encoded, _ := json.MarshalIndent(input, "", "  ")
		parts = append(parts, "Input:\n"+string(encoded))
	}
	return strings.Join(parts, "\n\n")
}

// Execute runs the model, executing requested tools until it answers or
// max_iterations model calls have been made.
func (e *Executor) Execute(ctx context.Context, req executor.Request) (executor.Result, error) {
	var c config
	if err := executor.DecodeConfig(req.Config, &c); err != nil {
		return executor.Result{}, err
	}
	p, err := e.plan(ctx, req, c)
	if err != nil {
		return executor.Result{}, err
	}
	registry, names, closeTools, err := e.resolveTools(ctx, req.TenantID, p.tools)
	if err != nil {
		var permanent *executor.PermanentError
		var retryable *executor.RetryableError
		if errors.As(err, &permanent) || errors.As(err, &retryable) {
			return executor.Result{}, err
		}
		return executor.Result{}, executor.Retryable("mcp_tools", err)
	}
	defer closeTools()
	p.tools = names
	resolved, err := e.clients.Client(ctx, req.TenantID, p.ref)
	switch {
	case errors.Is(err, ErrNotConfigured):
		return executor.Result{}, executor.Permanent("llm_not_configured", err)
	case errors.Is(err, ErrConnectionNotFound):
		return executor.Result{}, executor.Permanent("E_PROVIDER_NOT_FOUND", fmt.Errorf("provider connection %q does not exist", p.ref.Connection))
	case err != nil:
		return executor.Result{}, executor.Retryable("provider_lookup", err)
	}

	maxTokens := p.maxTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}
	var schema any
	structured := len(p.outputSchema) > 0 && string(p.outputSchema) != "null"
	if structured {
		_ = json.Unmarshal(p.outputSchema, &schema)
	}
	llmRequest := llm.Request{
		Model: p.ref.Model, System: systemPrompt(p.system, p.outputSchema, structured),
		MaxTokens: maxTokens, Temperature: p.temperature,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.Block{{Type: llm.BlockText, Text: p.prompt}}}},
	}
	for _, name := range p.tools {
		tool, _ := registry.Get(name)
		llmRequest.Tools = append(llmRequest.Tools, llm.Tool{Name: tool.Name(), Description: tool.Description(), InputSchema: tool.InputSchema()})
	}

	spent := &usage{model: p.ref.Model, agent: p.agent, revision: p.revision}
	call := Call{TenantID: req.TenantID, RunID: req.RunID, NodeID: req.NodeID, RedactSecrets: req.RedactSecrets}
	for {
		if spent.iterations >= p.maxIterations {
			return executor.Result{Metrics: spent.metrics()}, executor.Permanent("max_iterations_exceeded",
				fmt.Errorf("the model still wanted tools after %d iterations", p.maxIterations))
		}
		spent.iterations++
		response, err := resolved.Client.Complete(ctx, llmRequest)
		if err != nil {
			return executor.Result{Metrics: spent.metrics()}, classify(ctx, err)
		}
		spent.add(resolved, response)

		uses := response.ToolUses()
		if len(uses) == 0 || response.StopReason != llm.StopToolUse {
			return finish(response, structured, schema, spent)
		}
		llmRequest.Messages = append(llmRequest.Messages, llm.Message{Role: llm.RoleAssistant, Content: response.Content})
		results := make([]llm.Block, 0, len(uses))
		for _, use := range uses {
			spent.toolCalls++
			results = append(results, runTool(ctx, registry, call, p.tools, use))
		}
		llmRequest.Messages = append(llmRequest.Messages, llm.Message{Role: llm.RoleUser, Content: results})
	}
}

// runTool executes one requested tool. Anything the model got wrong, or any
// tool failure, goes back to the model as an error result so it can recover.
func runTool(ctx context.Context, registry *Tools, call Call, allowed []string, use llm.Block) llm.Block {
	result := llm.Block{Type: llm.BlockToolResult, ToolUseID: use.ToolUseID}
	tool, ok := registry.Get(use.ToolName)
	if !ok || !hasTool(allowed, use.ToolName) {
		result.IsError = true
		result.ToolContent = fmt.Sprintf("tool %q is not available", use.ToolName)
		return result
	}
	call.ToolCallID = use.ToolUseID
	output, err := tool.Call(ctx, call, use.ToolInput)
	text := string(output)
	if err != nil {
		result.IsError = true
		text = err.Error()
	}
	if len(text) > maxToolResultBytes {
		text = text[:maxToolResultBytes] + "…[truncated]"
	}
	result.ToolContent = text
	return result
}

// finish turns the model's final reply into node output.
func finish(response llm.Response, structured bool, schema any, spent *usage) (executor.Result, error) {
	text := response.Text()
	if !structured {
		output, err := executor.JSON(map[string]string{"text": text})
		return executor.Result{Output: output, Metrics: spent.metrics()}, err
	}
	if response.StopReason == llm.StopMaxTokens {
		return executor.Result{Metrics: spent.metrics()}, executor.Permanent("invalid_output",
			errors.New("the reply was cut off at max_tokens before the JSON was complete"))
	}
	value, err := extractJSON(text)
	if err != nil {
		return executor.Result{Metrics: spent.metrics()}, executor.Permanent("invalid_output", err)
	}
	var decoded any
	if err := json.Unmarshal(value, &decoded); err != nil {
		return executor.Result{Metrics: spent.metrics()}, executor.Permanent("invalid_output", err)
	}
	if violations := jsonschema.Validate(schema, decoded); len(violations) > 0 {
		parts := make([]string, 0, len(violations))
		for _, v := range violations {
			parts = append(parts, v.Field+" "+v.Message)
		}
		// Retrying a schema-violating reply usually reproduces it; fail once.
		return executor.Result{Metrics: spent.metrics()}, executor.Permanent("invalid_output",
			fmt.Errorf("reply does not match output_schema: %s", strings.Join(parts, "; ")))
	}
	return executor.Result{Output: value, Metrics: spent.metrics()}, nil
}

func systemPrompt(system string, schema json.RawMessage, structured bool) string {
	if !structured {
		return system
	}
	instruction := "Reply with one JSON value that matches this JSON Schema, and nothing else:\n" + string(schema)
	if system == "" {
		return instruction
	}
	return system + "\n\n" + instruction
}

// promptText sends a string prompt as is and any other JSON value as JSON.
func promptText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return string(raw)
}

// extractJSON finds the JSON value in a reply that may wrap it in a code fence
// or surround it with prose.
func extractJSON(text string) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "```") {
		trimmed = strings.TrimPrefix(trimmed, "```")
		if newline := strings.IndexByte(trimmed, '\n'); newline >= 0 {
			trimmed = trimmed[newline+1:]
		}
		trimmed = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(trimmed), "```"))
	}
	if json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed), nil
	}
	for _, pair := range [][2]string{{"{", "}"}, {"[", "]"}} {
		start, end := strings.Index(trimmed, pair[0]), strings.LastIndex(trimmed, pair[1])
		if start >= 0 && end > start && json.Valid([]byte(trimmed[start:end+1])) {
			return json.RawMessage(trimmed[start : end+1]), nil
		}
	}
	return nil, errors.New("the reply does not contain valid JSON")
}

// classify maps a provider failure onto the retry model: rate limits and
// outages are retried (honouring Retry-After), everything else fails once.
func classify(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err() // DeadlineExceeded is a retryable timeout; Canceled is permanent
	}
	var apiErr *llm.Error
	if !errors.As(err, &apiErr) {
		return executor.Retryable("connection_error", err)
	}
	switch {
	case apiErr.Status == http.StatusTooManyRequests:
		return &executor.RetryableError{Code: "rate_limited", Err: apiErr, RetryAfter: apiErr.RetryAfter}
	case apiErr.Retryable && apiErr.Status >= 500:
		return &executor.RetryableError{Code: "upstream_5xx", Err: apiErr, RetryAfter: apiErr.RetryAfter}
	case apiErr.Retryable:
		code := apiErr.Code
		if code == "" {
			code = "upstream_error"
		}
		return &executor.RetryableError{Code: code, Err: apiErr, RetryAfter: apiErr.RetryAfter}
	default:
		return executor.Permanent("upstream_4xx", apiErr)
	}
}
