//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/kartikey/workflow_service/internal/api"
	"github.com/kartikey/workflow_service/internal/server"
	"github.com/kartikey/workflow_service/internal/services/identity"
	"github.com/kartikey/workflow_service/internal/services/tasks"
)

const adminPassword = "correct horse battery"

// identityAPI serves the API with real authentication (sessions and API keys,
// no development header) and plan enforcement wired in.
func (h *harness) identityAPI() (string, *identity.Service) {
	h.t.Helper()
	ident, err := identity.NewService(h.pool, identity.Config{BcryptCost: bcrypt.MinCost})
	if err != nil {
		h.t.Fatal(err)
	}
	h.publish.Plans = ident.Plans()
	h.flow.Allowance = ident.Plans()
	services := &api.Services{
		Pool: h.pool, Workflows: h.publish, Runs: h.flow, Executors: h.registry, Tasks: tasks.NewService(h.flow),
		Admin: h.flow, Identity: ident, PublicURL: "http://app.test", Auth: ident.Authenticator(),
	}
	if h.vaultSvc != nil {
		services.Vault = h.vaultSvc
	}
	srv, err := server.New(server.Config{Address: "127.0.0.1:0", API: services}, h.pool, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	h.t.Cleanup(ts.Close)
	return ts.URL, ident
}

// person is one signed-in (or key-authenticated) caller with its own cookie jar.
type person struct {
	t      *testing.T
	base   string
	client *http.Client
	bearer string
}

func newPerson(t *testing.T, base string) *person {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &person{t: t, base: base, client: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (p *person) do(method, path string, body any, headers ...string) (int, map[string]any, http.Header) {
	p.t.Helper()
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	req, err := http.NewRequest(method, p.base+path, bytes.NewReader(data))
	if err != nil {
		p.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if p.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+p.bearer)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := p.client.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	if decoded == nil {
		decoded = map[string]any{"_raw": string(raw)}
	}
	return res.StatusCode, decoded, res.Header
}

func (p *person) call(method, path string, body any, headers ...string) (int, map[string]any) {
	p.t.Helper()
	status, decoded, _ := p.do(method, path, body, headers...)
	return status, decoded
}

func (p *person) login(email, password string) (int, map[string]any) {
	p.t.Helper()
	return p.call("POST", "/be/v1/auth/login", map[string]any{"email": email, "password": password})
}

func (p *person) mustLogin(email, password string) {
	p.t.Helper()
	if status, body := p.login(email, password); status != 200 {
		p.t.Fatalf("login %s = %d %v", email, status, body)
	}
}

func (p *person) orgID() string {
	p.t.Helper()
	status, me := p.call("GET", "/be/v1/auth/me", nil)
	if status != 200 {
		p.t.Fatalf("me = %d %v", status, me)
	}
	return me["organization"].(map[string]any)["id"].(string)
}

// invite has admin invite email to a new organization and returns the token.
func inviteToOrg(t *testing.T, admin *person, email, org string) string {
	t.Helper()
	status, body := admin.call("POST", "/be/v1/admin/invites", map[string]any{"email": email, "organization_name": org})
	if status != 201 {
		t.Fatalf("platform invite = %d %v", status, body)
	}
	return body["token"].(string)
}

func accept(t *testing.T, p *person, token, password, name string) (int, map[string]any) {
	t.Helper()
	return p.call("POST", "/be/v1/auth/accept-invite", map[string]any{"token": token, "password": password, "name": name})
}

const planWorkflow = `
apiVersion: workflow/v1
name: PLAN_NAME
nodes:
  - {id: a, type: noop, config: {}}
`

func workflowSource(name string, steps int) string {
	var b strings.Builder
	b.WriteString("apiVersion: workflow/v1\nname: " + name + "\nnodes:\n  - {id: n0, type: noop, config: {}}\n")
	for i := 1; i < steps; i++ {
		b.WriteString("  - {id: n" + string(rune('0'+i)) + ", type: noop, needs: [n" + string(rune('0'+i-1)) + "], config: {}}\n")
	}
	return b.String()
}

func (p *person) publish(source string) (int, map[string]any) {
	p.t.Helper()
	req, _ := http.NewRequest("POST", p.base+"/be/v1/workflows", strings.NewReader(source))
	req.Header.Set("Content-Type", "application/x-yaml")
	if p.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+p.bearer)
	}
	res, err := p.client.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return res.StatusCode, decoded
}

// M11: bootstrap an admin, invite a user to a new organization, accept, invite
// a member, give someone a second organization and switch between them.
func TestIdentityInviteAcceptLoginSwitch(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	base, ident := h.identityAPI()

	if created, err := ident.Bootstrap(ctx, "root@example.com", adminPassword); err != nil || !created {
		t.Fatalf("bootstrap = %v, %v", created, err)
	}
	if created, err := ident.Bootstrap(ctx, "other@example.com", adminPassword); err != nil || created {
		t.Fatalf("a second bootstrap must do nothing: %v, %v", created, err)
	}
	root := newPerson(t, base)
	root.mustLogin("root@example.com", adminPassword)
	if _, me := root.call("GET", "/be/v1/auth/me", nil); me["user"].(map[string]any)["platform_admin"] != true {
		t.Fatalf("bootstrap user is not a platform admin: %v", me)
	}

	token := inviteToOrg(t, root, "Alice@Example.com", "Acme Inc")
	anon := newPerson(t, base)
	status, preview := anon.call("GET", "/be/v1/auth/invite?token="+token, nil)
	if status != 200 || preview["email"] != "alice@example.com" || preview["organization_name"] != "Acme Inc" || preview["has_account"] != false {
		t.Fatalf("preview = %d %v", status, preview)
	}
	alice := newPerson(t, base)
	if status, body := accept(t, alice, token, "short", "Alice"); status != 400 {
		t.Fatalf("a short password = %d %v, want 400", status, body)
	}
	status, body, header := alice.do("POST", "/be/v1/auth/accept-invite", map[string]any{"token": token, "password": "alice-long-password", "name": "Alice"})
	if status != 201 || body["organization"].(map[string]any)["role"] != "owner" || body["organization"].(map[string]any)["name"] != "Acme Inc" {
		t.Fatalf("accept = %d %v", status, body)
	}
	cookie := header.Get("Set-Cookie")
	if !strings.Contains(cookie, "wf_session=") || !strings.Contains(cookie, "HttpOnly") || !strings.Contains(cookie, "SameSite=Lax") {
		t.Fatalf("session cookie = %q, want HttpOnly and SameSite=Lax", cookie)
	}
	if strings.Contains(cookie, "alice-long-password") {
		t.Fatal("cookie leaks the password")
	}
	if status, body := accept(t, newPerson(t, base), token, "alice-long-password", "Alice"); status != 410 {
		t.Fatalf("reusing an invite = %d %v, want 410", status, body)
	}
	if status, _ := anon.call("GET", "/be/v1/auth/invite?token=nope", nil); status != 410 {
		t.Fatalf("an unknown invite token = %d, want 410", status)
	}

	// Alice (owner) invites Bob as editor; Bob accepts.
	status, inv := alice.call("POST", "/be/v1/invites", map[string]any{"email": "bob@example.com", "role": "editor"})
	if status != 201 || !strings.HasPrefix(inv["accept_url"].(string), "http://app.test/ui/accept-invite?token=") {
		t.Fatalf("invite = %d %v", status, inv)
	}
	bob := newPerson(t, base)
	if status, body := accept(t, bob, inv["token"].(string), "bob-long-password", "Bob"); status != 201 || body["organization"].(map[string]any)["role"] != "editor" {
		t.Fatalf("bob accepts = %d %v", status, body)
	}
	if _, list := alice.call("GET", "/be/v1/invites", nil); len(list["items"].([]any)) != 0 {
		t.Fatalf("accepted invites must not stay pending: %v", list)
	}
	acme := alice.orgID()

	// A platform invite gives Bob a second organization; he already has an
	// account, so accepting needs his password.
	second := inviteToOrg(t, root, "bob@example.com", "Globex")
	if status, body := accept(t, newPerson(t, base), second, "not-bobs-password", ""); status != 401 {
		t.Fatalf("accepting for an existing account with the wrong password = %d %v, want 401", status, body)
	}
	if status, body := accept(t, newPerson(t, base), second, "bob-long-password", ""); status != 201 {
		t.Fatalf("accepting with the account's password = %d %v", status, body)
	}
	bob2 := newPerson(t, base)
	bob2.mustLogin("bob@example.com", "bob-long-password")
	_, me := bob2.call("GET", "/be/v1/auth/me", nil)
	orgs := me["organizations"].([]any)
	if len(orgs) != 2 {
		t.Fatalf("bob's organizations = %v, want 2", orgs)
	}
	if status, body := bob2.call("POST", "/be/v1/auth/switch-org", map[string]any{"organization_id": acme}); status != 200 || body["organization"].(map[string]any)["role"] != "editor" {
		t.Fatalf("switch = %d %v", status, body)
	}
	if got := bob2.orgID(); got != acme {
		t.Fatalf("current organization = %s, want %s", got, acme)
	}
	_, members := bob2.call("GET", "/be/v1/members", nil)
	if len(members["items"].([]any)) != 2 {
		t.Fatalf("Acme members = %v, want alice and bob", members)
	}
	if status, _ := bob2.call("POST", "/be/v1/auth/switch-org", map[string]any{"organization_id": "00000000-0000-7000-8000-000000000009"}); status != 404 {
		t.Fatalf("switching to a foreign organization = %d, want 404", status)
	}
}

// M11: viewers cannot publish or run, API keys are bounded by the creator's
// role, and removing a member or revoking a key takes effect immediately.
func TestIdentityRolesKeysAndRevocation(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	base, ident := h.identityAPI()
	if _, err := ident.Bootstrap(ctx, "root@example.com", adminPassword); err != nil {
		t.Fatal(err)
	}
	root := newPerson(t, base)
	root.mustLogin("root@example.com", adminPassword)
	// Give the organization a plan with room for the scenario.
	owner := newPerson(t, base)
	accept(t, owner, inviteToOrg(t, root, "owner@example.com", "Roles Co"), "owner-long-password", "Owner")
	orgID := owner.orgID()
	if status, body := root.call("PUT", "/be/v1/admin/organizations/"+orgID+"/plan", map[string]any{"plan": "pro"}); status != 200 {
		t.Fatalf("set plan = %d %v", status, body)
	}

	join := func(role, email string) *person {
		_, inv := owner.call("POST", "/be/v1/invites", map[string]any{"email": email, "role": role})
		p := newPerson(t, base)
		if status, body := accept(t, p, inv["token"].(string), "member-long-password", role); status != 201 {
			t.Fatalf("%s joins = %d %v", role, status, body)
		}
		return p
	}
	viewer, editor, admin := join("viewer", "v@example.com"), join("editor", "e@example.com"), join("admin", "a@example.com")

	if status, _ := viewer.publish(workflowSource("w1", 1)); status != 403 {
		t.Fatalf("a viewer publishing = %d, want 403", status)
	}
	if status, _ := viewer.call("POST", "/be/v1/runs", map[string]any{"workflow": "w1"}); status != 403 {
		t.Fatalf("a viewer starting a run = %d, want 403", status)
	}
	if status, _ := viewer.call("GET", "/be/v1/workflows", nil); status != 200 {
		t.Fatalf("a viewer listing workflows = %d, want 200", status)
	}
	if status, body := editor.publish(workflowSource("w1", 1)); status != 201 {
		t.Fatalf("an editor publishing = %d %v", status, body)
	}
	if status, _ := editor.call("POST", "/be/v1/runs", map[string]any{"workflow": "w1"}); status != 201 {
		t.Fatalf("an editor starting a run = %d", status)
	}
	if status, _ := editor.call("GET", "/be/v1/api-keys", nil); status != 403 {
		t.Fatalf("an editor listing API keys = %d, want 403", status)
	}
	if status, _ := editor.call("POST", "/be/v1/invites", map[string]any{"email": "x@example.com"}); status != 403 {
		t.Fatalf("an editor inviting = %d, want 403", status)
	}
	if status, _ := editor.call("GET", "/be/v1/admin/organizations", nil); status != 403 {
		t.Fatalf("a non-platform-admin listing organizations = %d, want 403", status)
	}

	// Role ceilings: an admin cannot mint owners or touch the owner.
	if status, _ := admin.call("POST", "/be/v1/invites", map[string]any{"email": "z@example.com", "role": "owner"}); status != 403 {
		t.Fatalf("an admin inviting an owner = %d, want 403", status)
	}
	_, members := owner.call("GET", "/be/v1/members", nil)
	var ownerID, editorID string
	for _, m := range members["items"].([]any) {
		switch m.(map[string]any)["email"] {
		case "owner@example.com":
			ownerID = m.(map[string]any)["user_id"].(string)
		case "e@example.com":
			editorID = m.(map[string]any)["user_id"].(string)
		}
	}
	if status, _ := admin.call("PATCH", "/be/v1/members/"+ownerID, map[string]any{"role": "viewer"}); status != 403 {
		t.Fatalf("an admin demoting the owner = %d, want 403", status)
	}
	if status, _ := admin.call("DELETE", "/be/v1/members/"+ownerID, nil); status != 403 {
		t.Fatalf("an admin removing the owner = %d, want 403", status)
	}
	if status, body := owner.call("PATCH", "/be/v1/members/"+ownerID, map[string]any{"role": "admin"}); status != 409 {
		t.Fatalf("demoting the last owner = %d %v, want 409", status, body)
	}
	if status, body := owner.call("DELETE", "/be/v1/members/"+ownerID, nil); status != 409 {
		t.Fatalf("the last owner leaving = %d %v, want 409", status, body)
	}

	// API keys: bearer auth works, is bounded by the creator's role, never owner.
	if status, _ := admin.call("POST", "/be/v1/api-keys", map[string]any{"name": "root-key", "role": "owner"}); status != 400 {
		t.Fatalf("an owner key = %d, want 400", status)
	}
	status, key := admin.call("POST", "/be/v1/api-keys", map[string]any{"name": "ci", "role": "editor"})
	if status != 201 || !strings.HasPrefix(key["token"].(string), "wfk_") {
		t.Fatalf("create key = %d %v", status, key)
	}
	ci := newPerson(t, base)
	ci.bearer = key["token"].(string)
	if status, body := ci.publish(workflowSource("by-key", 1)); status != 201 {
		t.Fatalf("publish with a key = %d %v", status, body)
	}
	if status, _ := ci.call("GET", "/be/v1/api-keys", nil); status != 403 {
		t.Fatalf("an editor key managing keys = %d, want 403", status)
	}
	if _, list := admin.call("GET", "/be/v1/api-keys", nil); strings.Contains(toJSON(list), key["token"].(string)) {
		t.Fatalf("the key list leaks the token: %v", list)
	}
	if status, _ := admin.call("DELETE", "/be/v1/api-keys/"+key["id"].(string), nil); status != 204 {
		t.Fatalf("revoke = %d", status)
	}
	if status, _ := ci.call("GET", "/be/v1/workflows", nil); status != 401 {
		t.Fatalf("a revoked key = %d, want 401", status)
	}
	bad := newPerson(t, base)
	bad.bearer = "wfk_not-a-real-key"
	if status, _ := bad.call("GET", "/be/v1/workflows", nil); status != 401 {
		t.Fatalf("an unknown key = %d, want 401", status)
	}

	// Removing a member ends their access at once, even with a live session.
	if status, _ := editor.call("GET", "/be/v1/workflows", nil); status != 200 {
		t.Fatalf("editor before removal = %d", status)
	}
	if status, _ := admin.call("DELETE", "/be/v1/members/"+editorID, nil); status != 204 {
		t.Fatalf("remove member = %d", status)
	}
	if status, _ := editor.call("GET", "/be/v1/workflows", nil); status != 401 {
		t.Fatalf("a removed member = %d, want 401", status)
	}

	// Logging out ends the session.
	if status, _ := viewer.call("POST", "/be/v1/auth/logout", nil); status != 204 {
		t.Fatalf("logout = %d", status)
	}
	if status, _ := viewer.call("GET", "/be/v1/workflows", nil); status != 401 {
		t.Fatalf("after logout = %d, want 401", status)
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// Cookie sessions reject a state-changing request from another origin, and
// development-header auth is not available when only identity is configured.
func TestIdentityRejectsCrossOriginAndDevHeader(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	base, ident := h.identityAPI()
	if _, err := ident.Bootstrap(ctx, "root@example.com", adminPassword); err != nil {
		t.Fatal(err)
	}
	root := newPerson(t, base)
	root.mustLogin("root@example.com", adminPassword)
	host := strings.TrimPrefix(base, "http://")
	if status, _ := root.call("POST", "/be/v1/auth/switch-org", map[string]any{"organization_id": root.orgID()}, "Origin", "http://evil.example"); status != 401 {
		t.Fatalf("a cross-origin POST with a session = %d, want 401", status)
	}
	if status, _ := root.call("POST", "/be/v1/auth/switch-org", map[string]any{"organization_id": root.orgID()}, "Origin", "http://"+host); status != 200 {
		t.Fatalf("a same-origin POST = %d, want 200", status)
	}
	anon := newPerson(t, base)
	if status, _ := anon.call("GET", "/be/v1/workflows", nil, "X-Tenant-ID", uidString(h.tenant)); status != 401 {
		t.Fatalf("the development header on an identity-only server = %d, want 401", status)
	}
}

// Sign-in does not reveal which emails exist, and repeated failures lock the
// account even for the right password.
func TestIdentityLoginHardening(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	base, ident := h.identityAPI()
	if _, err := ident.Bootstrap(ctx, "root@example.com", adminPassword); err != nil {
		t.Fatal(err)
	}
	p := newPerson(t, base)
	s1, b1 := p.login("nobody@example.com", "whatever-long-password")
	s2, b2 := p.login("root@example.com", "the-wrong-password")
	if s1 != 401 || s2 != 401 || b1["code"] != "invalid_credentials" || b1["detail"] != b2["detail"] || b1["title"] != b2["title"] {
		t.Fatalf("unknown email %d %v vs wrong password %d %v must look the same", s1, b1, s2, b2)
	}
	for i := 0; i < 4; i++ { // five failures in a row in total
		p.login("root@example.com", "the-wrong-password")
	}
	if status, body := p.login("root@example.com", adminPassword); status != 429 || body["code"] != "account_locked" {
		t.Fatalf("login while locked = %d %v, want 429 account_locked", status, body)
	}
}

func uidString(id interface{ String() string }) string { return id.String() }

// M11: the free plan allows one workflow of five steps and twenty runs a
// month; the platform admin can move an organization to another plan.
func TestIdentityPlanLimits(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	base, ident := h.identityAPI()
	if _, err := ident.Bootstrap(ctx, "root@example.com", adminPassword); err != nil {
		t.Fatal(err)
	}
	root := newPerson(t, base)
	root.mustLogin("root@example.com", adminPassword)
	owner := newPerson(t, base)
	accept(t, owner, inviteToOrg(t, root, "owner@example.com", "Free Co"), "owner-long-password", "Owner")

	if status, body := owner.publish(workflowSource("one", 6)); status != 402 || !strings.Contains(toJSON(body), "steps per workflow") {
		t.Fatalf("six steps on the free plan = %d %v, want 402", status, body)
	}
	if status, body := owner.publish(workflowSource("one", 5)); status != 201 {
		t.Fatalf("five steps = %d %v", status, body)
	}
	if status, body := owner.publish(workflowSource("one", 4)); status != 201 {
		t.Fatalf("a new version of the same workflow must not count as a second workflow: %d %v", status, body)
	}
	if status, body := owner.publish(workflowSource("two", 1)); status != 402 || !strings.Contains(toJSON(body), "workflows") {
		t.Fatalf("a second workflow = %d %v, want 402", status, body)
	}

	for i := 0; i < 20; i++ {
		if status, body := owner.call("POST", "/be/v1/runs", map[string]any{"workflow": "one"}); status != 201 {
			t.Fatalf("run %d = %d %v", i+1, status, body)
		}
	}
	status, body := owner.call("POST", "/be/v1/runs", map[string]any{"workflow": "one"})
	if status != 402 || !strings.Contains(toJSON(body), "runs per month") {
		t.Fatalf("the 21st run = %d %v, want 402", status, body)
	}
	_, usage := owner.call("GET", "/be/v1/usage", nil)
	if usage["plan"].(map[string]any)["id"] != "free" || usage["usage"].(map[string]any)["runs_this_month"] != float64(20) || usage["usage"].(map[string]any)["workflows"] != float64(1) {
		t.Fatalf("usage = %v", usage)
	}

	// Upgrading lifts the limit.
	if status, _ := root.call("PUT", "/be/v1/admin/organizations/"+owner.orgID()+"/plan", map[string]any{"plan": "pro"}); status != 200 {
		t.Fatal("could not move the organization to pro")
	}
	if status, body := owner.call("POST", "/be/v1/runs", map[string]any{"workflow": "one"}); status != 201 {
		t.Fatalf("a run after upgrading = %d %v", status, body)
	}
	if status, _ := root.call("PUT", "/be/v1/admin/organizations/"+owner.orgID()+"/plan", map[string]any{"plan": "nope"}); status != 400 {
		t.Fatalf("an unknown plan = %d, want 400", status)
	}
}

// Concurrent starts cannot overshoot the monthly allowance, and an idempotent
// replay is not charged twice.
func TestIdentityRunAllowanceHoldsUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	base, ident := h.identityAPI()
	if _, err := ident.Bootstrap(ctx, "root@example.com", adminPassword); err != nil {
		t.Fatal(err)
	}
	root := newPerson(t, base)
	root.mustLogin("root@example.com", adminPassword)
	owner := newPerson(t, base)
	accept(t, owner, inviteToOrg(t, root, "owner@example.com", "Burst Co"), "owner-long-password", "Owner")
	owner.publish(workflowSource("burst", 1))

	// A replayed idempotent start costs one run, not two.
	for i := 0; i < 3; i++ {
		if status, _ := owner.call("POST", "/be/v1/runs", map[string]any{"workflow": "burst"}, "Idempotency-Key", "same"); status != 201 && status != 200 {
			t.Fatalf("idempotent start %d = %d", i, status)
		}
	}
	var created, refused atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, _ := owner.call("POST", "/be/v1/runs", map[string]any{"workflow": "burst"})
			switch status {
			case 201:
				created.Add(1)
			case 402:
				refused.Add(1)
			}
		}()
	}
	wg.Wait()
	if created.Load() != 19 || refused.Load() != 21 {
		t.Fatalf("created %d and refused %d, want exactly 19 and 21 (the idempotent run used 1 of 20)", created.Load(), refused.Load())
	}
}

// One organization's API key reads nothing of another's, with real credentials.
func TestIdentityTenantIsolationWithKeys(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	base, ident := h.identityAPI()
	if _, err := ident.Bootstrap(ctx, "root@example.com", adminPassword); err != nil {
		t.Fatal(err)
	}
	root := newPerson(t, base)
	root.mustLogin("root@example.com", adminPassword)
	a, b := newPerson(t, base), newPerson(t, base)
	accept(t, a, inviteToOrg(t, root, "a@example.com", "Org A"), "a-long-password-1", "A")
	accept(t, b, inviteToOrg(t, root, "b@example.com", "Org B"), "b-long-password-1", "B")
	a.publish(workflowSource("secret", 1))
	_, run := a.call("POST", "/be/v1/runs", map[string]any{"workflow": "secret"})

	_, k := b.call("POST", "/be/v1/api-keys", map[string]any{"name": "b-key", "role": "admin"})
	bk := newPerson(t, base)
	bk.bearer = k["token"].(string)
	if status, _ := bk.call("GET", "/be/v1/runs/"+run["id"].(string), nil); status != 404 {
		t.Fatalf("org B's key reading org A's run = %d, want 404", status)
	}
	if status, _ := bk.call("GET", "/be/v1/workflows/secret", nil); status != 404 {
		t.Fatalf("org B's key reading org A's workflow = %d, want 404", status)
	}
	_, list := bk.call("GET", "/be/v1/runs", nil)
	if len(list["items"].([]any)) != 0 {
		t.Fatalf("org B lists %v", list)
	}
	if status, _ := bk.call("POST", "/be/v1/runs/"+run["id"].(string)+"/cancel", nil); status != 404 {
		t.Fatalf("org B's key cancelling org A's run = %d, want 404", status)
	}
}

// Two API replicas may start together on a fresh database. Exactly one creates
// the administrator, and both starts must succeed.
func TestConcurrentAdministratorBootstrap(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	_, identity := h.identityAPI()
	type outcome struct {
		created bool
		err     error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			created, err := identity.Bootstrap(ctx, "root@example.com", adminPassword)
			results <- outcome{created, err}
		}()
	}
	close(start)
	created := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.created {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("created %d administrators", created)
	}
}
