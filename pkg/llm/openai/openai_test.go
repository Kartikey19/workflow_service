package openai

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

func serve(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func TestCompleteMapsConversationAndToolCalls(t *testing.T) {
	var got map[string]any
	var auth, path string
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = io.WriteString(w, `{"model":"gpt-x","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,
			"tool_calls":[{"id":"call_9","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]}}],
			"usage":{"prompt_tokens":12,"completion_tokens":4}}`)
	})
	temperature := 0.2
	res, err := New("sk-test", server.URL+"/v1", nil).Complete(context.Background(), llm.Request{
		Model: "gpt-x", System: "be brief", MaxTokens: 50, Temperature: &temperature,
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.Block{{Type: llm.BlockText, Text: "hi"}}},
			{Role: llm.RoleAssistant, Content: []llm.Block{{Type: llm.BlockText, Text: "thinking"}, {Type: llm.BlockToolUse, ToolUseID: "call_1", ToolName: "lookup", ToolInput: json.RawMessage(`{"a":1}`)}}},
			{Role: llm.RoleUser, Content: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "call_1", ToolContent: "boom", IsError: true}}},
		},
		Tools: []llm.Tool{{Name: "lookup", Description: "find", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer sk-test" || path != "/v1/chat/completions" {
		t.Fatalf("auth %q path %q", auth, path)
	}
	if got["max_completion_tokens"] != float64(50) || got["max_tokens"] != nil || got["temperature"] != 0.2 {
		t.Fatalf("OpenAI proper must send max_completion_tokens: %v", got)
	}
	messages := got["messages"].([]any)
	roles := []string{}
	for _, m := range messages {
		roles = append(roles, m.(map[string]any)["role"].(string))
	}
	if len(roles) != 4 || roles[0] != "system" || roles[1] != "user" || roles[2] != "assistant" || roles[3] != "tool" {
		t.Fatalf("roles = %v", roles)
	}
	assistant := messages[2].(map[string]any)
	call := assistant["tool_calls"].([]any)[0].(map[string]any)
	if assistant["content"] != "thinking" || call["id"] != "call_1" || call["function"].(map[string]any)["arguments"] != `{"a":1}` {
		t.Fatalf("assistant message = %v", assistant)
	}
	tool := messages[3].(map[string]any)
	if tool["tool_call_id"] != "call_1" || tool["content"] != "Error: boom" {
		t.Fatalf("tool message = %v", tool)
	}
	if res.StopReason != llm.StopToolUse || res.Usage.InputTokens != 12 || res.Usage.OutputTokens != 4 {
		t.Fatalf("response = %+v", res)
	}
	uses := res.ToolUses()
	if len(uses) != 1 || uses[0].ToolUseID != "call_9" || string(uses[0].ToolInput) != `{"q":"x"}` {
		t.Fatalf("tool uses = %+v", uses)
	}
}

func TestCompatibleSendsMaxTokensAndNoAuthWithoutKey(t *testing.T) {
	var got map[string]any
	var hasAuth bool
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, hasAuth = r.Header["Authorization"]
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"length","message":{"content":"cut"}}],"usage":{}}`)
	})
	res, err := NewCompatible("", server.URL, nil).Complete(context.Background(), llm.Request{Model: "llama", MaxTokens: 9,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.Block{{Type: llm.BlockText, Text: "x"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if got["max_tokens"] != float64(9) || got["max_completion_tokens"] != nil || hasAuth {
		t.Fatalf("compatible request = %v (auth header present: %v)", got, hasAuth)
	}
	if res.StopReason != llm.StopMaxTokens || res.Text() != "cut" {
		t.Fatalf("response = %+v", res)
	}
}

func TestFinishReasonsAndBadToolArguments(t *testing.T) {
	for finish, want := range map[string]string{"stop": llm.StopEndTurn, "length": llm.StopMaxTokens, "tool_calls": llm.StopToolUse, "content_filter": "content_filter"} {
		server := serve(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"`+finish+`","message":{"content":"x"}}],"usage":{}}`)
		})
		res, err := New("k", server.URL, nil).Complete(context.Background(), llm.Request{Model: "m"})
		if err != nil || res.StopReason != want {
			t.Errorf("finish %q -> %q, %v; want %q", finish, res.StopReason, err, want)
		}
	}
	// Servers that send "stop" together with tool calls still mean tool use, and
	// arguments that are not JSON become an empty object instead of corrupting the request.
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{oops"}}]}}],"usage":{}}`)
	})
	res, err := NewCompatible("", server.URL, nil).Complete(context.Background(), llm.Request{Model: "m"})
	if err != nil || res.StopReason != llm.StopToolUse || string(res.ToolUses()[0].ToolInput) != `{}` {
		t.Fatalf("response = %+v, %v", res, err)
	}
}

func TestErrorsAndModelList(t *testing.T) {
	for _, tt := range []struct {
		status    int
		retryable bool
	}{{429, true}, {500, true}, {401, false}, {400, false}} {
		server := serve(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(tt.status)
			_, _ = io.WriteString(w, `{"error":{"message":"nope","type":"invalid_request_error"}}`)
		})
		_, err := New("k", server.URL, nil).Complete(context.Background(), llm.Request{Model: "m"})
		var apiErr *llm.Error
		if !errors.As(err, &apiErr) || apiErr.Retryable != tt.retryable || apiErr.Status != tt.status || apiErr.Message != "nope" || apiErr.RetryAfter.Seconds() != 3 {
			t.Errorf("status %d -> %+v", tt.status, err)
		}
	}
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"gpt-a"},{"id":"gpt-b"}]}`)
	})
	models, err := New("k", server.URL+"/v1/", nil).ListModels(context.Background())
	if err != nil || len(models) != 2 || models[1] != "gpt-b" {
		t.Fatalf("models = %v, %v", models, err)
	}
	denied := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key"}}`)
	})
	if _, err := New("k", denied.URL, nil).ListModels(context.Background()); err == nil {
		t.Fatal("a rejected key listed models")
	}
}
