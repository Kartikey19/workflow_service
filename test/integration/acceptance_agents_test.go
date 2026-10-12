//go:build integration

package integration_test

import (
	"context"
	"slices"
	"strings"
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
	_, inv := owner.call("POST", "/be/v1/invites", map[string]any{"email": "ed@example.com", "role": "editor"})
	editor := newPerson(t, s.base)
	accept(t, editor, inv["token"].(string), "editor-long-password", "Ed")
	_, inv = owner.call("POST", "/be/v1/invites", map[string]any{"email": "vi@example.com", "role": "viewer"})
	viewer := newPerson(t, s.base)
	accept(t, viewer, inv["token"].(string), "viewer-long-password", "Vi")

	conn := owner.mustOK("POST", "/be/v1/providers", map[string]any{
		"name": "compat", "provider": "openai_compatible", "base_url": s.fakes.servers["openai_compatible"].URL, "api_key": "compat-key-123456",
	}, 201)

	modelsPath := "/be/v1/providers/" + conn["id"].(string) + "/models"
	models := editor.mustOK("GET", modelsPath, nil, 200)
	if !strings.Contains(toJSON(models), "fake-model") || strings.Contains(toJSON(models), "compat-key-123456") {
		t.Fatalf("model catalog = %v", models)
	}
	if status, _ := viewer.call("GET", modelsPath, nil); status != 403 {
		t.Fatalf("viewer model catalog = %d", status)
	}

	// Validation: each of these is a 400 and creates nothing.
	for name, mutate := range map[string]func(map[string]any){
		"bad name":             func(b map[string]any) { b["name"] = "Not Valid" },
		"no model":             func(b map[string]any) { b["model"] = "" },
		"no max_iterations":    func(b map[string]any) { delete(b, "max_iterations") },
		"too many iterations":  func(b map[string]any) { b["max_iterations"] = 51 },
		"unknown tool":         func(b map[string]any) { b["tools"] = []string{"search_kb"} },
		"unknown connection":   func(b map[string]any) { b["connection"] = "ghost" },
		"bad temperature":      func(b map[string]any) { b["temperature"] = 2 },
		"bad output schema":    func(b map[string]any) { b["output_schema"] = map[string]any{"oneOf": []any{}} },
		"bad input schema":     func(b map[string]any) { b["input_schema"] = map[string]any{"oneOf": []any{}} },
		"legacy allowed_hosts": func(b map[string]any) { b["allowed_hosts"] = []string{"api.example.com"} },
	} {
		body := agentBody("candidate", "compat")
		mutate(body)
		if status, resp := owner.call("POST", "/be/v1/agents", body); status != 400 {
			t.Errorf("%s: status %d %v, want 400", name, status, resp)
		}
	}
	if _, list := owner.call("GET", "/be/v1/agents", nil); len(list["items"].([]any)) != 0 {
		t.Fatalf("a rejected agent was saved: %v", list)
	}

	created := owner.mustOK("POST", "/be/v1/agents", agentBody("helper", "compat"), 201)
	if created["revision"] != float64(1) || created["connection"] != "compat" {
		t.Fatalf("created = %v", created)
	}
	if status, _ := owner.call("POST", "/be/v1/agents", agentBody("helper", "compat")); status != 409 {
		t.Fatalf("a duplicate name = %d, want 409", status)
	}
	// Roles: editors manage agents, viewers only read.
	if status, _ := viewer.call("POST", "/be/v1/agents", agentBody("by-viewer", "")); status != 403 {
		t.Fatalf("a viewer creating an agent = %d, want 403", status)
	}
	if status, _ := viewer.call("GET", "/be/v1/agents/helper", nil); status != 200 {
		t.Fatalf("a viewer reading an agent = %d", status)
	}
	if status, _ := editor.call("POST", "/be/v1/providers", map[string]any{"name": "x", "provider": "openai", "api_key": "k"}); status != 403 {
		t.Fatalf("an editor creating a provider connection = %d, want 403 (admins manage keys)", status)
	}
	update := agentBody("ignored-name", "compat")
	update["system_prompt"] = "Be terse."
	updated := editor.mustOK("PUT", "/be/v1/agents/helper", update, 200)
	if updated["revision"] != float64(2) || updated["system_prompt"] != "Be terse." || updated["name"] != "helper" {
		t.Fatalf("updated = %v", updated)
	}
	if status, _ := editor.call("PUT", "/be/v1/agents/missing", agentBody("missing", "")); status != 404 {
		t.Fatalf("updating a missing agent = %d, want 404", status)
	}

	// Tools are discoverable: every agent searches its connectors with the same three.
	_, tools := viewer.call("GET", "/be/v1/agent-tools", nil)
	names := []string{}
	for _, item := range tools["items"].([]any) {
		names = append(names, item.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "glob,grep,read" || !strings.Contains(toJSON(tools["searchable_kinds"]), "postgres") {
		t.Fatalf("tools = %v", tools)
	}

	// A connection used by an agent cannot be deleted; unused ones can.
	if status, body := owner.call("DELETE", "/be/v1/providers/"+conn["id"].(string), nil); status != 409 || body["code"] != "connection_in_use" {
		t.Fatalf("deleting a connection in use = %d %v, want 409", status, body)
	}
	_, list := owner.call("GET", "/be/v1/providers", nil)
	if list["items"].([]any)[0].(map[string]any)["agents"] != float64(1) {
		t.Fatalf("connection list = %v", list)
	}

	// Another organization sees neither agents nor connections, and cannot use a name that is not theirs.
	other := s.owner(t, "Other Co")
	if status, _ := other.call("GET", "/be/v1/agents/helper", nil); status != 404 {
		t.Fatalf("another organization reading the agent = %d, want 404", status)
	}
	if status, body := other.call("POST", "/be/v1/agents", agentBody("mine", "compat")); status != 400 {
		t.Fatalf("another organization using our connection = %d %v, want 400", status, body)
	}
	if status, _ := other.call("GET", modelsPath, nil); status != 404 {
		t.Fatalf("another organization model catalog = %d", status)
	}

	if status, _ := other.call("DELETE", "/be/v1/providers/"+conn["id"].(string), nil); status != 404 {
		t.Fatalf("another organization deleting our connection = %d, want 404", status)
	}

	if status, _ := owner.call("DELETE", "/be/v1/agents/helper", nil); status != 204 {
		t.Fatalf("delete agent = %d", status)
	}
	if status, _ := owner.call("DELETE", "/be/v1/providers/"+conn["id"].(string), nil); status != 204 {
		t.Fatalf("delete connection after its agent is gone = %d", status)
	}
}

// Runtime failures of a stored agent are permanent and name the problem.
func TestAgentNodeFailuresAreClear(t *testing.T) {
	ctx := context.Background()
	s := newStack(t, ctx)
	owner := s.owner(t, "Failures Co")
	owner.mustOK("POST", "/be/v1/providers", map[string]any{
		"name": "compat", "provider": "openai_compatible", "base_url": s.fakes.servers["openai_compatible"].URL, "api_key": "compat-key-123456",
	}, 201)
	body := agentBody("strict", "compat")
	body["input_schema"] = map[string]any{"type": "object", "required": []string{"text"}, "properties": map[string]any{"text": map[string]any{"type": "string"}}}
	owner.mustOK("POST", "/be/v1/agents", body, 201)

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
		_, started := owner.call("POST", "/be/v1/runs", map[string]any{"workflow": wf})
		owner.waitRun(started["id"].(string))
		attempts := s.h.attempts(mustParse(t, started["id"].(string)), "a")
		if len(attempts) != 1 || attempts[0]["code"] != tt.code {
			t.Errorf("%s: attempts = %v, want one attempt failing %s", name, attempts, tt.code)
		}
	}
	// The test endpoint reports a failing agent as ok:false (HTTP 200), with the reason.
	result := owner.mustOK("POST", "/be/v1/agents/strict/test", map[string]any{"input": map[string]any{"text": 5}}, 200)
	if result["ok"] != false || result["error"].(map[string]any)["code"] != "invalid_input" {
		t.Fatalf("agent test = %v", result)
	}
}

// Every provider omp supports with a key is offered. OpenAI-compatible kinds
// bring their own endpoint, and only a local server such as Ollama may go
// without a key.
func TestProviderKindsCatalog(t *testing.T) {
	ctx := context.Background()
	s := newStack(t, ctx)
	owner := s.owner(t, "Kinds Co")
	_, kinds := owner.call("GET", "/be/v1/provider-kinds", nil)
	ids := []string{}
	for _, item := range kinds["items"].([]any) {
		ids = append(ids, item.(map[string]any)["id"].(string))
	}
	for _, want := range []string{"anthropic", "openai", "gemini", "ollama", "groq", "openrouter", "deepseek", "xai", "openai_compatible"} {
		if !slices.Contains(ids, want) {
			t.Fatalf("provider kinds %v lack %s", ids, want)
		}
	}
	local := owner.mustOK("POST", "/be/v1/providers", map[string]any{"name": "local", "provider": "ollama", "base_url": s.fakes.servers["openai_compatible"].URL}, 201)
	if local["provider"] != "ollama" || local["models"].([]any)[0] != "fake-model" {
		t.Fatalf("ollama = %v", local)
	}
	for name, body := range map[string]map[string]any{
		"a hosted kind needs a key":  {"name": "g", "provider": "groq"},
		"an unknown kind":            {"name": "x", "provider": "made_up", "api_key": "k-123456"},
		"a native kind takes no url": {"name": "a", "provider": "anthropic", "api_key": "k-123456", "base_url": "https://example.com"},
		"a custom kind needs an url": {"name": "c", "provider": "openai_compatible"},
	} {
		if status, resp := owner.call("POST", "/be/v1/providers", body); status != 400 {
			t.Errorf("%s: %d %v, want 400", name, status, resp)
		}
	}
	groq := owner.mustOK("POST", "/be/v1/providers", map[string]any{"name": "groq", "provider": "groq", "api_key": "groq-test-key-123", "skip_test": true}, 201)
	if groq["base_url"] != "https://api.groq.com/openai/v1" {
		t.Fatalf("groq uses %v, want its own endpoint", groq["base_url"])
	}
}
