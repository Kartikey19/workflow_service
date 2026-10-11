//go:build integration

package integration_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kartikey/workflow_service/internal/app"
	"github.com/kartikey/workflow_service/internal/config"
	"github.com/kartikey/workflow_service/internal/server"
)

// fakeProviders stands in for the four LLM providers' HTTP APIs. Each records
// the credential it was called with and answers a structured summary.
type fakeProviders struct {
	t       *testing.T
	mu      sync.Mutex
	keys    map[string][]string // provider -> credentials seen
	servers map[string]*httptest.Server
}

const summaryJSON = `{\"summary\":\"short summary\"}`

func newFakeProviders(t *testing.T) *fakeProviders {
	f := &fakeProviders{t: t, keys: map[string][]string{}, servers: map[string]*httptest.Server{}}
	record := func(provider, credential string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.keys[provider] = append(f.keys[provider], credential)
	}
	reject := func(w http.ResponseWriter, ok bool) bool {
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"bad key"},"type":"error"}`)
		}
		return !ok
	}
	f.servers["anthropic"] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("x-api-key")
		record("anthropic", key)
		if reject(w, key != "bad-key") {
			return
		}
		if r.URL.Path == "/v1/models" {
			_, _ = io.WriteString(w, `{"data":[{"id":"claude-sonnet-5-5"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"model":"claude-sonnet-5-5","stop_reason":"end_turn","usage":{"input_tokens":40,"output_tokens":9},
			"content":[{"type":"text","text":"{\"summary\":\"short summary\"}"}]}`)
	}))
	openaiHandler := func(provider string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			record(provider, key)
			if reject(w, key != "bad-key") {
				return
			}
			if strings.HasSuffix(r.URL.Path, "/models") {
				_, _ = io.WriteString(w, `{"data":[{"id":"fake-model"}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"model":"fake-model","choices":[{"finish_reason":"stop","message":{"content":"{\"summary\":\"short summary\"}"}}],"usage":{"prompt_tokens":30,"completion_tokens":7}}`)
		}
	}
	f.servers["openai"] = httptest.NewServer(openaiHandler("openai"))
	f.servers["openai_compatible"] = httptest.NewServer(openaiHandler("openai_compatible"))
	f.servers["gemini"] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("x-goog-api-key")
		record("gemini", key)
		if reject(w, key != "bad-key") {
			return
		}
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = io.WriteString(w, `{"models":[{"name":"models/fake-gemini","supportedGenerationMethods":["generateContent"]}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"{\"summary\":\"short summary\"}"}]}}],"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":5}}`)
	}))
	for _, server := range f.servers {
		t.Cleanup(server.Close)
	}
	return f
}

func (f *fakeProviders) seen(provider string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys[provider]...)
}

// stack is the application assembled exactly as the binaries assemble it
// (app.Build), with a real worker and the real HTTP handlers.
type stack struct {
	h     *harness
	deps  *app.Deps
	base  string
	fakes *fakeProviders
}

func newStack(t *testing.T, ctx context.Context) *stack {
	t.Helper()
	h := newHarness(t, ctx) // for the isolated database and helpers
	fakes := newFakeProviders(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 7)
	}
	cfg := config.Config{
		App:    config.AppConfig{Environment: "testing", Mode: "api", ServiceName: "stack", BuildVersion: "test"},
		Worker: config.WorkerConfig{ID: "stack-worker", Concurrency: 8, BatchSize: 4, PollInterval: 50 * time.Millisecond, LeaseTTL: 3 * time.Second, SweepInterval: 200 * time.Millisecond, MaxNodesPerRun: 1000},
		Egress: config.EgressConfig{AllowPrivate: true},
		Vault:  config.VaultConfig{MasterKey: base64.StdEncoding.EncodeToString(key), KeyVersion: 1},
		LLM: config.LLMConfig{
			AnthropicAPIKey: "platform-anthropic-key", AnthropicBaseURL: fakes.servers["anthropic"].URL,
			OpenAIBaseURL: fakes.servers["openai"].URL, GeminiBaseURL: fakes.servers["gemini"].URL,
		},
	}
	deps, err := app.Build(ctx, cfg, h.pool, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Config{Address: "127.0.0.1:0", API: deps.Services()}, h.pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	workerCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = deps.Worker(cfg.Worker).Run(workerCtx, 2*time.Second)
	}()
	t.Cleanup(func() {
		stop()
		<-done
	})
	return &stack{h: h, deps: deps, base: ts.URL, fakes: fakes}
}

// owner signs in as the owner of a fresh organization on the pro plan.
func (s *stack) owner(t *testing.T, org string) *person {
	t.Helper()
	ctx := context.Background()
	email := strings.ToLower(strings.ReplaceAll(org, " ", "")) + "@example.com"
	if _, err := s.deps.Identity.Bootstrap(ctx, "root@example.com", adminPassword); err != nil {
		t.Fatal(err)
	}
	root := newPerson(t, s.base)
	root.mustLogin("root@example.com", adminPassword)
	owner := newPerson(t, s.base)
	if status, body := accept(t, owner, inviteToOrg(t, root, email, org), "owner-long-password", "Owner"); status != 201 {
		t.Fatalf("accept = %d %v", status, body)
	}
	if status, body := root.call("PUT", "/v1/admin/organizations/"+owner.orgID()+"/plan", map[string]any{"plan": "pro"}); status != 200 {
		t.Fatalf("plan = %d %v", status, body)
	}
	return owner
}

func (p *person) mustOK(method, path string, body any, want int) map[string]any {
	p.t.Helper()
	status, decoded := p.call(method, path, body)
	if status != want {
		p.t.Fatalf("%s %s = %d %v, want %d", method, path, status, decoded, want)
	}
	return decoded
}

func (p *person) waitRun(id string) map[string]any {
	p.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, run := p.call("GET", "/v1/runs/"+id, nil)
		switch run["status"] {
		case "succeeded", "failed", "cancelled":
			return run
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("run %s did not finish: %v", id, run)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// The whole product through the real wiring: a vault secret reaches an
// upstream, a stored agent runs on a tenant provider connection, an inline agent
// runs on the platform provider, and a sub-workflow finishes the pipeline.
func TestAppWiringEndToEnd(t *testing.T) {
	ctx := context.Background()
	s := newStack(t, ctx)

	// Every node type the spec documents is registered by Build.
	for _, nodeType := range []string{"noop", "transform", "branch", "sleep", "wait_event", "human_task", "http", "agent", "workflow"} {
		if _, ok := s.deps.Registry.Get(nodeType); !ok {
			t.Errorf("app.Build does not register the %q node type", nodeType)
		}
	}
	if s.deps.Secrets == nil || s.deps.Vault == nil || s.deps.Providers == nil || s.deps.Agents == nil || s.deps.Tasks == nil || s.deps.Identity == nil {
		t.Fatalf("app.Build left a service unwired: %+v", s.deps)
	}

	owner := s.owner(t, "Stack Co")
	var echoed []string
	var mu sync.Mutex
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		echoed = append(echoed, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"text":"a long article","you_sent":%q}`, r.Header.Get("Authorization"))
	}))
	t.Cleanup(echo.Close)

	owner.mustOK("PUT", "/v1/secrets/UPSTREAM_KEY", map[string]any{"value": "upstream-secret-9999"}, 200)
	conn := owner.mustOK("POST", "/v1/providers", map[string]any{
		"name": "fake-openai", "provider": "openai_compatible", "base_url": s.fakes.servers["openai_compatible"].URL, "api_key": "compat-key-123456",
	}, 201)
	if conn["key_hint"] != "…3456" || strings.Contains(toJSON(conn), "compat-key-123456") {
		t.Fatalf("connection = %v", conn)
	}
	owner.mustOK("POST", "/v1/agents", map[string]any{
		"name": "summarizer", "connection": "fake-openai", "model": "fake-model", "system_prompt": "Summarize the text.",
		"input_schema":   map[string]any{"type": "object", "required": []string{"text"}, "properties": map[string]any{"text": map[string]any{"type": "string"}}},
		"output_schema":  map[string]any{"type": "object", "required": []string{"summary"}, "properties": map[string]any{"summary": map[string]any{"type": "string"}}},
		"max_iterations": 2,
	}, 201)

	for _, source := range []string{doubleWorkflowFor("double-it"), `
apiVersion: workflow/v1
name: pipeline
inputs:
  n: {type: number, required: true}
nodes:
  - id: fetch
    type: http
    timeout: 5s
    config:
      method: GET
      url: "` + echo.URL + `/article"
      headers: {Authorization: "Bearer ${secrets.UPSTREAM_KEY}"}
  - id: summarize
    type: agent
    needs: [fetch]
    config:
      agent: summarizer
      input: {text: '${nodes.fetch.output.body.text}'}
  - id: triage
    type: agent
    needs: [fetch]
    config: {model: claude-sonnet-5-5, prompt: 'say ok', max_iterations: 1, output_schema: {type: object, required: [summary]}}
  - id: double
    type: workflow
    needs: [summarize, triage]
    config: {name: double-it, input: {n: '${input.n}'}}
outputs:
  summary: '${nodes.summarize.output.summary}'
  platform: '${nodes.triage.output.summary}'
  doubled: '${nodes.double.output.output.result}'
`} {
		if status, body := owner.publish(source); status != 201 {
			t.Fatalf("publish = %d %v", status, body)
		}
	}
	_, started := owner.call("POST", "/v1/runs", map[string]any{"workflow": "pipeline", "input": map[string]any{"n": 21}})
	run := owner.waitRun(started["id"].(string))
	if run["status"] != "succeeded" {
		_, nodes := owner.call("GET", "/v1/runs/"+started["id"].(string)+"/nodes", nil)
		t.Fatalf("run = %v, nodes %v", run, nodes)
	}
	runID := mustParse(t, started["id"].(string))
	out, _ := s.h.runOutput(runID)
	if out["summary"] != "short summary" || out["platform"] != "short summary" || out["doubled"] != float64(42) {
		t.Fatalf("outputs = %v", out)
	}

	// The credentials each hop used.
	mu.Lock()
	gotEcho := append([]string(nil), echoed...)
	mu.Unlock()
	if len(gotEcho) != 1 || gotEcho[0] != "Bearer upstream-secret-9999" {
		t.Fatalf("the upstream received %v, want the vault secret", gotEcho)
	}
	if got := s.fakes.seen("openai_compatible"); len(got) < 2 || got[len(got)-1] != "compat-key-123456" {
		t.Fatalf("the tenant provider saw %v, want its own key (a test call, then the agent call)", got)
	}
	if got := s.fakes.seen("anthropic"); len(got) != 1 || got[0] != "platform-anthropic-key" {
		t.Fatalf("the platform provider saw %v", got)
	}
	for _, secret := range []string{"upstream-secret-9999", "compat-key-123456", "platform-anthropic-key"} {
		if n := s.h.occurrences(secret); n != 0 {
			t.Errorf("%q is stored in %d row(s)", secret, n)
		}
	}
	if m := s.h.nodeMetrics(runID, "summarize"); m["agent"] != "summarizer" || m["agent_revision"] != float64(1) || m["tokens_in"] != float64(30) {
		t.Fatalf("summarize metrics = %v", m)
	}
	if kids := s.h.children(runID); len(kids) != 1 || kids[0].Status != "succeeded" {
		t.Fatalf("children = %v", kids)
	}
	// The management routes exist behind the same wiring.
	for _, path := range []string{"/v1/secrets", "/v1/providers", "/v1/agents", "/v1/agent-tools", "/v1/usage", "/v1/members", "/v1/tasks", "/v1/executors"} {
		if status, _ := owner.call("GET", path, nil); status != 200 {
			t.Errorf("GET %s = %d", path, status)
		}
	}
}

func doubleWorkflowFor(name string) string {
	return strings.Replace(doubleWorkflow, "name: double\n", "name: "+name+"\n", 1)
}

// M13: each of Anthropic, OpenAI, OpenAI-compatible and Gemini passes the
// connection test and runs a structured-output stored agent in a workflow; a bad
// key is rejected before anything is saved.
func TestProvidersConnectionTestAndStructuredAgentForEachKind(t *testing.T) {
	ctx := context.Background()
	s := newStack(t, ctx)
	owner := s.owner(t, "Providers Co")

	for _, tt := range []struct {
		provider, model, wantModel, baseURL string
	}{
		{"anthropic", "claude-sonnet-5-5", "claude-sonnet-5-5", ""},
		{"openai", "fake-model", "fake-model", ""},
		{"openai_compatible", "fake-model", "fake-model", s.fakes.servers["openai_compatible"].URL},
		{"gemini", "fake-gemini", "fake-gemini", ""},
	} {
		t.Run(tt.provider, func(t *testing.T) {
			// Bad key: the test fails with a clear message and nothing is saved.
			bad := map[string]any{"provider": tt.provider, "api_key": "bad-key", "base_url": tt.baseURL}
			status, body := owner.call("POST", "/v1/providers/test", bad)
			if status != 422 || body["code"] != "provider_test_failed" || !strings.Contains(body["detail"].(string), "rejected the API key") || strings.Contains(toJSON(body), "bad-key") {
				t.Fatalf("a bad key = %d %v, want a 422 that does not echo the key", status, body)
			}
			bad["name"] = tt.provider + "-bad"
			if status, _ := owner.call("POST", "/v1/providers", bad); status != 422 {
				t.Fatalf("saving a connection with a bad key = %d, want 422", status)
			}
			_, list := owner.call("GET", "/v1/providers", nil)
			for _, item := range list["items"].([]any) {
				if item.(map[string]any)["name"] == tt.provider+"-bad" {
					t.Fatal("a connection that failed its test was saved")
				}
			}

			// Good key: the test lists models; the connection is saved; keys are write-only.
			good := map[string]any{"name": tt.provider + "-conn", "provider": tt.provider, "api_key": "good-key-123456", "base_url": tt.baseURL}
			created := owner.mustOK("POST", "/v1/providers", good, 201)
			models := created["models"].([]any)
			if len(models) != 1 || models[0] != tt.wantModel {
				t.Fatalf("models = %v", models)
			}
			if strings.Contains(toJSON(created), "good-key-123456") || created["key_hint"] != "…3456" {
				t.Fatalf("created = %v", created)
			}
			owner.mustOK("POST", "/v1/providers/"+created["id"].(string)+"/test", nil, 200)

			// A structured-output stored agent runs in a workflow on that connection.
			agentName := tt.provider + "-agent"
			owner.mustOK("POST", "/v1/agents", map[string]any{
				"name": agentName, "connection": tt.provider + "-conn", "model": tt.model, "system_prompt": "Summarize.",
				"output_schema":  map[string]any{"type": "object", "required": []string{"summary"}, "properties": map[string]any{"summary": map[string]any{"type": "string"}}},
				"max_iterations": 1,
			}, 201)
			wf := strings.ReplaceAll(tt.provider, "_", "-") + "-flow"
			if status, body := owner.publish("apiVersion: workflow/v1\nname: " + wf + "\nnodes:\n  - id: a\n    type: agent\n    config: {agent: " + agentName + ", input: {text: hello}}\noutputs: {summary: '${nodes.a.output.summary}'}\n"); status != 201 {
				t.Fatalf("publish = %d %v", status, body)
			}
			_, started := owner.call("POST", "/v1/runs", map[string]any{"workflow": wf})
			run := owner.waitRun(started["id"].(string))
			if run["status"] != "succeeded" {
				t.Fatalf("run = %v", run)
			}
			if out, _ := s.h.runOutput(mustParse(t, started["id"].(string))); out["summary"] != "short summary" {
				t.Fatalf("outputs = %v", out)
			}
			if seen := s.fakes.seen(tt.provider); len(seen) == 0 || seen[len(seen)-1] != "good-key-123456" {
				t.Fatalf("%s saw credentials %v, want its stored key last", tt.provider, seen)
			}

			// The agent's own test endpoint works too.
			result := owner.mustOK("POST", "/v1/agents/"+agentName+"/test", map[string]any{"input": map[string]any{"text": "hi"}}, 200)
			if result["ok"] != true || result["output"].(map[string]any)["summary"] != "short summary" {
				t.Fatalf("agent test = %v", result)
			}
		})
	}
}
