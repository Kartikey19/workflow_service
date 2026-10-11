//go:build integration

package integration_test

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// This is optional for Go-only development; CI runs it after building the UI
// and installing Chromium. The browser talks to the actual app and worker.
func TestWebUIBrowser(t *testing.T) {
	if os.Getenv("RUN_UI_TESTS") != "true" {
		t.Skip("set RUN_UI_TESTS=true after npm ci and playwright install chromium")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	s := newStack(t, ctx)
	owner := s.owner(t, "Browser Co")
	owner.mustOK("POST", "/v1/providers", map[string]any{"name": "browser-provider", "provider": "openai_compatible", "base_url": s.fakes.servers["openai_compatible"].URL, "api_key": "browser-provider-test-key"}, 201)
	invite := owner.mustOK("POST", "/v1/invites", map[string]any{"email": "browser@example.com", "role": "admin"}, 201)
	cmd := exec.CommandContext(ctx, "npm", "run", "test:browser")
	cmd.Dir = "../../web"
	cmd.Env = append(os.Environ(), "UI_TEST_URL="+s.base, "UI_TEST_INVITE="+invite["token"].(string), "UI_TEST_PROVIDER_URL="+s.fakes.servers["openai_compatible"].URL)
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
	}{{"GET", "/", 200}, {"GET", "/accept-invite", 200}, {"GET", "/v1/missing", 404}, {"POST", "/v1/missing", 404}, {"POST", "/hooks/missing/extra", 404}, {"GET", "/assets/missing.js", 404}} {
		status, _, _ := client.do(tt.method, tt.path, nil)
		if status != tt.want {
			t.Errorf("%s %s = %d, want %d", tt.method, tt.path, status, tt.want)
		}
	}
}
