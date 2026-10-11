//go:build integration

package integration_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kartikey/workflow_service/internal/services/vault"
	"github.com/kartikey/workflow_service/pkg/uid"
	vaultcrypto "github.com/kartikey/workflow_service/pkg/vault"
)

func hookSignature(secret string, body any) string {
	raw, _ := json.Marshal(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
func TestWebhookTriggers(t *testing.T) {
	ctx := context.Background()
	s := newStack(t, ctx)
	owner := s.owner(t, "Hooks Co")
	anonymous := newPerson(t, s.base)
	if status, resp := owner.publish("apiVersion: workflow/v1\nname: hooked\ninputs:\n  message: {type: string, required: true}\nnodes:\n  - {id: receive, type: transform, config: {value: '${input.message}'}}\n"); status != 201 {
		t.Fatalf("publish %d %v", status, resp)
	}
	hook := owner.mustOK("POST", "/be/v1/triggers", map[string]any{"name": "incoming", "workflow": "hooked", "signed": true}, 201)
	credential := hook["token"].(string)
	secret := hook["signing_secret"].(string)
	path := "/hooks/" + credential
	body := map[string]any{"message": "hello"}
	signature := hookSignature(secret, body)
	if status, _ := anonymous.call("POST", path, body); status != 401 {
		t.Fatalf("missing HMAC %d", status)
	}
	if status, _ := anonymous.call("POST", path, body, "X-Workflow-Signature", hookSignature("wrong", body)); status != 401 {
		t.Fatalf("bad HMAC %d", status)
	}
	status, run := anonymous.call("POST", path, body, "X-Workflow-Signature", signature, "Idempotency-Key", "event-1")
	if status != 201 {
		t.Fatalf("fire %d %v", status, run)
	}
	if finished := owner.waitRun(run["id"].(string)); finished["status"] != "succeeded" || finished["trigger"] != "webhook" {
		t.Fatalf("webhook run %v", finished)
	}
	status, replay := anonymous.call("POST", path, body, "X-Workflow-Signature", signature, "Idempotency-Key", "event-1")
	if status != 200 || replay["id"] != run["id"] {
		t.Fatalf("replay %d %v", status, replay)
	}
	changed := map[string]any{"message": "different"}
	if status, _ := anonymous.call("POST", path, changed, "X-Workflow-Signature", hookSignature(secret, changed), "Idempotency-Key", "event-1"); status != 409 {
		t.Fatalf("changed replay %d", status)
	}
	_, listed := owner.call("GET", "/be/v1/triggers", nil)
	if strings.Contains(toJSON(listed), credential) || strings.Contains(toJSON(listed), secret) {
		t.Fatal("listing exposed credentials")
	}
	if s.h.occurrences(credential) != 0 || s.h.occurrences(secret) != 0 {
		t.Fatal("webhook credentials persisted in clear")
	}
	other := s.owner(t, "Other hooks")
	if status, _ := other.call("PATCH", "/be/v1/triggers/"+hook["id"].(string), map[string]any{"enabled": false}); status != 404 {
		t.Fatalf("foreign trigger %d", status)
	}
	if status, _ := other.call("POST", "/be/v1/triggers", map[string]any{"name": "foreign", "workflow": "hooked"}); status != 400 {
		t.Fatalf("foreign workflow %d", status)
	}
	owner.mustOK("PATCH", "/be/v1/triggers/"+hook["id"].(string), map[string]any{"enabled": false}, 200)
	if status, _ := anonymous.call("POST", path, body, "X-Workflow-Signature", signature); status != 404 {
		t.Fatalf("disabled trigger %d", status)
	}
	owner.mustOK("PATCH", "/be/v1/triggers/"+hook["id"].(string), map[string]any{"enabled": true}, 200)
	// Online master-key rotation covers webhook HMAC keys.
	currentKey := make([]byte, 32)
	for i := range currentKey {
		currentKey[i] = 2
	}
	oldKey := make([]byte, 32)
	for i := range oldKey {
		oldKey[i] = byte(i + 7)
	}
	// Read only the test fixture's non-secret key configuration, never a live key.
	keys, err := vaultcrypto.NewKeyring(2, map[int][]byte{2: currentKey, 1: oldKey})
	if err != nil {
		t.Fatal(err)
	}
	rotator := vault.NewService(s.h.pool, keys, testLogger())
	rotated, err := rotator.Rotate(ctx)
	if err != nil || rotated.Rotated == 0 {
		t.Fatalf("rotation %v %v", rotated, err)
	}
	// The original process doesn't know key 2; use a service with both keys to authenticate.
	// Its source uses the same run service and tenant boundary as the HTTP route.
	// Restore under key 1 so the original process can authenticate the same HMAC.
	originalKeys, err := vaultcrypto.NewKeyring(1, map[int][]byte{1: oldKey, 2: currentKey})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = vault.NewService(s.h.pool, originalKeys, testLogger()).Rotate(ctx); err != nil {
		t.Fatal(err)
	}
	if status, _ := anonymous.call("POST", path, body, "X-Workflow-Signature", signature); status != 201 {
		t.Fatalf("rotated signing key %d", status)
	}
	replaced := owner.mustOK("POST", "/be/v1/triggers/"+hook["id"].(string)+"/rotate-token", nil, 200)
	if status, _ := anonymous.call("POST", path, body, "X-Workflow-Signature", signature); status != 404 {
		t.Fatalf("old token %d", status)
	}
	path = "/hooks/" + replaced["token"].(string)
	if status, _ := anonymous.call("POST", path, body, "X-Workflow-Signature", signature); status != 201 {
		t.Fatalf("new token %d", status)
	}
	owner.mustOK("DELETE", "/be/v1/triggers/"+hook["id"].(string), nil, 204)
	if status, _ := anonymous.call("POST", path, body, "X-Workflow-Signature", signature); status != 404 {
		t.Fatalf("deleted token %d", status)
	}
}
func TestWebhookEnvelopeAndPlanAllowance(t *testing.T) {
	s := newStack(t, context.Background())
	owner := s.owner(t, "Envelope Co")
	anonymous := newPerson(t, s.base)
	source := "apiVersion: workflow/v1\nname: envelope\ninputs:\n  body: {type: object}\n  headers: {type: object}\n  query: {type: object}\nnodes:\n  - {id: receive, type: transform, config: {value: '${input.body}'}}\n"
	if status, resp := owner.publish(source); status != 201 {
		t.Fatalf("publish %d %v", status, resp)
	}
	hook := owner.mustOK("POST", "/be/v1/triggers", map[string]any{"name": "envelope", "workflow": "envelope", "input_mode": "envelope"}, 201)
	path := "/hooks/" + hook["token"].(string) + "?source=partner"
	status, run := anonymous.call("POST", path, map[string]any{"hello": "world"}, "Authorization", "Bearer sensitive-value", "X-Event", "created")
	if status != 201 {
		t.Fatalf("envelope %d %v", status, run)
	}
	detail := owner.mustOK("GET", "/be/v1/runs/"+run["id"].(string), nil, 200)
	input := detail["input"].(map[string]any)
	if strings.Contains(toJSON(input), "sensitive-value") || !strings.Contains(toJSON(input), "created") || !strings.Contains(toJSON(input), "partner") {
		t.Fatalf("envelope input %v", input)
	}
	// Use a tiny real plan allowance; all ingress shares the organization's lock.
	org, err := uid.Parse(owner.orgID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.h.pool.Exec(context.Background(), `update plans set max_runs_per_month=1 where id=(select plan_id from organizations where id=$1)`, org); err != nil {
		t.Fatal(err)
	}
	if status, _ := anonymous.call("POST", path, map[string]any{"hello": "again"}); status != 402 {
		t.Fatalf("webhook exceeded plan: %d", status)
	}
}
func TestAssistantTenReferencesAndRepair(t *testing.T) {
	s := newStack(t, context.Background())
	owner := s.owner(t, "Assistant Co")
	var mu sync.Mutex
	var systems []string
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			io.WriteString(w, `{"data":[{"id":"fake-model"}]}`)
			return
		}
		var request struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&request)
		requirement := ""
		repair := false
		for _, m := range request.Messages {
			if m.Role == "system" {
				mu.Lock()
				systems = append(systems, m.Content)
				mu.Unlock()
			}
			if m.Role == "user" && requirement == "" {
				requirement = m.Content
			}
			if strings.Contains(m.Content, "Repair this draft") {
				repair = true
			}
		}
		yaml := "apiVersion: workflow/v1\nname: drafted\nnodes:\n  - {id: work, type: noop, config: {}}\n"
		if strings.Contains(requirement, "agent") {
			yaml = "apiVersion: workflow/v1\nname: drafted\nnodes:\n  - {id: work, type: agent, config: {agent: helper, input: {text: hello}}}\n"
		}
		if strings.Contains(requirement, "repair") && !repair || strings.Contains(requirement, "never-valid") {
			yaml = "apiVersion: workflow/v1\nname: drafted\nnodes:\n  - {id: work, type: nonexistent, config: {}}\n"
		}
		fmt.Fprintf(w, `{"model":"fake-model","choices":[{"finish_reason":"stop","message":{"content":%q}}],"usage":{"prompt_tokens":10,"completion_tokens":3}}`, yaml)
	}))
	defer model.Close()
	owner.mustOK("POST", "/be/v1/providers", map[string]any{"name": "writer", "provider": "openai_compatible", "base_url": model.URL, "api_key": "never-expose-provider-key"}, 201)
	owner.mustOK("PUT", "/be/v1/secrets/EXAMPLE", map[string]any{"value": "never-expose-vault-value"}, 200)
	owner.mustOK("POST", "/be/v1/agents", agentBody("helper", "writer"), 201)
	prompts := []string{"Draft a greeting", "Transform an input", "Route by priority", "Fetch an HTTP API", "Send an email", "Query a database", "Wait for a human", "Call a subworkflow", "Use an agent", "repair a draft"}
	for _, prompt := range prompts {
		draft := owner.mustOK("POST", "/be/v1/assistant/workflows", map[string]any{"requirements": prompt, "connection": "writer", "model": "fake-model"}, 200)
		if draft["valid"] != true {
			t.Fatalf("invalid draft: %v", draft)
		}
		if strings.Contains(prompt, "repair") && draft["attempts"] != float64(2) {
			t.Fatalf("repair %v", draft)
		}
		if status, resp := owner.publish(draft["yaml"].(string)); status != 201 && status != 200 {
			t.Fatalf("draft did not publish %d %v", status, resp)
		}
	}
	_, bad := owner.call("POST", "/be/v1/assistant/workflows", map[string]any{"requirements": "never-valid", "connection": "writer", "model": "fake-model"})
	if bad["valid"] != false || bad["attempts"] != float64(4) {
		t.Fatalf("repair bound %v", bad)
	}
	mu.Lock()
	prompt := strings.Join(systems, "\n")
	mu.Unlock()
	if !strings.Contains(prompt, "helper") || !strings.Contains(prompt, "EXAMPLE") || !strings.Contains(prompt, "writer") || strings.Contains(prompt, "never-expose-vault-value") || strings.Contains(prompt, "never-expose-provider-key") {
		t.Fatal("assistant catalog omitted names or exposed values")
	}
	other := s.owner(t, "Other assistant")
	if status, _ := other.call("POST", "/be/v1/assistant/workflows", map[string]any{"requirements": "hello", "connection": "writer", "model": "fake-model"}); status != 400 {
		t.Fatalf("foreign provider %d", status)
	}
}
