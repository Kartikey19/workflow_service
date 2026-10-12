//go:build integration

package integration_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kartikey/workflow_service/internal/services/workflows"
	"github.com/kartikey/workflow_service/pkg/uid"
)

func TestDraftPublicationRespectsWorkflowLimit(t *testing.T) {
	ctx := context.Background()
	s := newStack(t, ctx)
	owner := s.owner(t, "Draft Limit")
	tenantID, err := uid.Parse(owner.orgID())
	if err != nil {
		t.Fatal(err)
	}
	svc := workflows.NewService(s.h.pool, s.deps.Registry)
	definition := func(name string) []byte {
		return []byte("apiVersion: workflow/v1\nname: " + name + "\nnodes:\n  - {id: done, type: noop, config: {}}\n")
	}
	if _, err := svc.PrepareTest(ctx, tenantID, definition("draft-only"), "test"); err != nil {
		t.Fatal(err)
	}
	params := workflows.PublishParams{TenantID: tenantID, Source: definition("published"), Limits: workflows.Limits{MaxWorkflows: 1}}
	if _, err := svc.Publish(ctx, params); err != nil {
		t.Fatalf("draft should not consume workflow allowance: %v", err)
	}
	params.Source = definition("draft-only")
	var limit *workflows.LimitError
	if _, err := svc.Publish(ctx, params); !errors.As(err, &limit) || limit.Limit != "workflows" {
		t.Fatalf("draft publication at workflow limit = %v", err)
	}
	params.Source = definition("published")
	if _, err := svc.Publish(ctx, params); err != nil {
		t.Fatalf("existing publication should still be allowed: %v", err)
	}
}

// alertSink is a webhook endpoint that records alert deliveries.
type alertSink struct {
	mu       sync.Mutex
	received []map[string]any
	headers  []http.Header
	bodies   [][]byte
	fail     atomic.Bool
	server   *httptest.Server
}

func newAlertSink(t *testing.T) *alertSink {
	s := &alertSink{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if s.fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		s.mu.Lock()
		s.received = append(s.received, decoded)
		s.headers = append(s.headers, r.Header.Clone())
		s.bodies = append(s.bodies, body)
		s.mu.Unlock()
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *alertSink) events() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []string{}
	for _, r := range s.received {
		if event, ok := r["event"].(string); ok {
			out = append(out, event)
		}
	}
	return out
}

func (s *alertSink) waitFor(t *testing.T, count int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for len(s.events()) < count {
		if time.Now().After(deadline) {
			t.Fatalf("expected %d alerts, got %v", count, s.events())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// flaky answers 500 until healthy is set, then 200.
func flaky(t *testing.T) (*httptest.Server, *atomic.Bool, *atomic.Int32) {
	var healthy atomic.Bool
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !healthy.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"down"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)
	return server, &healthy, &calls
}

func twoStepWorkflow(name, url string) string {
	return `apiVersion: workflow/v1
name: ` + name + `
nodes:
  - id: prepare
    type: http
    config: {method: GET, url: "` + url + `/prepare"}
  - id: deliver
    type: http
    needs: [prepare]
    retry: {max_attempts: 1}
    config: {method: POST, url: "` + url + `/deliver", body: {ok: true}}
`
}

func TestDraftTestRuns(t *testing.T) {
	ctx := context.Background()
	s := newStack(t, ctx)
	owner := s.owner(t, "Draft tests")
	draft := "apiVersion: workflow/v1\nname: drafted\ninputs:\n  who: {type: string, required: true}\nnodes:\n  - {id: greet, type: transform, config: {value: 'hello ${input.who}'}}\n"
	if status, body := owner.call("POST", "/be/v1/test-runs", map[string]any{"yaml": draft, "input": map[string]any{}}); status != 400 {
		t.Fatalf("missing required input = %d %v", status, body)
	}
	if status, body := owner.call("POST", "/be/v1/test-runs", map[string]any{"yaml": "apiVersion: workflow/v1\nname: broken\nnodes: []\n"}); status != 400 || body["errors"] == nil {
		t.Fatalf("invalid draft = %d %v", status, body)
	}
	run := owner.mustOK("POST", "/be/v1/test-runs", map[string]any{"yaml": draft, "input": map[string]any{"who": "world"}}, 201)
	if run["test"] != true || run["trigger"] != "test" || run["version"].(float64) != -1 {
		t.Fatalf("test run %v", run)
	}
	if done := owner.waitRun(run["id"].(string)); done["status"] != "succeeded" || done["test"] != true {
		t.Fatalf("finished test run %v", done)
	}
	// A draft that was only tested is not a published workflow.
	if _, list := owner.call("GET", "/be/v1/workflows", nil); strings.Contains(toJSON(list), `"drafted"`) {
		t.Fatalf("test-only workflow listed: %v", list)
	}
	if status, _ := owner.call("POST", "/be/v1/runs", map[string]any{"workflow": "drafted", "input": map[string]any{"who": "x"}}); status != 404 {
		t.Fatalf("published run of a tested draft = %d", status)
	}
	// The same draft reuses its test version; publishing still starts at v1.
	again := owner.mustOK("POST", "/be/v1/test-runs", map[string]any{"yaml": draft, "input": map[string]any{"who": "again"}}, 201)
	if again["version"].(float64) != -1 {
		t.Fatalf("identical draft got a new test version: %v", again)
	}
	if status, published := owner.publish(draft); status != 201 || published["version"].(float64) != 1 {
		t.Fatalf("publish after testing = %d %v", status, published)
	}
	changed := strings.Replace(draft, "hello", "hi", 1)
	edited := owner.mustOK("POST", "/be/v1/test-runs", map[string]any{"yaml": changed, "input": map[string]any{"who": "v2"}}, 201)
	if edited["version"].(float64) != -2 {
		t.Fatalf("edited draft version %v", edited["version"])
	}
	owner.waitRun(edited["id"].(string))
	// Retrying a test run reruns the draft it tested, not the published version.
	retry := owner.mustOK("POST", "/be/v1/runs/"+edited["id"].(string)+"/retry", nil, 201)
	if retry["version"].(float64) != -2 {
		t.Fatalf("retry of a test run used version %v", retry["version"])
	}
	nodes := waitNodes(t, owner, retry["id"].(string))
	if out := toJSON(nodes); !strings.Contains(out, "hi v2") {
		t.Fatalf("retry output %s", out)
	}
	_, detail := owner.call("GET", "/be/v1/workflows/drafted", nil)
	if versions := detail["versions"].([]any); len(versions) != 1 {
		t.Fatalf("test versions listed as published: %v", versions)
	}
	viewer := s.member(t, owner, "viewer")
	if status, _ := viewer.call("POST", "/be/v1/test-runs", map[string]any{"yaml": draft, "input": map[string]any{"who": "v"}}); status != 403 {
		t.Fatalf("viewer test run = %d", status)
	}
}

func waitNodes(t *testing.T, p *person, runID string) map[string]any {
	t.Helper()
	p.waitRun(runID)
	_, nodes := p.call("GET", "/be/v1/runs/"+runID+"/nodes", nil)
	return nodes
}

func TestResumeFromFailedStep(t *testing.T) {
	ctx := context.Background()
	s := newStack(t, ctx)
	owner := s.owner(t, "Resume Co")
	server, healthy, calls := flaky(t)
	// prepare always succeeds (the server is healthy for GET /prepare only
	// after we flip it, so route by path instead).
	mux := http.NewServeMux()
	var prepareCalls atomic.Int32
	mux.HandleFunc("/prepare", func(w http.ResponseWriter, r *http.Request) {
		prepareCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"prepared":true}`))
	})
	mux.Handle("/deliver", server.Config.Handler)
	upstream := httptest.NewServer(mux)
	t.Cleanup(upstream.Close)
	if status, body := owner.publish(twoStepWorkflow("resumable", upstream.URL)); status != 201 {
		t.Fatalf("publish %d %v", status, body)
	}
	run := owner.mustOK("POST", "/be/v1/runs", map[string]any{"workflow": "resumable", "input": map[string]any{}}, 201)
	failed := owner.waitRun(run["id"].(string))
	if failed["status"] != "failed" || failed["error"].(map[string]any)["node_id"] != "deliver" {
		t.Fatalf("first run %v", failed)
	}
	if status, _ := owner.call("POST", "/be/v1/runs/"+run["id"].(string)+"/resume", nil); status != 201 {
		// A resume of a run that fails again is still a new run.
		t.Fatalf("resume of failed run = %d", status)
	}
	healthy.Store(true)
	resumed := owner.mustOK("POST", "/be/v1/runs/"+run["id"].(string)+"/resume", nil, 201)
	if resumed["trigger"] != "resume" || resumed["parent_run_id"] != run["id"] {
		t.Fatalf("resumed run %v", resumed)
	}
	if done := owner.waitRun(resumed["id"].(string)); done["status"] != "succeeded" {
		t.Fatalf("resumed run %v", done)
	}
	if prepareCalls.Load() != 1 {
		t.Fatalf("prepare ran %d times; resume must reuse its result", prepareCalls.Load())
	}
	if calls.Load() < 3 {
		t.Fatalf("deliver calls = %d", calls.Load())
	}
	_, nodes := owner.call("GET", "/be/v1/runs/"+resumed["id"].(string)+"/nodes", nil)
	if out := toJSON(nodes); !strings.Contains(out, "reused_from_run") || !strings.Contains(out, `"prepared":true`) {
		t.Fatalf("resumed nodes %s", out)
	}
	if status, _ := owner.call("POST", "/be/v1/runs/"+resumed["id"].(string)+"/resume", nil); status != 409 {
		t.Fatalf("resume of a succeeded run = %d", status)
	}
	other := s.owner(t, "Resume Other")
	if status, _ := other.call("POST", "/be/v1/runs/"+run["id"].(string)+"/resume", nil); status != 404 {
		t.Fatalf("foreign resume = %d", status)
	}
}

func TestSchedules(t *testing.T) {
	ctx := context.Background()
	s := newStack(t, ctx)
	owner := s.owner(t, "Schedule Co")
	sink := newAlertSink(t)
	if status, body := owner.publish("apiVersion: workflow/v1\nname: nightly\ninputs:\n  team: {type: string, required: true}\nnodes:\n  - {id: report, type: transform, config: {value: 'report for ${input.team}'}}\n"); status != 201 {
		t.Fatalf("publish %d %v", status, body)
	}
	owner.mustOK("POST", "/be/v1/alert-channels", map[string]any{"name": "ops", "kind": "webhook", "url": sink.server.URL, "events": []string{"schedule.failed"}}, 201)

	preview := owner.mustOK("POST", "/be/v1/schedules/preview", map[string]any{"cron": "0 9 * * 1-5", "timezone": "Asia/Kolkata", "count": 3}, 200)
	if next := preview["next"].([]any); len(next) != 3 || !strings.HasSuffix(next[0].(string), "+05:30") {
		t.Fatalf("preview %v", preview)
	}
	for _, bad := range []map[string]any{
		{"name": "x", "workflow": "nightly", "cron": "nope", "input": map[string]any{"team": "a"}},
		{"name": "x", "workflow": "nightly", "cron": "@hourly", "timezone": "Nowhere/Land", "input": map[string]any{"team": "a"}},
		{"name": "x", "workflow": "nightly", "cron": "@hourly"},
		{"name": "x", "workflow": "missing", "cron": "@hourly"},
		{"name": "Bad Name", "workflow": "nightly", "cron": "@hourly", "input": map[string]any{"team": "a"}},
	} {
		if status, body := owner.call("POST", "/be/v1/schedules", bad); status != 400 {
			t.Fatalf("invalid schedule %v = %d %v", bad, status, body)
		}
	}
	created := owner.mustOK("POST", "/be/v1/schedules", map[string]any{
		"name": "weekday-report", "workflow": "nightly", "cron": "0 9 * * 1-5", "timezone": "Europe/London",
		"input": map[string]any{"team": "ops"},
	}, 201)
	id := created["id"].(string)
	if created["overlap"] != "skip" || created["catch_up"] != "latest" || created["next_run_at"] == nil {
		t.Fatalf("created %v", created)
	}
	if status, _ := owner.call("POST", "/be/v1/schedules", map[string]any{"name": "weekday-report", "workflow": "nightly", "cron": "@hourly", "input": map[string]any{"team": "a"}}); status != 409 {
		t.Fatalf("duplicate name = %d", status)
	}

	// Make the slot due; a worker starts exactly one run for it.
	due := func(ago string) {
		if _, err := s.h.pool.Exec(ctx, "update schedules set next_run_at = now() - $2::interval where id = $1", id, ago); err != nil {
			t.Fatal(err)
		}
	}
	waitOutcome := func(want string) map[string]any {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			_, list := owner.call("GET", "/be/v1/schedules", nil)
			item := list["items"].([]any)[0].(map[string]any)
			if item["last_outcome"] == want {
				return item
			}
			if time.Now().After(deadline) {
				t.Fatalf("schedule outcome %v, want %s", item, want)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	due("1 second")
	started := waitOutcome("started")
	runID := started["last_run_id"].(string)
	finished := owner.waitRun(runID)
	if finished["trigger"] != "schedule" || finished["created_by"] != "schedule:weekday-report" {
		t.Fatalf("scheduled run %v", finished)
	}
	if next, _ := time.Parse(time.RFC3339, started["next_run_at"].(string)); !next.After(time.Now()) {
		t.Fatalf("next slot did not move forward: %v", started["next_run_at"])
	}
	var runs int
	if err := s.h.pool.QueryRow(ctx, "select count(*) from runs where trigger = 'schedule'").Scan(&runs); err != nil || runs != 1 {
		t.Fatalf("scheduled runs = %d %v", runs, err)
	}

	// Overlap: a still-running previous run skips the slot.
	if _, err := s.h.pool.Exec(ctx, "update runs set status = 'running', ended_at = null where id = $1", runID); err != nil {
		t.Fatal(err)
	}
	due("1 second")
	if skipped := waitOutcome("skipped"); skipped["last_run_id"] != runID {
		t.Fatalf("overlap skip %v", skipped)
	}
	if _, err := s.h.pool.Exec(ctx, "update runs set status = 'succeeded', ended_at = now() where id = $1", runID); err != nil {
		t.Fatal(err)
	}

	// Catch-up off: a slot missed by hours is recorded and alerted, not run.
	owner.mustOK("PATCH", "/be/v1/schedules/"+id, map[string]any{"catch_up": "skip"}, 200)
	due("3 hours")
	waitOutcome("missed")
	sink.waitFor(t, 1)
	if sink.events()[0] != "schedule.failed" {
		t.Fatalf("alerts %v", sink.events())
	}

	// Run now starts immediately without moving the next slot.
	before := waitOutcome("missed")["next_run_at"]
	manual := owner.mustOK("POST", "/be/v1/schedules/"+id+"/run", nil, 201)
	if done := owner.waitRun(manual["id"].(string)); done["status"] != "succeeded" {
		t.Fatalf("manual run %v", done)
	}
	if after := waitOutcome("started"); after["next_run_at"] != before {
		t.Fatalf("run now moved the schedule: %v -> %v", before, after["next_run_at"])
	}

	// Disabling clears the next slot; re-enabling schedules from now.
	disabled := owner.mustOK("PATCH", "/be/v1/schedules/"+id, map[string]any{"enabled": false}, 200)
	if disabled["next_run_at"] != nil {
		t.Fatalf("disabled schedule still due: %v", disabled)
	}
	enabled := owner.mustOK("PATCH", "/be/v1/schedules/"+id, map[string]any{"enabled": true, "cron": "*/5 * * * *"}, 200)
	if enabled["next_run_at"] == nil || enabled["cron"] != "*/5 * * * *" {
		t.Fatalf("re-enabled %v", enabled)
	}
	if status, _ := owner.call("PATCH", "/be/v1/schedules/"+id, map[string]any{"input": map[string]any{}}); status != 400 {
		t.Fatalf("invalid input update = %d", status)
	}

	// Archiving the workflow makes the next slot fail visibly.
	owner.mustOK("DELETE", "/be/v1/workflows/nightly", nil, 200)
	owner.mustOK("PATCH", "/be/v1/schedules/"+id, map[string]any{"catch_up": "latest"}, 200)
	due("1 second")
	if failed := waitOutcome("error"); !strings.Contains(failed["last_error"].(string), "archived") {
		t.Fatalf("archived workflow outcome %v", failed)
	}

	other := s.owner(t, "Schedule Other")
	if status, _ := other.call("PATCH", "/be/v1/schedules/"+id, map[string]any{"enabled": false}); status != 404 {
		t.Fatalf("foreign schedule = %d", status)
	}
	if _, list := other.call("GET", "/be/v1/schedules", nil); len(list["items"].([]any)) != 0 {
		t.Fatalf("foreign schedules listed: %v", list)
	}
	viewer := s.member(t, owner, "viewer")
	if status, _ := viewer.call("POST", "/be/v1/schedules/"+id+"/run", nil); status != 403 {
		t.Fatalf("viewer run now = %d", status)
	}
	owner.mustOK("DELETE", "/be/v1/schedules/"+id, nil, 204)
}

func TestFailureAlerts(t *testing.T) {
	ctx := context.Background()
	s := newStack(t, ctx)
	owner := s.owner(t, "Alert Co")
	sink := newAlertSink(t)
	server, healthy, _ := flaky(t)
	if status, body := owner.publish(twoStepWorkflow("watched", server.URL)); status != 201 {
		t.Fatalf("publish %d %v", status, body)
	}
	if status, _ := owner.call("POST", "/be/v1/alert-channels", map[string]any{"name": "ops", "kind": "pager", "url": sink.server.URL}); status != 400 {
		t.Fatalf("unknown kind = %d", status)
	}
	channel := owner.mustOK("POST", "/be/v1/alert-channels", map[string]any{"name": "ops", "kind": "webhook", "url": sink.server.URL}, 201)
	secret := channel["signing_secret"].(string)
	if secret == "" || len(channel["events"].([]any)) != 4 {
		t.Fatalf("channel %v", channel)
	}
	slack := owner.mustOK("POST", "/be/v1/alert-channels", map[string]any{"name": "slack-ops", "kind": "slack", "url": sink.server.URL + "/services/T1/B1/topsecretvalue", "events": []string{"run.failed"}, "workflows": []string{"other"}}, 201)
	if strings.Contains(toJSON(slack), "topsecretvalue") {
		t.Fatalf("slack URL exposed: %v", slack)
	}
	_, listed := owner.call("GET", "/be/v1/alert-channels", nil)
	if strings.Contains(toJSON(listed), secret) || strings.Contains(toJSON(listed), "topsecretvalue") {
		t.Fatal("listing exposed channel secrets")
	}
	if s.h.occurrences(secret) != 0 || s.h.occurrences("topsecretvalue") != 0 {
		t.Fatal("channel secrets persisted in clear")
	}
	owner.mustOK("POST", "/be/v1/alert-channels/"+channel["id"].(string)+"/test", nil, 200)
	owner.mustOK("POST", "/be/v1/alert-channels/"+slack["id"].(string)+"/test", nil, 200)
	sink.waitFor(t, 1) // the Slack-format test has no "event" field
	sink.mu.Lock()
	first := sink.bodies[0]
	header := sink.headers[0]
	sink.mu.Unlock()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(first)
	if header.Get("X-Weft-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) || header.Get("X-Weft-Event") != "test" {
		t.Fatalf("signature headers %v", header)
	}
	sink.mu.Lock()
	if len(sink.bodies) != 2 || !strings.Contains(string(sink.bodies[1]), `"text":"*Test alert from Weft*`) {
		t.Fatalf("slack test body %q", sink.bodies)
	}
	sink.received, sink.headers, sink.bodies = nil, nil, nil
	sink.mu.Unlock()

	// Two failures open one incident: one alert, not two.
	for range 2 {
		run := owner.mustOK("POST", "/be/v1/runs", map[string]any{"workflow": "watched", "input": map[string]any{}}, 201)
		if done := owner.waitRun(run["id"].(string)); done["status"] != "failed" {
			t.Fatalf("expected failure %v", done)
		}
	}
	sink.waitFor(t, 1)
	time.Sleep(time.Second) // a second alert would have been delivered by now
	if events := sink.events(); len(events) != 1 || events[0] != "run.failed" {
		t.Fatalf("alerts after two failures: %v", events)
	}
	sink.mu.Lock()
	failedAlert := sink.received[0]
	sink.mu.Unlock()
	data := failedAlert["data"].(map[string]any)
	if data["workflow"] != "watched" || data["node_id"] != "prepare" || !strings.Contains(failedAlert["text"].(string), "Step prepare failed") {
		t.Fatalf("failure alert %v", failedAlert)
	}

	// A draft test run that fails never alerts.
	test := owner.mustOK("POST", "/be/v1/test-runs", map[string]any{"yaml": twoStepWorkflow("watched", server.URL) + "description: draft\n"}, 201)
	owner.waitRun(test["id"].(string))
	for _, action := range []string{"resume", "retry"} {
		draft := owner.mustOK("POST", "/be/v1/runs/"+test["id"].(string)+"/"+action, nil, 201)
		owner.waitRun(draft["id"].(string))
	}

	// Recovery closes the incident with one message.
	healthy.Store(true)
	run := owner.mustOK("POST", "/be/v1/runs", map[string]any{"workflow": "watched", "input": map[string]any{}}, 201)
	if done := owner.waitRun(run["id"].(string)); done["status"] != "succeeded" {
		t.Fatalf("recovery run %v", done)
	}
	sink.waitFor(t, 2)
	if events := sink.events(); len(events) != 2 || events[1] != "run.recovered" {
		t.Fatalf("alerts after recovery: %v", events)
	}
	sink.mu.Lock()
	recovered := sink.received[1]["data"].(map[string]any)
	sink.mu.Unlock()
	if recovered["failures"].(float64) != 2 {
		t.Fatalf("recovery payload %v", recovered)
	}

	// An unreachable endpoint is retried with backoff and shown in the log.
	healthy.Store(false)
	sink.fail.Store(true)
	run = owner.mustOK("POST", "/be/v1/runs", map[string]any{"workflow": "watched", "input": map[string]any{}}, 201)
	owner.waitRun(run["id"].(string))
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, log := owner.call("GET", "/be/v1/alert-deliveries", nil)
		items := log["items"].([]any)
		latest := items[0].(map[string]any)
		if latest["status"] == "pending" && latest["attempts"].(float64) >= 1 && latest["last_error"] != nil {
			if latest["next_attempt_at"] == nil {
				t.Fatalf("pending delivery without retry time %v", latest)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("delivery log %v", items)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Disabled channels receive nothing; other organizations see nothing.
	owner.mustOK("PATCH", "/be/v1/alert-channels/"+channel["id"].(string), map[string]any{"enabled": false, "events": []string{"run.failed"}}, 200)
	other := s.owner(t, "Alert Other")
	if status, _ := other.call("DELETE", "/be/v1/alert-channels/"+channel["id"].(string), nil); status != 404 {
		t.Fatalf("foreign delete = %d", status)
	}
	if _, log := other.call("GET", "/be/v1/alert-deliveries", nil); len(log["items"].([]any)) != 0 {
		t.Fatalf("foreign deliveries visible: %v", log)
	}
	editor := s.member(t, owner, "editor")
	if status, _ := editor.call("POST", "/be/v1/alert-channels", map[string]any{"name": "mine", "kind": "webhook", "url": sink.server.URL}); status != 403 {
		t.Fatalf("editor created a channel = %d", status)
	}
	owner.mustOK("DELETE", "/be/v1/alert-channels/"+channel["id"].(string), nil, 204)
}

var memberSeq atomic.Int32

// member invites a new person into owner's organization with role.
func (s *stack) member(t *testing.T, owner *person, role string) *person {
	t.Helper()
	email := role + "-" + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "") + "-" + string(rune('a'+memberSeq.Add(1)%26)) + "@example.com"
	_, inv := owner.call("POST", "/be/v1/invites", map[string]any{"email": email, "role": role})
	p := newPerson(t, s.base)
	if status, body := accept(t, p, inv["token"].(string), "member-long-password", role); status != 201 {
		t.Fatalf("%s joins = %d %v", role, status, body)
	}
	return p
}

func TestEmailAlertChannel(t *testing.T) {
	ctx := context.Background()
	s := newStack(t, ctx)
	owner := s.owner(t, "Mail Alerts")
	smtp := startSMTPSink(t, "mailer", "sink-password-1")
	owner.mustOK("POST", "/be/v1/connections", map[string]any{"name": "team-smtp", "kind": "smtp",
		"config": map[string]any{"host": "127.0.0.1", "port": smtp.port(), "tls": "none", "username": "mailer", "from": "Weft <alerts@example.com>"},
		"secret": map[string]any{"password": "sink-password-1"}}, 201)
	if status, _ := owner.call("POST", "/be/v1/alert-channels", map[string]any{"name": "mail", "kind": "email", "connection": "missing", "to": []string{"ops@example.com"}}); status != 400 {
		t.Fatalf("unknown connection = %d", status)
	}
	if status, _ := owner.call("POST", "/be/v1/alert-channels", map[string]any{"name": "mail", "kind": "email", "connection": "team-smtp", "to": []string{"nope"}}); status != 400 {
		t.Fatalf("bad recipient = %d", status)
	}
	channel := owner.mustOK("POST", "/be/v1/alert-channels", map[string]any{"name": "mail", "kind": "email", "connection": "team-smtp", "to": []string{"ops@example.com"}, "events": []string{"run.failed"}}, 201)
	if channel["target"] != "team-smtp → ops@example.com" {
		t.Fatalf("email channel %v", channel)
	}
	owner.mustOK("POST", "/be/v1/alert-channels/"+channel["id"].(string)+"/test", nil, 200)
	if got := smtp.received(); len(got) != 1 || !strings.Contains(got[0], "Subject: [Weft] Test alert from Weft") {
		t.Fatalf("test email %v", got)
	}
	if status, body := owner.publish("apiVersion: workflow/v1\nname: breaks\nnodes:\n  - {id: boom, type: http, retry: {max_attempts: 1}, config: {method: GET, url: 'http://127.0.0.1:1/'}}\n"); status != 201 {
		t.Fatalf("publish %d %v", status, body)
	}
	run := owner.mustOK("POST", "/be/v1/runs", map[string]any{"workflow": "breaks", "input": map[string]any{}}, 201)
	owner.waitRun(run["id"].(string))
	deadline := time.Now().Add(10 * time.Second)
	for len(smtp.received()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("failure email was not delivered")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if msg := smtp.received()[1]; !strings.Contains(msg, "Subject: [Weft] Workflow breaks is failing") || !strings.Contains(msg, "Step boom failed") {
		t.Fatalf("failure email %s", msg)
	}
}
