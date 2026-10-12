//go:build integration

package integration_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kartikey/workflow_service/internal/config"
)

// smtpSink is a plaintext SMTP server for tests. It checks AUTH PLAIN, records
// what it accepts, and can be told to reject recipients.
type smtpSink struct {
	ln         net.Listener
	user, pass string
	rcptReply  string // e.g. "550 no such user"; empty accepts

	mu       sync.Mutex
	messages []string
	froms    []string
}

func startSMTPSink(t *testing.T, user, pass string) *smtpSink {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &smtpSink{ln: ln, user: user, pass: pass}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s
}

func (s *smtpSink) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *smtpSink) received() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.messages...)
}

func (s *smtpSink) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	say := func(format string, args ...any) { fmt.Fprintf(conn, format+"\r\n", args...) }
	say("220 sink ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"):
			say("250-sink")
			if s.user != "" {
				say("250-AUTH PLAIN")
			}
			say("250 8BITMIME")
		case strings.HasPrefix(upper, "AUTH PLAIN"):
			raw, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(line[len("AUTH PLAIN"):]))
			if string(raw) == "\x00"+s.user+"\x00"+s.pass {
				say("235 ok")
			} else {
				say("535 5.7.8 authentication failed for %s", strings.ReplaceAll(string(raw), "\x00", " "))
			}
		case strings.HasPrefix(upper, "MAIL FROM:"):
			s.mu.Lock()
			s.froms = append(s.froms, line)
			s.mu.Unlock()
			say("250 ok")
		case strings.HasPrefix(upper, "RCPT TO:"):
			if s.rcptReply != "" {
				say("%s", s.rcptReply)
			} else {
				say("250 ok")
			}
		case upper == "DATA":
			say("354 go")
			var data strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				data.WriteString(l)
			}
			s.mu.Lock()
			s.messages = append(s.messages, data.String())
			s.mu.Unlock()
			say("250 queued")
		case upper == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

// resendSink fakes the Resend API: it records requests and answers per call.
type resendSink struct {
	*httptest.Server
	mu       sync.Mutex
	keys     []string // Idempotency-Key per /emails call
	bodies   []string
	failures int32 // answer 503 for this many /emails calls first
	calls    atomic.Int32
}

func startResend(t *testing.T) *resendSink {
	t.Helper()
	r := &resendSink{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		auth := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.URL.Path == "/domains" && auth == "restricted-sending-key":
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"name":"restricted_api_key","message":"This API key is restricted to only send emails"}`)
		case auth != "re_good_key_123456":
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"name":"validation_error","message":"API key is invalid"}`)
		case req.URL.Path == "/domains":
			_, _ = io.WriteString(w, `{"data":[]}`)
		case req.URL.Path == "/emails":
			body, _ := io.ReadAll(req.Body)
			r.mu.Lock()
			r.keys = append(r.keys, req.Header.Get("Idempotency-Key"))
			r.bodies = append(r.bodies, string(body))
			r.mu.Unlock()
			if int32(r.calls.Add(1)) <= r.failures {
				w.WriteHeader(503)
				_, _ = io.WriteString(w, `{"message":"try again"}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"email_123"}`)
		}
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *resendSink) sent() (keys, bodies []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.keys...), append([]string(nil), r.bodies...)
}

func publishWorkflow(t *testing.T, p *person, name, nodeConfig string) {
	t.Helper()
	source := "apiVersion: workflow/v1\nname: " + name + "\nnodes:\n  - id: mail\n    type: email.send\n    retry: {max_attempts: 3, backoff: fixed, initial: 10ms}\n    config: " + nodeConfig + "\n"
	if status, body := p.publish(source); status != 201 {
		t.Fatalf("publish %s = %d %v", name, status, body)
	}
}

func runToEnd(t *testing.T, p *person, workflow string) (string, map[string]any) {
	t.Helper()
	_, started := p.call("POST", "/be/v1/runs", map[string]any{"workflow": workflow})
	run := p.waitRun(started["id"].(string))
	return started["id"].(string), run
}

// M14: connections are tested before they are saved, credentials are
// write-only, and roles and validation are enforced.
func TestConnectionsAPI(t *testing.T) {
	ctx := context.Background()
	resend := startResend(t)
	s := newStack(t, ctx, func(c *config.Config) { c.Services.ResendBaseURL = resend.URL })
	owner := s.owner(t, "Conn Co")
	_, inv := owner.call("POST", "/be/v1/invites", map[string]any{"email": "ed@example.com", "role": "editor"})
	editor := newPerson(t, s.base)
	accept(t, editor, inv["token"].(string), "editor-long-password", "Ed")
	sink := startSMTPSink(t, "mailer", "sink-password-1")

	smtp := func(name, password string) map[string]any {
		return map[string]any{"name": name, "kind": "smtp",
			"config": map[string]any{"host": "127.0.0.1", "port": sink.port(), "tls": "none", "username": "mailer", "from": "Ops <ops@example.com>"},
			"secret": map[string]any{"password": password}}
	}
	// A bad password fails the test, says why without echoing it, and saves nothing.
	status, body := owner.call("POST", "/be/v1/connections", smtp("team-smtp", "wrong-password-1"))
	if status != 422 || body["code"] != "connection_test_failed" || strings.Contains(toJSON(body), "wrong-password-1") {
		t.Fatalf("a bad password = %d %v", status, body)
	}
	if _, list := owner.call("GET", "/be/v1/connections", nil); len(list["items"].([]any)) != 0 {
		t.Fatalf("a connection that failed its test was saved: %v", list)
	}
	created := owner.mustOK("POST", "/be/v1/connections", smtp("team-smtp", "sink-password-1"), 201)
	if created["kind"] != "smtp" || created["secret_hint"] != "…d-1" && created["secret_hint"] != "…rd-1" || strings.Contains(toJSON(created), "sink-password-1") {
		t.Fatalf("created = %v", created)
	}
	if created["config"].(map[string]any)["from"] != "Ops <ops@example.com>" {
		t.Fatalf("config = %v", created["config"])
	}
	if status, _ := owner.call("POST", "/be/v1/connections", smtp("team-smtp", "sink-password-1")); status != 409 {
		t.Fatalf("a duplicate name = %d, want 409", status)
	}
	owner.mustOK("POST", "/be/v1/connections/team-smtp/test", nil, 200)

	// Credentials are write-only; editors can see connections but not manage them.
	_, got := editor.call("GET", "/be/v1/connections/team-smtp", nil)
	if strings.Contains(toJSON(got), "sink-password-1") || got["name"] != "team-smtp" {
		t.Fatalf("an editor reads %v", got)
	}
	if status, _ := editor.call("POST", "/be/v1/connections", smtp("by-editor", "sink-password-1")); status != 403 {
		t.Fatalf("an editor creating a connection = %d, want 403", status)
	}
	if status, _ := editor.call("DELETE", "/be/v1/connections/team-smtp", nil); status != 403 {
		t.Fatalf("an editor deleting a connection = %d, want 403", status)
	}

	// Rotating the password: omit the secret to keep it, send one to replace it.
	update := smtp("ignored", "")
	delete(update, "secret")
	update["config"].(map[string]any)["from"] = "Alerts <alerts@example.com>"
	updated := owner.mustOK("PUT", "/be/v1/connections/team-smtp", update, 200)
	if updated["config"].(map[string]any)["from"] != "Alerts <alerts@example.com>" || updated["secret_hint"] == nil {
		t.Fatalf("keeping the password: %v", updated)
	}
	sink.pass = "rotated-password-9"
	if status, _ := owner.call("PUT", "/be/v1/connections/team-smtp", update); status != 422 {
		t.Fatalf("the old password no longer works, so the update must fail its test: %d", status)
	}
	withNew := smtp("x", "rotated-password-9")
	if status, _ := owner.call("PUT", "/be/v1/connections/team-smtp", withNew); status != 200 {
		t.Fatalf("updating with the new password = %d", status)
	}

	// Validation: unknown kinds and settings, credentials in the wrong place.
	for name, mutate := range map[string]func(map[string]any){
		"unknown kind":            func(b map[string]any) { b["kind"] = "carrier-pigeon" },
		"unavailable oauth kind":  func(b map[string]any) { b["kind"] = "gmail" },
		"missing host":            func(b map[string]any) { delete(b["config"].(map[string]any), "host") },
		"bad port":                func(b map[string]any) { b["config"].(map[string]any)["port"] = 70000 },
		"bad tls":                 func(b map[string]any) { b["config"].(map[string]any)["tls"] = "maybe" },
		"unknown setting":         func(b map[string]any) { b["config"].(map[string]any)["colour"] = "blue" },
		"password inside config":  func(b map[string]any) { b["config"].(map[string]any)["password"] = "oops-oops-oops" },
		"bad sender":              func(b map[string]any) { b["config"].(map[string]any)["from"] = "not an address" },
		"username without secret": func(b map[string]any) { delete(b, "secret") },
		"bad name":                func(b map[string]any) { b["name"] = "Not Valid" },
	} {
		b := smtp("candidate", "rotated-password-9")
		b["skip_test"] = true
		mutate(b)
		if status, resp := owner.call("POST", "/be/v1/connections", b); status != 400 {
			t.Errorf("%s: status %d %v, want 400", name, status, resp)
		}
	}
	// Resend: a bad key is rejected, a sending-only key is accepted.
	resendBody := func(key string) map[string]any {
		return map[string]any{"name": "resend-" + key[:3], "kind": "resend", "config": map[string]any{"from": "ops@example.com"}, "secret": map[string]any{"api_key": key}}
	}
	if status, body := owner.call("POST", "/be/v1/connections/test", resendBody("re_bad_key_000000")); status != 422 || !strings.Contains(body["detail"].(string), "rejected the API key") {
		t.Fatalf("a bad Resend key = %d %v", status, body)
	}
	owner.mustOK("POST", "/be/v1/connections/test", resendBody("restricted-sending-key"), 200)
	owner.mustOK("POST", "/be/v1/connections", resendBody("re_good_key_123456"), 201)

	// The editor's view of what can be created.
	_, kinds := editor.call("GET", "/be/v1/integrations", nil)
	got2 := []string{}
	for _, k := range kinds["items"].([]any) {
		got2 = append(got2, k.(map[string]any)["kind"].(string))
	}
	if strings.Join(got2, ",") != "smtp,resend,gmail,gdrive,gdocs,gsheets,gcalendar,dropbox,onedrive,postgres,mysql,mongo,slack,notion,mcp" {
		t.Fatalf("available kinds = %v", got2)
	}
	if status, _ := owner.call("DELETE", "/be/v1/connections/team-smtp", nil); status != 204 {
		t.Fatalf("delete = %d", status)
	}
	if status, _ := owner.call("GET", "/be/v1/connections/team-smtp", nil); status != 404 {
		t.Fatalf("after delete = %d, want 404", status)
	}
	// Another organization sees none of it.
	other := s.owner(t, "Elsewhere Co")
	if _, list := other.call("GET", "/be/v1/connections", nil); len(list["items"].([]any)) != 0 {
		t.Fatalf("another organization lists %v", list)
	}
}

// M14: email.send over SMTP with a saved connection. The message is correct, the
// Bcc recipient is not in the headers, the Message-ID comes from the node's
// stable key, and the password is stored nowhere.
func TestEmailSendOverSMTPWithAConnection(t *testing.T) {
	ctx := context.Background()
	s := newStack(t, ctx)
	owner := s.owner(t, "Mail Co")
	sink := startSMTPSink(t, "mailer", "sink-password-1")
	owner.mustOK("POST", "/be/v1/connections", map[string]any{"name": "team-smtp", "kind": "smtp",
		"config": map[string]any{"host": "127.0.0.1", "port": sink.port(), "tls": "none", "username": "mailer", "from": "Ops <ops@example.com>"},
		"secret": map[string]any{"password": "sink-password-1"}}, 201)

	publishWorkflow(t, owner, "mail-it", `{connection: team-smtp, to: [a@example.com], cc: "c@example.com", bcc: [hidden@example.com], subject: "Order ${run.id} shipped", text: "Hi there", html: "<p>Hi there</p>"}`)
	id, run := runToEnd(t, owner, "mail-it")
	if run["status"] != "succeeded" {
		t.Fatalf("run = %v", run)
	}
	got := sink.received()
	if len(got) != 1 {
		t.Fatalf("the relay received %d messages", len(got))
	}
	msg := got[0]
	for _, want := range []string{"From: \"Ops\" <ops@example.com>", "To: <a@example.com>", "Cc: <c@example.com>", "Subject: Order " + id + " shipped",
		"multipart/alternative", "Hi there", "<p>Hi there</p>"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "hidden@example.com") {
		t.Fatal("the Bcc recipient appears in the message")
	}
	var output map[string]any
	if err := s.h.pool.QueryRow(ctx, `select output from node_executions where run_id = $1 and node_id = 'mail'`, mustParse(t, id)).Scan(&output); err != nil {
		t.Fatal(err)
	}
	if output["provider"] != "smtp" || output["recipients"] != float64(3) || !strings.HasSuffix(output["message_id"].(string), "@example.com") || !strings.Contains(msg, "Message-ID: <"+output["message_id"].(string)+">") {
		t.Fatalf("output = %v", output)
	}
	if n := s.h.occurrences("sink-password-1"); n != 0 {
		t.Fatalf("the SMTP password is stored in %d row(s)", n)
	}
}

// Failures follow the retry model, and credentials in an error never get stored.
func TestEmailSendFailureClassification(t *testing.T) {
	ctx := context.Background()
	s := newStack(t, ctx)
	owner := s.owner(t, "Fail Mail Co")
	sink := startSMTPSink(t, "mailer", "sink-password-1")
	conn := func(name string, password string) {
		owner.mustOK("POST", "/be/v1/connections", map[string]any{"name": name, "kind": "smtp", "skip_test": true,
			"config": map[string]any{"host": "127.0.0.1", "port": sink.port(), "tls": "none", "username": "mailer", "from": "ops@example.com"},
			"secret": map[string]any{"password": password}}, 201)
	}
	conn("good", "sink-password-1")
	conn("wrong", "wrong-password-77")

	node := func(connection string) string {
		return "{connection: " + connection + ", to: a@example.com, subject: hi, text: body}"
	}
	for _, tt := range []struct {
		name, connection, rcpt, code string
		attempts                     int
	}{
		{"recipient rejected is permanent", "good", "550 no such mailbox", "smtp_rejected", 1},
		{"a 450 is retried until the budget runs out", "good", "450 try later", "smtp_temporary", 3},
		{"bad credentials are permanent and not echoed", "wrong", "", "smtp_auth", 1},
		{"unknown connection", "ghost", "", "E_CONNECTION_NOT_FOUND", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sink.rcptReply = tt.rcpt
			wf := "fail-" + strings.NewReplacer(" ", "-", "'", "").Replace(strings.ToLower(tt.code)) + fmt.Sprint(tt.attempts)
			publishWorkflow(t, owner, strings.ReplaceAll(wf, "_", "-"), node(tt.connection))
			id, run := runToEnd(t, owner, strings.ReplaceAll(wf, "_", "-"))
			if run["status"] != "failed" {
				t.Fatalf("run = %v", run)
			}
			attempts := s.h.attempts(mustParse(t, id), "mail")
			if len(attempts) != tt.attempts || attempts[len(attempts)-1]["code"] != tt.code {
				t.Fatalf("attempts = %v, want %d ending in %s", attempts, tt.attempts, tt.code)
			}
		})
	}
	if n := s.h.occurrences("wrong-password-77"); n != 0 {
		t.Fatalf("a rejected password is stored in %d row(s); the server echoed it back and it must be scrubbed", n)
	}
}

// email.send over Resend with a saved connection and with inline credentials from
// the vault: the Idempotency-Key is stable across the retry of a 503.
func TestEmailSendOverResend(t *testing.T) {
	ctx := context.Background()
	resend := startResend(t)
	resend.failures = 1
	s := newStack(t, ctx, func(c *config.Config) { c.Services.ResendBaseURL = resend.URL })
	owner := s.owner(t, "Resend Co")
	owner.mustOK("POST", "/be/v1/connections", map[string]any{"name": "resend-main", "kind": "resend",
		"config": map[string]any{"from": "Ops <ops@example.com>"}, "secret": map[string]any{"api_key": "re_good_key_123456"}}, 201)
	owner.mustOK("PUT", "/be/v1/secrets/RESEND_KEY", map[string]any{"value": "re_good_key_123456"}, 200)

	publishWorkflow(t, owner, "via-connection", `{connection: resend-main, to: a@example.com, subject: hi, text: body}`)
	publishWorkflow(t, owner, "via-inline", `{resend: {api_key: "${secrets.RESEND_KEY}"}, from: bot@example.com, to: [a@example.com, b@example.com], subject: hi, html: "<b>b</b>"}`)

	id, run := runToEnd(t, owner, "via-connection")
	if run["status"] != "succeeded" {
		t.Fatalf("run = %v", run)
	}
	keys, bodies := resend.sent()
	if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Fatalf("Idempotency-Keys across the retry = %v, want two identical non-empty keys", keys)
	}
	if !strings.Contains(bodies[1], `"from":"Ops \u003cops@example.com\u003e"`) && !strings.Contains(bodies[1], "ops@example.com") {
		t.Fatalf("body = %s", bodies[1])
	}
	if attempts := s.h.attempts(mustParse(t, id), "mail"); len(attempts) != 2 || attempts[0]["code"] != "upstream_5xx" {
		t.Fatalf("attempts = %v", attempts)
	}

	if _, run := runToEnd(t, owner, "via-inline"); run["status"] != "succeeded" {
		t.Fatalf("inline run = %v", run)
	}
	keys, bodies = resend.sent()
	if len(keys) != 3 || !strings.Contains(bodies[2], "b@example.com") || !strings.Contains(bodies[2], `"html":"\u003cb\u003eb\u003c/b\u003e"`) && !strings.Contains(bodies[2], "<b>b</b>") {
		t.Fatalf("inline send: keys %v body %s", keys, bodies[len(bodies)-1])
	}
	if n := s.h.occurrences("re_good_key_123456"); n != 0 {
		t.Fatalf("the Resend key is stored in %d row(s)", n)
	}
}

// Publish-time validation of the node.
func TestEmailSendConfigValidation(t *testing.T) {
	ctx := context.Background()
	s := newStack(t, ctx)
	owner := s.owner(t, "Validate Mail Co")
	for name, nodeConfig := range map[string]string{
		"no provider":   `{to: a@example.com, subject: hi, text: body}`,
		"two providers": `{connection: x, resend: {api_key: abcdefgh}, to: a@example.com, subject: hi, text: body}`,
		"no subject":    `{connection: x, to: a@example.com, text: body}`,
		"no body":       `{connection: x, to: a@example.com, subject: hi}`,
		"no recipient":  `{connection: x, subject: hi, text: body}`,
		"unknown field": `{connection: x, to: a@example.com, subject: hi, text: body, attachments: []}`,
	} {
		source := "apiVersion: workflow/v1\nname: bad-mail\nnodes:\n  - id: mail\n    type: email.send\n    config: " + nodeConfig + "\n"
		if status, _ := owner.publish(source); status != 400 {
			t.Errorf("%s: publish = %d, want 400", name, status)
		}
	}
}
