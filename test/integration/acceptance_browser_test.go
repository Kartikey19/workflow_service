//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"github.com/kartikey/workflow_service/internal/config"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// This is optional for Go-only development; CI runs it after building the UI
// and installing Chromium. The browser talks to the actual app and worker.
func TestWebUIBrowser(t *testing.T) {
	if os.Getenv("RUN_UI_TESTS") != "true" {
		t.Skip("set RUN_UI_TESTS=true after npm ci and playwright install chromium")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	workspaceAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": "browser-notion", "team_id": "browser-slack"})
	}))
	defer workspaceAPI.Close()
	creatorAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		first := true
		paymentReview := false
		for _, m := range request.Messages {
			paymentReview = paymentReview || strings.Contains(m.Content, "payment review")
			if strings.Contains(m.Content, `"id":"first"`) {
				first = false
			}
		}
		node := "  - {id: first, type: transform, config: {value: hello}}\n"
		if !first {
			node = "  - {id: second, type: transform, needs: [first], config: {value: '${nodes.first.output}'}}\n"
		}
		definition := "apiVersion: workflow/v1\nname: reviewed-creator\nnodes:\n" + node
		if paymentReview {
			definition = `apiVersion: workflow/v1
name: reviewed-approval
inputs:
  amount: {type: number, required: true}
  vendor: {type: string, required: true}
nodes:
  - id: finance_review
    type: human_task
    config:
      queue: finance
      form: vendor-payment
      payload:
        amount: '${input.amount}'
        vendor: '${input.vendor}'
`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": definition}}}})
	}))
	defer creatorAPI.Close()
	s := newStack(t, ctx, func(c *config.Config) {
		ep, _ := json.Marshal(map[string]string{"slack_api": workspaceAPI.URL, "notion_api": workspaceAPI.URL})
		c.Services.IntegrationEndpoints = string(ep)
		c.Assistant = config.AssistantConfig{Provider: "openrouter", APIKey: "fake-creator-key", BaseURL: creatorAPI.URL}
	})
	owner := s.owner(t, "Browser Co")
	s.owner(t, "Fresh Setup") // no agents, connectors or provider accounts
	owner.mustOK("POST", "/be/v1/connections", dbConnection("browser-database", postgresBackend(t)), 201)
	for _, kind := range []string{"slack", "notion"} {
		owner.mustOK("POST", "/be/v1/connections", map[string]any{"name": "browser-" + kind, "kind": kind, "secret": map[string]string{"access_token": "browser-workspace-token"}}, 201)
	}
	if uri := os.Getenv("MONGO_URL"); uri != "" {
		owner.mustOK("POST", "/be/v1/connections", map[string]any{"name": "browser-mongo", "kind": "mongo", "config": map[string]any{"database": "workflow_browser", "tls": "disable"}, "secret": map[string]string{"uri": uri}}, 201)
	}
	owner.mustOK("POST", "/be/v1/providers", map[string]any{"name": "browser-provider", "provider": "openai_compatible", "base_url": s.fakes.servers["openai_compatible"].URL, "api_key": "browser-provider-test-key"}, 201)
	invite := owner.mustOK("POST", "/be/v1/invites", map[string]any{"email": "browser@example.com", "role": "admin"}, 201)
	cmd := exec.CommandContext(ctx, "npm", "run", "test:browser")
	cmd.Dir = "../../web"
	cmd.Env = append(os.Environ(), "UI_TEST_URL="+s.base, "UI_TEST_INVITE="+invite["token"].(string), "UI_TEST_PROVIDER_URL="+s.fakes.servers["openai_compatible"].URL, "UI_TEST_ADMIN_PASSWORD="+adminPassword)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("browser acceptance: %v\n%s", err, output)
	}
	t.Log(string(output))
}

func TestEmbeddedUIRouting(t *testing.T) {
	s := newStack(t, context.Background())
	client := newPerson(t, s.base)
	for _, tt := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/", 302},
		{"GET", "/ui", 307},
		{"GET", "/ui/", 200},
		{"GET", "/ui/accept-invite", 200},
		{"GET", "/ui/connectors", 200},
		{"POST", "/ui/workflows", 405},
		{"GET", "/workflows", 404},
		{"GET", "/accept-invite", 404},
		{"GET", "/be/v1/missing", 404},
		{"POST", "/be/v1/missing", 404},
		{"GET", "/v1/workflows", 404},
		{"POST", "/v1/auth/login", 404},
		{"POST", "/hooks/missing/extra", 404},
		{"GET", "/ui/assets/missing.js", 404},
		{"GET", "/assets/missing.js", 404},
	} {
		status, _, _ := client.do(tt.method, tt.path, nil)
		if status != tt.want {
			t.Errorf("%s %s = %d, want %d", tt.method, tt.path, status, tt.want)
		}
	}
	_, _, header := client.do("GET", "/?from=bookmark", nil)
	if header.Get("Location") != "/ui/?from=bookmark" {
		t.Fatalf("root redirect = %q", header.Get("Location"))
	}
}
