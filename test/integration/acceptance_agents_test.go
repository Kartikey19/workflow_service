//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func agentBody(name, connection string) map[string]any {
	body := map[string]any{
		"name": name, "model": "fake-model", "system_prompt": "Be helpful.", "max_iterations": 3,
		"output_schema": map[string]any{"type": "object", "required": []string{"summary"}, "properties": map[string]any{"summary": map[string]any{"type": "string"}}},
	}
	if connection != "" {
		body["connection"] = connection
	}
	return body
}

// Definitions are validated when saved, edits bump the revision, and a
// connection in use cannot be deleted from under an agent.
func TestAgentsAPIRulesAndRevisions(t *testing.T) {
	ctx := context.Background()
	s := newStack(t, ctx)
	owner := s.owner(t, "Agents Co")
	_, inv := owner.call("POST", "/v1/invites", map[string]any{"email": "ed@example.com", "role": "editor"})
	editor := newPerson(t, s.base)
	accept(t, editor, inv["token"].(string), "editor-long-password", "Ed")
	_, inv = owner.call("POST", "/v1/invites", map[string]any{"email": "vi@example.com", "role": "viewer"})
	viewer := newPerson(t, s.base)
	accept(t, viewer, inv["token"].(string), "viewer-long-password", "Vi")

	conn := owner.mustOK("POST", "/v1/providers", map[string]any{
		"name": "compat", "provider": "openai_compatible", "base_url": s.fakes.servers["openai_compatible"].URL, "api_key": "compat-key-123456",
	}, 201)

	// Validation: each of these is a 400 and creates nothing.
	for name, mutate := range map[string]func(map[string]any){
		"bad name":            func(b map[string]any) { b["name"] = "Not Valid" },
		"no model":            func(b map[string]any) { b["model"] = "" },
		"no max_iterations":   func(b map[string]any) { delete(b, "max_iterations") },
		"too many iterations": func(b map[string]any) { b["max_iterations"] = 51 },
		"unknown tool":        func(b map[string]any) { b["tools"] = []string{"search_kb"} },
		"unknown connection":  func(b map[string]any) { b["connection"] = "ghost" },
		"bad temperature":     func(b map[string]any) { b["temperature"] = 2 },
		"bad output schema":   func(b map[string]any) { b["output_schema"] = map[string]any{"oneOf": []any{}} },
		"bad input schema":    func(b map[string]any) { b["input_schema"] = map[string]any{"oneOf": []any{}} },
		"bad allowed host":    func(b map[string]any) { b["allowed_hosts"] = []string{"https://api.example.com/path"} },
	} {
		body := agentBody("candidate", "compat")
		mutate(body)
		if status, resp := owner.call("POST", "/v1/agents", body); status != 400 {
			t.Errorf("%s: status %d %v, want 400", name, status, resp)
		}
	}
	if _, list := owner.call("GET", "/v1/agents", nil); len(list["items"].([]any)) != 0 {
		t.Fatalf("a rejected agent was saved: %v", list)
	}

	created := owner.mustOK("POST", "/v1/agents", agentBody("helper", "compat"), 201)
	if created["revision"] != float64(1) || created["connection"] != "compat" {
		t.Fatalf("created = %v", created)
	}
	if status, _ := owner.call("POST", "/v1/agents", agentBody("helper", "compat")); status != 409 {
		t.Fatalf("a duplicate name = %d, want 409", status)
	}
	// Roles: editors manage agents, viewers only read.
	if status, _ := viewer.call("POST", "/v1/agents", agentBody("by-viewer", "")); status != 403 {
		t.Fatalf("a viewer creating an agent = %d, want 403", status)
	}
	if status, _ := viewer.call("GET", "/v1/agents/helper", nil); status != 200 {
		t.Fatalf("a viewer reading an agent = %d", status)
	}
	if status, _ := editor.call("POST", "/v1/providers", map[string]any{"name": "x", "provider": "openai", "api_key": "k"}); status != 403 {
		t.Fatalf("an editor creating a provider connection = %d, want 403 (admins manage keys)", status)
	}
	update := agentBody("ignored-name", "compat")
	update["system_prompt"] = "Be terse."
	updated := editor.mustOK("PUT", "/v1/agents/helper", update, 200)
	if updated["revision"] != float64(2) || updated["system_prompt"] != "Be terse." || updated["name"] != "helper" {
		t.Fatalf("updated = %v", updated)
	}
	if status, _ := editor.call("PUT", "/v1/agents/missing", agentBody("missing", "")); status != 404 {
		t.Fatalf("updating a missing agent = %d, want 404", status)
	}

	// Tools are discoverable.
	_, tools := viewer.call("GET", "/v1/agent-tools", nil)
	names := []string{}
	for _, item := range tools["items"].([]any) {
		names = append(names, item.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "current_time,describe_table,http_request,list_tables,run_query" {
		t.Fatalf("tools = %v", names)
	}

	// A connection used by an agent cannot be deleted; unused ones can.
	if status, body := owner.call("DELETE", "/v1/providers/"+conn["id"].(string), nil); status != 409 || body["code"] != "connection_in_use" {
		t.Fatalf("deleting a connection in use = %d %v, want 409", status, body)
	}
	_, list := owner.call("GET", "/v1/providers", nil)
	if list["items"].([]any)[0].(map[string]any)["agents"] != float64(1) {
		t.Fatalf("connection list = %v", list)
	}

	// Another organization sees neither agents nor connections, and cannot use a name that is not theirs.
	other := s.owner(t, "Other Co")
	if status, _ := other.call("GET", "/v1/agents/helper", nil); status != 404 {
		t.Fatalf("another organization reading the agent = %d, want 404", status)
	}
	if status, body := other.call("POST", "/v1/agents", agentBody("mine", "compat")); status != 400 {
		t.Fatalf("another organization using our connection = %d %v, want 400", status, body)
	}
	if status, _ := other.call("DELETE", "/v1/providers/"+conn["id"].(string), nil); status != 404 {
		t.Fatalf("another organization deleting our connection = %d, want 404", status)
	}

	if status, _ := owner.call("DELETE", "/v1/agents/helper", nil); status != 204 {
		t.Fatalf("delete agent = %d", status)
	}
	if status, _ := owner.call("DELETE", "/v1/providers/"+conn["id"].(string), nil); status != 204 {
		t.Fatalf("delete connection after its agent is gone = %d", status)
	}
}

// Runtime failures of a stored agent are permanent and name the problem.
func TestAgentNodeFailuresAreClear(t *testing.T) {
	ctx := context.Background()
	s := newStack(t, ctx)
	owner := s.owner(t, "Failures Co")
	owner.mustOK("POST", "/v1/providers", map[string]any{
		"name": "compat", "provider": "openai_compatible", "base_url": s.fakes.servers["openai_compatible"].URL, "api_key": "compat-key-123456",
	}, 201)
	body := agentBody("strict", "compat")
	body["input_schema"] = map[string]any{"type": "object", "required": []string{"text"}, "properties": map[string]any{"text": map[string]any{"type": "string"}}}
	owner.mustOK("POST", "/v1/agents", body, 201)

	cases := map[string]struct{ config, code string }{
		"unknown agent":      {"{agent: nobody, input: {a: 1}}", "E_AGENT_NOT_FOUND"},
		"input violates":     {"{agent: strict, input: {text: 5}}", "invalid_input"},
		"input missing text": {"{agent: strict, input: {}}", "invalid_input"},
	}
	for name, tt := range cases {
		wf := "fail-" + strings.ReplaceAll(strings.ReplaceAll(name, " ", "-"), "_", "-")
		if status, resp := owner.publish("apiVersion: workflow/v1\nname: " + wf + "\nnodes:\n  - id: a\n    type: agent\n    retry: {max_attempts: 3, backoff: fixed, initial: 10ms}\n    config: " + tt.config + "\n"); status != 201 {
			t.Fatalf("%s: publish = %d %v", name, status, resp)
		}
		_, started := owner.call("POST", "/v1/runs", map[string]any{"workflow": wf})
		owner.waitRun(started["id"].(string))
		attempts := s.h.attempts(mustParse(t, started["id"].(string)), "a")
		if len(attempts) != 1 || attempts[0]["code"] != tt.code {
			t.Errorf("%s: attempts = %v, want one attempt failing %s", name, attempts, tt.code)
		}
	}
	// The test endpoint reports a failing agent as ok:false (HTTP 200), with the reason.
	result := owner.mustOK("POST", "/v1/agents/strict/test", map[string]any{"input": map[string]any{"text": 5}}, 200)
	if result["ok"] != false || result["error"].(map[string]any)["code"] != "invalid_input" {
		t.Fatalf("agent test = %v", result)
	}
}

// toolServer is an OpenAI-compatible fake that asks for the http_request tool on
// its first call and answers once it has the tool result.
func toolServer(t *testing.T, target string) (*httptest.Server, func() string) {
	t.Helper()
	var calls atomic.Int32
	var mu sync.Mutex
	var toolResult string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &req)
		if calls.Add(1) == 1 {
			_, _ = fmt.Fprintf(w, `{"model":"fake-model","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"http_request","arguments":%q}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":3}}`,
				`{"url":"`+target+`"}`)
			return
		}
		for _, m := range req.Messages {
			if m.Role == "tool" {
				mu.Lock()
				toolResult = m.Content
				mu.Unlock()
			}
		}
		_, _ = io.WriteString(w, `{"model":"fake-model","choices":[{"finish_reason":"stop","message":{"content":"{\"summary\":\"done\"}"}}],"usage":{"prompt_tokens":10,"completion_tokens":3}}`)
	}))
	t.Cleanup(server.Close)
	return server, func() string {
		mu.Lock()
		defer mu.Unlock()
		return toolResult
	}
}

// The http_request tool is fail-closed: it works only for hosts in the agent's
// own allowed_hosts, and the model is told why when it is refused.
func TestAgentHTTPToolRespectsAllowedHosts(t *testing.T) {
	ctx := context.Background()
	s := newStack(t, ctx)
	owner := s.owner(t, "Tools Co")
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, `{"fact":"the sky is blue"}`)
	}))
	t.Cleanup(target.Close)

	for name, tt := range map[string]struct {
		hosts    []string
		wantHits int32
		wantIn   string
	}{
		"allowed host":  {[]string{"127.0.0.1"}, 1, "the sky is blue"},
		"no allow-list": {nil, 0, "no allowed_hosts"},
		"another host":  {[]string{"example.com"}, 0, "not in this agent's allowed_hosts"},
	} {
		t.Run(name, func(t *testing.T) {
			hits.Store(0)
			server, toolResult := toolServer(t, target.URL+"/fact")
			conn := strings.ReplaceAll(name, " ", "-")
			owner.mustOK("POST", "/v1/providers", map[string]any{"name": conn, "provider": "openai_compatible", "base_url": server.URL, "api_key": "compat-key-123456", "skip_test": true}, 201)
			agentBody := agentBody("fetcher-"+conn, conn)
			agentBody["tools"] = []string{"http_request"}
			agentBody["max_iterations"] = 3
			if tt.hosts != nil {
				agentBody["allowed_hosts"] = tt.hosts
			}
			owner.mustOK("POST", "/v1/agents", agentBody, 201)
			result := owner.mustOK("POST", "/v1/agents/fetcher-"+conn+"/test", map[string]any{"input": map[string]any{"q": "fact"}}, 200)
			if result["ok"] != true || result["metrics"].(map[string]any)["tool_calls"] != float64(1) {
				t.Fatalf("agent test = %v", result)
			}
			if hits.Load() != tt.wantHits {
				t.Fatalf("the target was called %d times, want %d", hits.Load(), tt.wantHits)
			}
			if got := toolResult(); !strings.Contains(got, tt.wantIn) {
				t.Fatalf("the model's tool result = %q, want it to contain %q", got, tt.wantIn)
			}
		})
	}
}
