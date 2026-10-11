//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/core/executor/agent"
	"github.com/kartikey/workflow_service/internal/services/connections"
	"github.com/kartikey/workflow_service/internal/services/providers"
	vaultsvc "github.com/kartikey/workflow_service/internal/services/vault"
	"github.com/kartikey/workflow_service/pkg/netguard"
	"github.com/kartikey/workflow_service/pkg/vault"
)

const apiKeyValue = "sk-live-1234567890abcdef"

func ringOf(t *testing.T, current int, versions ...int) *vault.Keyring {
	t.Helper()
	keys := map[int][]byte{}
	for _, v := range versions {
		keys[v] = deterministicKey(v)
	}
	ring, err := vault.NewKeyring(current, keys)
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

// deterministicKey gives each version a fixed key, so separate keyrings in one
// test agree about what "version 1" means.
func deterministicKey(version int) []byte {
	key := make([]byte, vault.KeySize)
	for i := range key {
		key[i] = byte(version*31 + i)
	}
	return key
}

// useVault turns the vault on for this harness: workers decrypt from it and the
// identity API exposes /v1/secrets.
func (h *harness) useVault(ring *vault.Keyring) *vaultsvc.Service {
	h.t.Helper()
	svc := vaultsvc.NewService(h.pool, ring, nil)
	h.secrets, h.vaultSvc = svc, svc
	return svc
}

// echoUpstream records the Authorization header and echoes it back, the way a
// misbehaving upstream might leak a credential into a response.
func echoUpstream(t *testing.T) (url string, headers func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"you_sent":%q}`, r.Header.Get("Authorization"))
	}))
	t.Cleanup(server.Close)
	return server.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func (h *harness) occurrences(needle string) int {
	h.t.Helper()
	total := 0
	for _, table := range []string{"node_executions", "run_events", "runs", "run_waits"} {
		var n int
		err := h.pool.QueryRow(context.Background(),
			fmt.Sprintf(`select count(*) from %s t where to_jsonb(t)::text like '%%' || $1 || '%%'`, table), needle).Scan(&n)
		if err != nil {
			h.t.Fatal(err)
		}
		total += n
	}
	return total
}

const secretWorkflow = `
apiVersion: workflow/v1
name: uses-secret
nodes:
  - id: call
    type: http
    timeout: 5s
    config:
      method: POST
      url: "URL"
      headers: {Authorization: "Bearer ${secrets.API_KEY}"}
      body: {note: hello}
`

// M12: a secret used in an http header reaches the upstream, but the stored
// node input, the stored output (the upstream echoes the header back), events
// and run output never contain it.
func TestVaultSecretReachesUpstreamButIsNeverStored(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	svc := h.useVault(ringOf(t, 1, 1))
	if err := svc.Put(ctx, h.tenant, "API_KEY", apiKeyValue, "test"); err != nil {
		t.Fatal(err)
	}
	url, headers := echoUpstream(t)
	h.mustPublish(strings.Replace(secretWorkflow, "URL", url, 1))
	h.startWorkers(1)
	runID := h.start("uses-secret", nil)
	if status := h.waitStatus(runID, 15*time.Second, "succeeded", "failed"); status != "succeeded" {
		t.Fatalf("run = %s, nodes %v", status, h.nodeStatuses(runID))
	}
	if got := headers(); len(got) != 1 || got[0] != "Bearer "+apiKeyValue {
		t.Fatalf("the upstream received %v, want the real secret", got)
	}
	if n := h.occurrences(apiKeyValue); n != 0 {
		t.Fatalf("the secret appears in %d stored row(s)", n)
	}
	var input, output string
	if err := h.pool.QueryRow(ctx, `select input::text, output::text from node_executions where run_id = $1 and node_id = 'call'`, runID).Scan(&input, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(input, "Bearer ***") {
		t.Fatalf("stored input = %s, want the secret shown as ***", input)
	}
	if !strings.Contains(output, "Bearer ***") || strings.Contains(output, apiKeyValue) {
		t.Fatalf("stored output = %s, want the echoed secret scrubbed", output)
	}
	h.checkInvariants(runID)
}

// A reference to a secret the vault does not hold fails once, permanently and
// by name; so does a workflow that uses secrets on a server with no vault.
func TestVaultMissingSecretIsAClearPermanentFailure(t *testing.T) {
	ctx := context.Background()
	for name, withVault := range map[string]bool{"secret not defined": true, "vault not configured": false} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, ctx)
			if withVault {
				h.useVault(ringOf(t, 1, 1))
			}
			url, headers := echoUpstream(t)
			h.mustPublish(strings.Replace(secretWorkflow, "URL", url, 1))
			h.startWorkers(1)
			runID := h.start("uses-secret", nil)
			h.waitStatus(runID, 15*time.Second, "failed")
			attempts := h.attempts(runID, "call")
			if len(attempts) != 1 || attempts[0]["code"] != "E_SECRET_MISSING" {
				t.Fatalf("attempts = %v", attempts)
			}
			if e := h.nodeError(runID, "call"); !strings.Contains(e["message"].(string), "API_KEY") {
				t.Fatalf("error = %v, want it to name API_KEY", e)
			}
			if len(headers()) != 0 {
				t.Fatal("the upstream was called")
			}
		})
	}
}

// Updating a secret takes effect on the next node; deleting makes uses fail.
func TestVaultUpdateAndDeleteTakeEffectImmediately(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	svc := h.useVault(ringOf(t, 1, 1))
	url, headers := echoUpstream(t)
	h.mustPublish(strings.Replace(secretWorkflow, "URL", url, 1))
	h.startWorkers(2)
	run := func() pgtype.UUID {
		id := h.start("uses-secret", nil)
		h.waitStatus(id, 15*time.Second, "succeeded", "failed")
		return id
	}
	if err := svc.Put(ctx, h.tenant, "API_KEY", "first-secret-value", "t"); err != nil {
		t.Fatal(err)
	}
	run()
	if err := svc.Put(ctx, h.tenant, "API_KEY", "second-secret-value", "t"); err != nil {
		t.Fatal(err)
	}
	run()
	if got := headers(); len(got) != 2 || got[0] != "Bearer first-secret-value" || got[1] != "Bearer second-secret-value" {
		t.Fatalf("upstream saw %v", got)
	}
	if err := svc.Delete(ctx, h.tenant, "API_KEY"); err != nil {
		t.Fatal(err)
	}
	id := run()
	if attempts := h.attempts(id, "call"); len(attempts) != 1 || attempts[0]["code"] != "E_SECRET_MISSING" {
		t.Fatalf("after delete: %v", attempts)
	}
}

// At rest the value is ciphertext bound to its organization and name.
func TestVaultCiphertextIsBoundToOrgAndName(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	svc := h.useVault(ringOf(t, 1, 1))
	other := newUUID(t)
	if err := svc.Put(ctx, h.tenant, "API_KEY", apiKeyValue, "t"); err != nil {
		t.Fatal(err)
	}
	var stored []byte
	if err := h.pool.QueryRow(ctx, `select ciphertext from secrets where org_id = $1`, h.tenant).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), apiKeyValue) {
		t.Fatal("the plaintext is stored")
	}
	// Moving the row to another organization, or under another name, breaks it.
	if _, err := h.pool.Exec(ctx, `update secrets set org_id = $1 where org_id = $2`, other, h.tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Secrets(ctx, other); err == nil || strings.Contains(err.Error(), apiKeyValue) {
		t.Fatalf("a row moved to another organization opened (err = %v)", err)
	}
	if _, err := h.pool.Exec(ctx, `update secrets set org_id = $1, name = 'OTHER_NAME' where org_id = $2`, h.tenant, other); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Secrets(ctx, h.tenant); err == nil {
		t.Fatal("a row renamed in place opened")
	}
	// Another organization sees none of it.
	if got, err := svc.Secrets(ctx, newUUID(t)); err != nil || len(got) != 0 {
		t.Fatalf("a stranger's secrets = %v, %v", got, err)
	}
}

// The API: write-only values, names listed, roles enforced, 501 without a key.
func TestVaultAPI(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.useVault(ringOf(t, 1, 1))
	base, ident := h.identityAPI()
	if _, err := ident.Bootstrap(ctx, "root@example.com", adminPassword); err != nil {
		t.Fatal(err)
	}
	root := newPerson(t, base)
	root.mustLogin("root@example.com", adminPassword)
	owner := newPerson(t, base)
	accept(t, owner, inviteToOrg(t, root, "owner@example.com", "Vault Co"), "owner-long-password", "Owner")
	_, inv := owner.call("POST", "/v1/invites", map[string]any{"email": "ed@example.com", "role": "editor"})
	editor := newPerson(t, base)
	accept(t, editor, inv["token"].(string), "editor-long-password", "Ed")

	if status, body := owner.call("PUT", "/v1/secrets/API_KEY", map[string]any{"value": apiKeyValue}); status != 200 || body["reference"] != "${secrets.API_KEY}" {
		t.Fatalf("put = %d %v", status, body)
	}
	if status, _ := editor.call("PUT", "/v1/secrets/OTHER_KEY", map[string]any{"value": apiKeyValue}); status != 403 {
		t.Fatalf("an editor writing a secret = %d, want 403", status)
	}
	if status, _ := editor.call("DELETE", "/v1/secrets/API_KEY", nil); status != 403 {
		t.Fatalf("an editor deleting a secret = %d, want 403", status)
	}
	for name, body := range map[string]map[string]any{
		"api_key":   {"value": apiKeyValue}, // lowercase name
		"BAD-NAME":  {"value": apiKeyValue},
		"SHORT_VAL": {"value": "tiny"},
	} {
		if status, _ := owner.call("PUT", "/v1/secrets/"+name, body); status != 400 {
			t.Fatalf("PUT %s = %d, want 400", name, status)
		}
	}
	status, list := editor.call("GET", "/v1/secrets", nil)
	if status != 200 || len(list["items"].([]any)) != 1 || list["items"].([]any)[0].(map[string]any)["name"] != "API_KEY" {
		t.Fatalf("list = %d %v", status, list)
	}
	if strings.Contains(toJSON(list), apiKeyValue) {
		t.Fatalf("the list leaks the value: %v", list)
	}
	// No other organization sees it.
	stranger := newPerson(t, base)
	accept(t, stranger, inviteToOrg(t, root, "x@example.com", "Elsewhere"), "stranger-long-password", "X")
	if _, other := stranger.call("GET", "/v1/secrets", nil); len(other["items"].([]any)) != 0 {
		t.Fatalf("another organization lists %v", other)
	}
	if status, _ := stranger.call("DELETE", "/v1/secrets/API_KEY", nil); status != 404 {
		t.Fatalf("another organization deleting the secret = %d, want 404", status)
	}
	if status, _ := owner.call("DELETE", "/v1/secrets/API_KEY", nil); status != 204 {
		t.Fatalf("delete = %d", status)
	}
	if status, _ := owner.call("DELETE", "/v1/secrets/API_KEY", nil); status != 404 {
		t.Fatalf("deleting twice = %d, want 404", status)
	}
	// Rotation and status are platform-admin only.
	if status, _ := owner.call("POST", "/v1/admin/vault/rotate", nil); status != 403 {
		t.Fatalf("an owner rotating the vault = %d, want 403", status)
	}
	if status, body := root.call("GET", "/v1/admin/vault", nil); status != 200 || body["current_key_version"] != float64(1) {
		t.Fatalf("status = %d %v", status, body)
	}
}

func TestVaultNotConfiguredAnswers501(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.useVault(nil) // a service without a keyring
	base, ident := h.identityAPI()
	if _, err := ident.Bootstrap(ctx, "root@example.com", adminPassword); err != nil {
		t.Fatal(err)
	}
	root := newPerson(t, base)
	root.mustLogin("root@example.com", adminPassword)
	for method, path := range map[string]string{"GET": "/v1/secrets", "PUT": "/v1/secrets/API_KEY"} {
		status, body := root.call(method, path, map[string]any{"value": apiKeyValue})
		if status != 501 || body["code"] != "vault_not_configured" {
			t.Fatalf("%s %s = %d %v, want 501 vault_not_configured", method, path, status, body)
		}
	}
}

// M12: rotating the master key re-seals every secret while the vault keeps
// serving; reads never fail, and afterwards the old key can be dropped.
func TestVaultRotationWithoutDowntime(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	old := h.useVault(ringOf(t, 1, 1))
	orgs := []pgtype.UUID{h.tenant, newUUID(t), newUUID(t)}
	for _, org := range orgs {
		for i := 0; i < 60; i++ {
			if err := old.Put(ctx, org, fmt.Sprintf("SECRET_%03d", i), fmt.Sprintf("value-%s-%03d-padding", uidString(org)[:8], i), "t"); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A new deployment holds key 2 as current and key 1 for the rows not yet moved.
	next := vaultsvc.NewService(h.pool, ringOf(t, 2, 1, 2), nil)
	if got, err := next.Secrets(ctx, h.tenant); err != nil || len(got) != 60 {
		t.Fatalf("before rotation the new keyring reads %d secrets, %v", len(got), err)
	}
	if v, _ := next.KeyVersions(ctx); v[1] != 180 || v[2] != 0 {
		t.Fatalf("key versions before = %v", v)
	}

	// Readers hammer the vault while it rotates. They all hold the new keyring:
	// rotation runs only after the new key is deployed everywhere.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var readErrors []error
	reads := 0
	for _, reader := range []*vaultsvc.Service{next, next, next} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, org := range orgs {
					got, err := reader.Secrets(ctx, org)
					mu.Lock()
					reads++
					if err != nil || len(got) != 60 {
						readErrors = append(readErrors, fmt.Errorf("read %d secrets: %v", len(got), err))
					}
					mu.Unlock()
				}
			}
		}()
	}
	result, err := next.Rotate(ctx)
	close(stop)
	wg.Wait()
	if err != nil || result.Rotated != 180 || result.Failed != 0 {
		t.Fatalf("rotate = %+v, %v", result, err)
	}
	if len(readErrors) > 0 || reads == 0 {
		t.Fatalf("%d reads, %d failed during rotation; first: %v", reads, len(readErrors), readErrors[:min(len(readErrors), 1)])
	}
	if v, _ := next.KeyVersions(ctx); v[1] != 0 || v[2] != 180 {
		t.Fatalf("key versions after = %v", v)
	}
	// Idempotent; and with key 1 removed everything is still readable.
	if again, err := next.Rotate(ctx); err != nil || again.Rotated != 0 {
		t.Fatalf("second rotation = %+v, %v", again, err)
	}
	only2 := vaultsvc.NewService(h.pool, ringOf(t, 2, 2), nil)
	got, err := only2.Secrets(ctx, orgs[1])
	if err != nil || got["SECRET_007"] != fmt.Sprintf("value-%s-007-padding", uidString(orgs[1])[:8]) {
		t.Fatalf("after dropping key 1: %v, %v", got["SECRET_007"], err)
	}
	// The old keyring can no longer read what moved to key 2.
	if _, err := old.Secrets(ctx, orgs[0]); err == nil {
		t.Fatal("a keyring without key 2 read rotated secrets")
	}
}

// Rows sealed under a key that is no longer held are reported, not retried
// forever, and do not stop the others from rotating.
func TestVaultRotationReportsUnreadableRows(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	lost := vaultsvc.NewService(h.pool, ringOf(t, 9, 9), nil)
	if err := lost.Put(ctx, h.tenant, "LOST_ONE", "sealed-with-a-lost-key", "t"); err != nil {
		t.Fatal(err)
	}
	old := vaultsvc.NewService(h.pool, ringOf(t, 1, 1), nil)
	if err := old.Put(ctx, h.tenant, "FINE_ONE", "sealed-with-key-one!!", "t"); err != nil {
		t.Fatal(err)
	}
	current := vaultsvc.NewService(h.pool, ringOf(t, 2, 1, 2), nil)
	result, err := current.Rotate(ctx)
	if err != nil || result.Rotated != 1 || result.Failed != 1 {
		t.Fatalf("rotate = %+v, %v; want 1 rotated and 1 failed", result, err)
	}
	if _, err := current.Secrets(ctx, h.tenant); err == nil || !strings.Contains(err.Error(), "LOST_ONE") || strings.Contains(err.Error(), "sealed-with") {
		t.Fatalf("reading the unreadable secret: %v (it should name the secret, never its value)", err)
	}
}

func randomKey(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, vault.KeySize)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		t.Fatal(err)
	}
	return b
}

// Rotation covers everything sealed under the master key, not just vault
// secrets: provider-connection API keys and connection credentials too. After
// the old key is dropped they must still open.
func TestVaultRotationCoversProviderKeysAndConnections(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	policy := netguard.Policy{AllowPrivate: true}
	oldRing := ringOf(t, 1, 1)

	// Seal one of each under key 1.
	if err := vaultsvc.NewService(h.pool, oldRing, nil).Put(ctx, h.tenant, "API_KEY", "vault-secret-value", "t"); err != nil {
		t.Fatal(err)
	}
	oldProviders := providers.NewService(h.pool, oldRing, policy, nil)
	if _, _, err := oldProviders.Create(ctx, h.tenant, "t", providers.CreateInput{
		Name: "compat", Provider: providers.OpenAICompatible, BaseURL: "http://127.0.0.1:1/v1", APIKey: "provider-key-123456", SkipTest: true,
	}); err != nil {
		t.Fatal(err)
	}
	oldConnections := connections.NewService(h.pool, oldRing, policy, connections.Options{})
	if _, err := oldConnections.Create(ctx, h.tenant, "t", connections.Input{
		Name: "smtp1", Kind: "smtp", SkipTest: true,
		Config: map[string]any{"host": "smtp.example.com", "username": "u"}, Secret: map[string]string{"password": "smtp-password-1"},
	}); err != nil {
		t.Fatal(err)
	}

	// Deploy key 2 (keeping key 1), then rotate.
	rotating := ringOf(t, 2, 1, 2)
	vaultNew := vaultsvc.NewService(h.pool, rotating, nil)
	if v, _ := vaultNew.KeyVersions(ctx); v[1] != 3 {
		t.Fatalf("sealed values under key 1 before rotating = %v, want 3 (secret, provider key, connection)", v)
	}
	result, err := vaultNew.Rotate(ctx)
	if err != nil || result.Rotated != 3 || result.Failed != 0 {
		t.Fatalf("rotate = %+v, %v; want 3 rotated", result, err)
	}
	if v, _ := vaultNew.KeyVersions(ctx); v[1] != 0 || v[2] != 3 {
		t.Fatalf("key versions after = %v", v)
	}

	// Drop key 1: everything still opens.
	only2 := ringOf(t, 2, 2)
	if got, err := vaultsvc.NewService(h.pool, only2, nil).Secrets(ctx, h.tenant); err != nil || got["API_KEY"] != "vault-secret-value" {
		t.Fatalf("vault secret after dropping key 1: %v, %v", got, err)
	}
	resolved, err := providers.NewService(h.pool, only2, policy, nil).Client(ctx, uuidOf(h.tenant), agent.ProviderRef{Connection: "compat"})
	if err != nil || resolved.Client == nil {
		t.Fatalf("provider key after dropping key 1: %v", err)
	}
	conn, err := connections.NewService(h.pool, only2, policy, connections.Options{}).Connection(ctx, uuidOf(h.tenant), "smtp1", "smtp")
	if err != nil || conn.Secret["password"] != "smtp-password-1" {
		t.Fatalf("connection credentials after dropping key 1: %+v, %v", conn, err)
	}
}
