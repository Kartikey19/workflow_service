package agent

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/pkg/llm"
)

// scripted answers each Complete call from a list and records the requests.
type scripted struct {
	mu       sync.Mutex
	replies  []any // llm.Response or error
	requests []llm.Request
}

func (s *scripted) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)
	index := len(s.requests) - 1
	if index >= len(s.replies) {
		index = len(s.replies) - 1
	}
	switch reply := s.replies[index].(type) {
	case error:
		return llm.Response{}, reply
	default:
		return reply.(llm.Response), nil
	}
}

func text(t string, in, out int) llm.Response {
	return llm.Response{
		Model: "claude-sonnet-5-5", StopReason: llm.StopEndTurn, Usage: llm.Usage{InputTokens: in, OutputTokens: out},
		Content: []llm.Block{{Type: llm.BlockText, Text: t}},
	}
}

func toolUse(id, name, input string) llm.Response {
	return llm.Response{
		Model: "claude-sonnet-5-5", StopReason: llm.StopToolUse, Usage: llm.Usage{InputTokens: 100, OutputTokens: 10},
		Content: []llm.Block{{Type: llm.BlockToolUse, ToolUseID: id, ToolName: name, ToolInput: json.RawMessage(input)}},
	}
}

type echoTool struct {
	calls []string
	err   error
}

func (*echoTool) Name() string                 { return "echo" }
func (*echoTool) Description() string          { return "echoes input" }
func (*echoTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (e *echoTool) Call(_ context.Context, _ Call, in json.RawMessage) (json.RawMessage, error) {
	e.calls = append(e.calls, string(in))
	if e.err != nil {
		return nil, e.err
	}
	return json.RawMessage(`{"echoed":true}`), nil
}

func newExecutor(t *testing.T, client llm.Client, tool Tool) *Executor {
	t.Helper()
	tools, err := NewTools(tool, CurrentTime{})
	if err != nil {
		t.Fatal(err)
	}
	pricing := llm.Pricing{"claude-sonnet": {InputPerMTok: 3, OutputPerMTok: 15}}
	return New(NewStatic("anthropic", client, pricing), tools, nil)
}

func run(t *testing.T, e *Executor, config string) (executor.Result, error) {
	t.Helper()
	return e.Execute(context.Background(), executor.Request{
		TenantID: uuid.New(), RunID: uuid.New(), NodeID: "n", Config: json.RawMessage(config),
	})
}

func TestPlainReplyAndCost(t *testing.T) {
	client := &scripted{replies: []any{text("hello", 1000, 500)}}
	result, err := run(t, newExecutor(t, client, &echoTool{}), `{"model":"claude-sonnet-5-5","prompt":"hi","max_iterations":3,"system":"s"}`)
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Output) != `{"text":"hello"}` {
		t.Fatalf("output = %s", result.Output)
	}
	m := result.Metrics
	wantCost := (1000*3.0 + 500*15.0) / 1e6
	if m["iterations"] != 1 || m["tokens_in"] != 1000 || m["tokens_out"] != 500 || m["stop_reason"] != "end_turn" ||
		m["model"] != "claude-sonnet-5-5" || math.Abs(m["cost_usd"].(float64)-wantCost) > 1e-12 || m["cost_known"] != true {
		t.Fatalf("metrics = %v", m)
	}
	if got := client.requests[0]; got.System != "s" || got.MaxTokens != defaultMaxTokens || got.Messages[0].Content[0].Text != "hi" {
		t.Fatalf("request = %+v", got)
	}
}

func TestToolLoopFeedsResultsBack(t *testing.T) {
	tool := &echoTool{}
	client := &scripted{replies: []any{toolUse("t1", "echo", `{"q":1}`), text("done", 5, 5)}}
	result, err := run(t, newExecutor(t, client, tool), `{"model":"claude-sonnet-5-5","prompt":{"a":1},"tools":["echo"],"max_iterations":3}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(tool.calls) != 1 || tool.calls[0] != `{"q":1}` {
		t.Fatalf("tool calls = %v", tool.calls)
	}
	if result.Metrics["iterations"] != 2 || result.Metrics["tool_calls"] != 1 {
		t.Fatalf("metrics = %v", result.Metrics)
	}
	second := client.requests[1]
	if len(second.Messages) != 3 || second.Messages[1].Role != llm.RoleAssistant {
		t.Fatalf("second request messages = %+v", second.Messages)
	}
	reply := second.Messages[2].Content[0]
	if reply.Type != llm.BlockToolResult || reply.ToolUseID != "t1" || reply.ToolContent != `{"echoed":true}` || reply.IsError {
		t.Fatalf("tool result = %+v", reply)
	}
	if len(second.Tools) != 1 || second.Tools[0].Name != "echo" {
		t.Fatalf("tools offered = %+v", second.Tools)
	}
	// An object prompt is sent as JSON text.
	if client.requests[0].Messages[0].Content[0].Text != `{"a":1}` {
		t.Fatalf("prompt = %q", client.requests[0].Messages[0].Content[0].Text)
	}
}

func TestMaxIterationsIsEnforcedAndReported(t *testing.T) {
	tool := &echoTool{}
	client := &scripted{replies: []any{toolUse("t", "echo", `{}`)}} // always wants another tool call
	result, err := run(t, newExecutor(t, client, tool), `{"model":"claude-sonnet-5-5","prompt":"p","tools":["echo"],"max_iterations":2}`)
	var permanent *executor.PermanentError
	if !errors.As(err, &permanent) || permanent.Code != "max_iterations_exceeded" {
		t.Fatalf("err = %v, want max_iterations_exceeded", err)
	}
	if result.Metrics["iterations"] != 2 || len(client.requests) != 2 {
		t.Fatalf("iterations = %v with %d model calls, want exactly 2", result.Metrics["iterations"], len(client.requests))
	}
	if result.Metrics["tokens_in"] != 200 {
		t.Fatalf("failed attempts must still report spend: %v", result.Metrics)
	}
}

func TestModelMistakesGoBackToTheModel(t *testing.T) {
	tool := &echoTool{err: errors.New("boom")}
	client := &scripted{replies: []any{
		toolUse("a", "missing", `{}`),      // unknown tool
		toolUse("b", "current_time", `{}`), // registered, but not enabled for this node
		toolUse("c", "echo", `{}`),         // enabled, but fails
		text("recovered", 1, 1),
	}}
	result, err := run(t, newExecutor(t, client, tool), `{"model":"claude-sonnet-5-5","prompt":"p","tools":["echo"],"max_iterations":5}`)
	if err != nil {
		t.Fatalf("tool failures must not fail the node: %v", err)
	}
	if string(result.Output) != `{"text":"recovered"}` {
		t.Fatalf("output = %s", result.Output)
	}
	last := client.requests[3].Messages
	for i, want := range []string{`tool "missing" is not available`, `tool "current_time" is not available`, "boom"} {
		block := last[2+2*i].Content[0]
		if !block.IsError || block.ToolContent != want {
			t.Fatalf("tool result %d = %+v, want error %q", i, block, want)
		}
	}
}

func TestStructuredOutputIsValidated(t *testing.T) {
	const config = `{"model":"claude-sonnet-5-5","prompt":"p","max_iterations":1,
		"output_schema":{"type":"object","required":["severity"],"properties":{"severity":{"type":"string","enum":["P0","P1"]}}}}`
	for name, tt := range map[string]struct {
		reply    llm.Response
		wantErr  string
		wantJSON string
	}{
		"plain JSON":       {reply: text(`{"severity":"P0"}`, 1, 1), wantJSON: `{"severity":"P0"}`},
		"fenced JSON":      {reply: text("```json\n{\"severity\":\"P1\"}\n```", 1, 1), wantJSON: `{"severity":"P1"}`},
		"JSON in prose":    {reply: text(`Sure! {"severity":"P0"} Hope it helps.`, 1, 1), wantJSON: `{"severity":"P0"}`},
		"schema violation": {reply: text(`{"severity":"P9"}`, 1, 1), wantErr: "does not match output_schema"},
		"missing field":    {reply: text(`{}`, 1, 1), wantErr: "does not match output_schema"},
		"not JSON":         {reply: text(`no idea`, 1, 1), wantErr: "valid JSON"},
		"cut off":          {reply: llm.Response{StopReason: llm.StopMaxTokens, Content: []llm.Block{{Type: llm.BlockText, Text: `{"sev`}}}, wantErr: "cut off"},
	} {
		t.Run(name, func(t *testing.T) {
			client := &scripted{replies: []any{tt.reply}}
			result, err := run(t, newExecutor(t, client, &echoTool{}), config)
			if tt.wantErr == "" {
				if err != nil || string(result.Output) != tt.wantJSON {
					t.Fatalf("result = %s, %v", result.Output, err)
				}
				return
			}
			var permanent *executor.PermanentError
			if !errors.As(err, &permanent) || permanent.Code != "invalid_output" || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want permanent invalid_output containing %q", err, tt.wantErr)
			}
		})
	}
	// The schema is shown to the model.
	client := &scripted{replies: []any{text(`{"severity":"P0"}`, 1, 1)}}
	if _, err := run(t, newExecutor(t, client, &echoTool{}), config); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(client.requests[0].System, `"required":["severity"]`) {
		t.Fatalf("system prompt does not carry the schema: %q", client.requests[0].System)
	}
}

func TestProviderErrorsFollowTheRetryModel(t *testing.T) {
	config := `{"model":"claude-sonnet-5-5","prompt":"p","max_iterations":1}`
	tests := []struct {
		name      string
		err       error
		retryable bool
		code      string
		after     time.Duration
	}{
		{"rate limit", &llm.Error{Status: 429, Code: "rate_limit_error", Retryable: true, RetryAfter: 9 * time.Second}, true, "rate_limited", 9 * time.Second},
		{"overloaded", &llm.Error{Status: 529, Retryable: true}, true, "upstream_5xx", 0},
		{"bad request", &llm.Error{Status: 400, Message: "bad"}, false, "upstream_4xx", 0},
		{"network", errors.New("dial failed"), true, "connection_error", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := run(t, newExecutor(t, &scripted{replies: []any{tt.err}}, &echoTool{}), config)
			var retryable *executor.RetryableError
			var permanent *executor.PermanentError
			switch {
			case tt.retryable && errors.As(err, &retryable):
				if retryable.Code != tt.code || retryable.RetryAfter != tt.after {
					t.Fatalf("retryable = %+v", retryable)
				}
			case !tt.retryable && errors.As(err, &permanent):
				if permanent.Code != tt.code {
					t.Fatalf("permanent = %+v", permanent)
				}
			default:
				t.Fatalf("err = %#v", err)
			}
		})
	}
}

func TestNotConfiguredAndBadResolvedConfig(t *testing.T) {
	_, err := run(t, New(nil, nil, nil), `{"model":"m","prompt":"p","max_iterations":1}`)
	var permanent *executor.PermanentError
	if !errors.As(err, &permanent) || permanent.Code != "llm_not_configured" {
		t.Fatalf("err = %v", err)
	}
	for _, config := range []string{
		`{"prompt":"p","max_iterations":1}`,
		`{"model":"m","max_iterations":1}`,
		`{"model":"m","prompt":"p"}`,
		`{"model":"m","prompt":"p","max_iterations":99}`,
		`{"model":"m","prompt":"p","max_iterations":1,"tools":["nope"]}`,
	} {
		_, err := run(t, newExecutor(t, &scripted{replies: []any{text("x", 1, 1)}}, &echoTool{}), config)
		if !errors.As(err, &permanent) || permanent.Code != "bad_config" {
			t.Fatalf("config %s: err = %v, want bad_config", config, err)
		}
	}
}

func TestValidateConfigAtPublishTime(t *testing.T) {
	e := newExecutor(t, &scripted{replies: []any{text("x", 1, 1)}}, &echoTool{})
	good := `{"model":"${input.model}","prompt":"${input.q}","max_iterations":6,"tools":["echo"],"temperature":0.2,
		"output_schema":{"type":"object","properties":{"a":{"type":"string"}}}}`
	if err := e.ValidateConfig(json.RawMessage(good)); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for name, tt := range map[string]struct{ config, want string }{
		"max_iterations is required":      {`{"model":"m","prompt":"p"}`, "max_iterations is required"},
		"max_iterations is literal":       {`{"model":"m","prompt":"p","max_iterations":"${input.n}"}`, "literal integer"},
		"max_iterations has an upper cap": {`{"model":"m","prompt":"p","max_iterations":51}`, "between 1 and 50"},
		"zero iterations":                 {`{"model":"m","prompt":"p","max_iterations":0}`, "between 1 and 50"},
		"model is required":               {`{"prompt":"p","max_iterations":1}`, "model is required"},
		"prompt is required":              {`{"model":"m","max_iterations":1}`, "prompt is required"},
		"unknown tool":                    {`{"model":"m","prompt":"p","max_iterations":1,"tools":["search_kb"]}`, `unknown tool "search_kb"`},
		"tools are literal":               {`{"model":"m","prompt":"p","max_iterations":1,"tools":["${input.t}"]}`, "literal tool names"},
		"temperature range":               {`{"model":"m","prompt":"p","max_iterations":1,"temperature":1.5}`, "between 0 and 1"},
		"unsupported schema keyword":      {`{"model":"m","prompt":"p","max_iterations":1,"output_schema":{"oneOf":[]}}`, "output_schema"},
		"unknown field":                   {`{"model":"m","prompt":"p","max_iterations":1,"top_k":3}`, `unknown field "top_k"`},
	} {
		t.Run(name, func(t *testing.T) {
			err := e.ValidateConfig(json.RawMessage(tt.config))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestToolsRegistry(t *testing.T) {
	if _, err := NewTools(&echoTool{}, &echoTool{}); err == nil {
		t.Fatal("duplicate tool names accepted")
	}
	tools, _ := NewTools(CurrentTime{Now: func() time.Time { return time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC) }})
	tool, ok := tools.Get("current_time")
	if !ok {
		t.Fatal("current_time missing")
	}
	out, err := tool.Call(context.Background(), Call{}, nil)
	if err != nil || string(out) != `{"now":"2026-10-11T09:00:00Z"}` {
		t.Fatalf("current_time = %s, %v", out, err)
	}
	if got := tools.Names(); len(got) != 1 || got[0] != "current_time" {
		t.Fatalf("names = %v", got)
	}
}
