package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kartikey/workflow_service/pkg/llm"
)

func serve(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func TestCompleteMapsConversationToolsAndUsage(t *testing.T) {
	var got map[string]any
	var key, path string
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		key, path = r.Header.Get("x-goog-api-key"), r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = io.WriteString(w, `{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[
			{"text":"let me check"},{"functionCall":{"name":"lookup","args":{"q":"x"}}}]}}],
			"usageMetadata":{"promptTokenCount":21,"candidatesTokenCount":6},"modelVersion":"gemini-test-001"}`)
	})
	temperature := 0.5
	res, err := New("g-key", server.URL, nil).Complete(context.Background(), llm.Request{
		Model: "models/gemini-test", System: "be brief", MaxTokens: 77, Temperature: &temperature,
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.Block{{Type: llm.BlockText, Text: "hi"}}},
			{Role: llm.RoleAssistant, Content: []llm.Block{{Type: llm.BlockToolUse, ToolUseID: "call_1", ToolName: "lookup", ToolInput: json.RawMessage(`{"a":1}`)}}},
			{Role: llm.RoleUser, Content: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "call_1", ToolContent: "42"}}},
		},
		Tools: []llm.Tool{{Name: "lookup", Description: "find", InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"q":{"type":"string","pattern":"x"}}}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if key != "g-key" || path != "/v1beta/models/gemini-test:generateContent" {
		t.Fatalf("key %q path %q", key, path)
	}
	if got["systemInstruction"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"] != "be brief" {
		t.Fatalf("system instruction = %v", got["systemInstruction"])
	}
	config := got["generationConfig"].(map[string]any)
	if config["maxOutputTokens"] != float64(77) || config["temperature"] != 0.5 {
		t.Fatalf("generation config = %v", config)
	}
	contents := got["contents"].([]any)
	roles := []string{contents[0].(map[string]any)["role"].(string), contents[1].(map[string]any)["role"].(string), contents[2].(map[string]any)["role"].(string)}
	if strings.Join(roles, ",") != "user,model,user" {
		t.Fatalf("roles = %v", roles)
	}
	response := contents[2].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	if response["name"] != "lookup" || response["response"].(map[string]any)["result"] != "42" {
		t.Fatalf("function response = %v", response)
	}
	schema := got["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)["parameters"].(map[string]any)
	if _, has := schema["additionalProperties"]; has || schema["properties"].(map[string]any)["q"].(map[string]any)["pattern"] != nil {
		t.Fatalf("unsupported schema keywords were sent: %v", schema)
	}
	if res.StopReason != llm.StopToolUse || res.Model != "gemini-test-001" || res.Usage.InputTokens != 21 || res.Usage.OutputTokens != 6 || res.Text() != "let me check" {
		t.Fatalf("response = %+v", res)
	}
	if uses := res.ToolUses(); len(uses) != 1 || uses[0].ToolUseID != "call_1" || uses[0].ToolName != "lookup" {
		t.Fatalf("tool uses = %+v", uses)
	}
}

func TestEmptyObjectSchemaIsOmittedAndOrphanResultsRejected(t *testing.T) {
	if got := sanitizeSchema(json.RawMessage(`{"type":"object","properties":{}}`)); got != nil {
		t.Fatalf("empty object schema = %s, want it omitted", got)
	}
	_, err := toWire(llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "ghost", ToolContent: "x"}}}}})
	if err == nil {
		t.Fatal("a tool result without a matching call was accepted")
	}
}

func TestFinishReasonsAndBlockedPrompt(t *testing.T) {
	for finish, want := range map[string]string{"STOP": llm.StopEndTurn, "MAX_TOKENS": llm.StopMaxTokens} {
		server := serve(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"candidates":[{"finishReason":"`+finish+`","content":{"parts":[{"text":"x"}]}}]}`)
		})
		res, err := New("k", server.URL, nil).Complete(context.Background(), llm.Request{Model: "m"})
		if err != nil || res.StopReason != want || res.Model != "m" {
			t.Errorf("finish %q -> %+v, %v", finish, res, err)
		}
	}
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"promptFeedback":{"blockReason":"SAFETY"}}`)
	})
	if _, err := New("k", server.URL, nil).Complete(context.Background(), llm.Request{Model: "m"}); err == nil || !strings.Contains(err.Error(), "no candidates") {
		t.Fatalf("a blocked prompt = %v", err)
	}
}

func TestErrorsAndModelList(t *testing.T) {
	for status, retryable := range map[int]bool{429: true, 503: true, 403: false, 400: false} {
		server := serve(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"code":1,"status":"RESOURCE_EXHAUSTED","message":"quota"}}`)
		})
		_, err := New("k", server.URL, nil).Complete(context.Background(), llm.Request{Model: "m"})
		var apiErr *llm.Error
		if !errors.As(err, &apiErr) || apiErr.Retryable != retryable || apiErr.Code != "RESOURCE_EXHAUSTED" || apiErr.Message != "quota" {
			t.Errorf("status %d -> %+v", status, err)
		}
	}
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"models":[
			{"name":"models/gemini-a","supportedGenerationMethods":["generateContent"]},
			{"name":"models/embed-a","supportedGenerationMethods":["embedContent"]},
			{"name":"models/gemini-b"}]}`)
	})
	models, err := New("k", server.URL, nil).ListModels(context.Background())
	if err != nil || len(models) != 2 || models[0] != "gemini-a" || models[1] != "gemini-b" {
		t.Fatalf("models = %v, %v (embedding-only models must be left out)", models, err)
	}
}
