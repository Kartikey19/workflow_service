package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kartikey/workflow_service/pkg/llm"
)

func TestCompleteSendsRequestAndDecodesToolUse(t *testing.T) {
	var got map[string]any
	var headers http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"model":"claude-sonnet-5-5","stop_reason":"tool_use",
			"content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"tu_1","name":"lookup","input":{"q":"x"}}],
			"usage":{"input_tokens":11,"output_tokens":7}}`)
	}))
	defer server.Close()

	temperature := 0.0
	res, err := New("secret", server.URL+"/", nil).Complete(context.Background(), llm.Request{
		Model: "claude-sonnet-5-5", System: "be brief", MaxTokens: 100, Temperature: &temperature,
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.Block{{Type: llm.BlockText, Text: "hi"}}},
			{Role: llm.RoleAssistant, Content: []llm.Block{{Type: llm.BlockToolUse, ToolUseID: "tu_0", ToolName: "lookup"}}},
			{Role: llm.RoleUser, Content: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "tu_0", ToolContent: "42", IsError: true}}},
		},
		Tools: []llm.Tool{{Name: "lookup", Description: "find", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if headers.Get("x-api-key") != "secret" || headers.Get("anthropic-version") == "" {
		t.Fatalf("headers = %v", headers)
	}
	if got["model"] != "claude-sonnet-5-5" || got["system"] != "be brief" || got["max_tokens"] != float64(100) || got["temperature"] != float64(0) {
		t.Fatalf("request = %v", got)
	}
	messages := got["messages"].([]any)
	result := messages[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if result["type"] != "tool_result" || result["tool_use_id"] != "tu_0" || result["is_error"] != true {
		t.Fatalf("tool result block = %v", result)
	}
	// A tool_use echoed without input still carries an object, as the API requires.
	echoed := messages[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if string(mustJSON(t, echoed["input"])) != `{}` {
		t.Fatalf("tool_use input = %v", echoed["input"])
	}
	if res.StopReason != llm.StopToolUse || res.Usage.InputTokens != 11 || res.Usage.OutputTokens != 7 || res.Text() != "checking" {
		t.Fatalf("response = %+v", res)
	}
	uses := res.ToolUses()
	if len(uses) != 1 || uses[0].ToolName != "lookup" || string(uses[0].ToolInput) != `{"q":"x"}` {
		t.Fatalf("tool uses = %+v", uses)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCompleteClassifiesErrors(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		header     string
		wantRetry  bool
		wantAfter  string
		wantCode   string
		wantStatus int
	}{
		{"rate limit honours Retry-After", 429, "7", true, "7s", "rate_limit_error", 429},
		{"overloaded", 529, "", true, "0s", "rate_limit_error", 529},
		{"server error", 500, "", true, "0s", "rate_limit_error", 500},
		{"bad request is permanent", 400, "", false, "0s", "rate_limit_error", 400},
		{"unauthorized is permanent", 401, "", false, "0s", "rate_limit_error", 401},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.header != "" {
					w.Header().Set("Retry-After", tt.header)
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
			}))
			defer server.Close()
			_, err := New("k", server.URL, nil).Complete(context.Background(), llm.Request{Model: "m", MaxTokens: 1})
			var apiErr *llm.Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v, want *llm.Error", err)
			}
			if apiErr.Retryable != tt.wantRetry || apiErr.RetryAfter.String() != tt.wantAfter || apiErr.Status != tt.wantStatus || apiErr.Message != "slow down" {
				t.Fatalf("error = %+v", apiErr)
			}
		})
	}
}

func TestCompleteHonoursContextAndTransportFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := server.URL
	server.Close() // nothing listens any more

	_, err := New("k", url, nil).Complete(context.Background(), llm.Request{Model: "m", MaxTokens: 1})
	var apiErr *llm.Error
	if !errors.As(err, &apiErr) || !apiErr.Retryable || apiErr.Code != "connection_error" {
		t.Fatalf("closed server error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New("k", url, nil).Complete(ctx, llm.Request{Model: "m", MaxTokens: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context error = %v, want context.Canceled", err)
	}
}
