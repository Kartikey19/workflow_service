//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/kartikey/workflow_service/internal/config"
	"github.com/kartikey/workflow_service/internal/services/assistant"
	"github.com/kartikey/workflow_service/internal/spec"
)

func TestCreatorPreservesAcceptedStepsAndExcludesSecrets(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	var payloads []string
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			io.WriteString(w, `{"data":[{"id":"fake-model"}]}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var request struct {
			Model string `json:"model"`
			Tools []any  `json:"tools"`
		}
		if json.Unmarshal(raw, &request) != nil || request.Model != assistant.CreatorModel || len(request.Tools) != 0 {
			t.Error("creator must use fixed model with no tools")
		}
		mu.Lock()
		calls++
		round := calls
		payloads = append(payloads, string(raw))
		mu.Unlock()
		node := "  - {id: next, type: noop, needs: [accepted], config: {}}\n"
		if round == 1 {
			node += "  - {id: extra, type: noop, config: {}}\n"
		}
		if round == 2 {
			node = "  - {id: next, type: nonexistent, config: {}}\n"
		}
		if round > 3 {
			node = "  - {id: accepted, type: noop, config: {}}\n"
		}
		yaml := "apiVersion: workflow/v1\nname: overwrite-attempt\ninputs: {replacement: {type: string}}\nnodes:\n" + node
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": yaml}}}})
	}))
	defer model.Close()
	s := newStack(t, context.Background(), func(c *config.Config) {
		c.Assistant = config.AssistantConfig{Provider: "openrouter", APIKey: "platform-secret-canary", BaseURL: model.URL}
	})
	owner := s.owner(t, "Creator Co")
	owner.mustOK("POST", "/be/v1/providers", map[string]any{"name": "client-key", "provider": "openai_compatible", "base_url": model.URL, "api_key": "provider-secret-canary"}, 201)
	owner.mustOK("PUT", "/be/v1/secrets/PRIVATE", map[string]any{"value": "vault-secret-canary"}, 200)
	body := agentBody("helper", "client-key")
	body["system_prompt"] = "instructions-secret-canary"
	body["description"] = "description-secret-canary"
	body["input_schema"] = map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string", "default": "default-secret-canary", "enum": []string{"enum-secret-canary"}}}}
	owner.mustOK("POST", "/be/v1/agents", body, 201)
	approved := "apiVersion: workflow/v1\nname: private-draft\ninputs:\n  text: {type: string, default: input-secret-canary}\nnodes:\n  - id: accepted\n    type: transform\n    config: {value: config-secret-canary}\noutputs: {value: '${nodes.accepted.output}'}\n"
	before, err := spec.Decode([]byte(approved))
	if err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"requirements": "Add the next step using helper if needed", "stepwise": true, "approved_yaml": approved, "model": "ignored-model", "connection": "ignored-account"}
	draft := owner.mustOK("POST", "/be/v1/assistant/workflows", input, 200)
	if draft["attempts"] != float64(3) || draft["valid"] != true {
		t.Fatal("step repair did not validate")
	}
	after, err := spec.Decode([]byte(draft["yaml"].(string)))
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != before.Name || !reflect.DeepEqual(after.Inputs, before.Inputs) || !reflect.DeepEqual(after.Outputs, before.Outputs) || len(after.Nodes) != 2 || !reflect.DeepEqual(after.Nodes[0], before.Nodes[0]) {
		t.Fatal("proposal modified an accepted field")
	}
	if status, _ := owner.call("GET", "/be/v1/workflows/private-draft", nil); status != 404 {
		t.Fatal("creator published without approval")
	}
	status, rejected := owner.call("POST", "/be/v1/assistant/workflows", input)
	if status != 422 || rejected["valid"] != false || rejected["attempts"] != float64(4) || rejected["yaml"] != approved {
		t.Fatal("replacement of an accepted step was allowed")
	}
	mu.Lock()
	prompt := strings.Join(payloads, "\n")
	mu.Unlock()
	for _, canary := range []string{"platform-secret-canary", "provider-secret-canary", "vault-secret-canary", "instructions-secret-canary", "description-secret-canary", "default-secret-canary", "enum-secret-canary", "example-secret-canary", "input-secret-canary", "config-secret-canary"} {
		if strings.Contains(prompt, canary) {
			t.Fatalf("creator payload exposed %s", canary)
		}
	}
	for _, name := range []string{"helper", "client-key", "PRIVATE", "accepted"} {
		if !strings.Contains(prompt, name) {
			t.Fatalf("creator lacks resource reference %s", name)
		}
	}
}

func TestProviderModelListIsNotTruncated(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		items := []map[string]string{}
		for i := 0; i < 350; i++ {
			items = append(items, map[string]string{"id": fmt.Sprintf("model-%03d", i)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
	defer model.Close()
	s := newStack(t, context.Background())
	owner := s.owner(t, "Complete Models")
	provider := owner.mustOK("POST", "/be/v1/providers", map[string]any{"name": "many-models", "provider": "openai_compatible", "base_url": model.URL, "api_key": "fake-key"}, 201)
	result := owner.mustOK("GET", "/be/v1/providers/"+provider["id"].(string)+"/models", nil, 200)
	if len(result["models"].([]any)) != 350 {
		t.Fatal("provider model list was truncated")
	}
}
