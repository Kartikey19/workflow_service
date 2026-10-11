//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kartikey/workflow_service/pkg/auth"
	"github.com/kartikey/workflow_service/pkg/uid"
)

// actorAuth lets a test act as different people: X-Actor names the caller and
// X-Role picks the organization role (default editor).
type actorAuth struct{}

func (actorAuth) Authenticate(r *http.Request) (auth.Principal, error) {
	tenant, err := uid.Parse(r.Header.Get("X-Tenant-ID"))
	if err != nil {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	role := r.Header.Get("X-Role")
	if role == "" {
		role = auth.RoleEditor
	}
	return auth.Principal{TenantID: tenant, Email: r.Header.Get("X-Actor"), Role: role, Via: "session"}, nil
}

func as(actor string, extra ...string) []string {
	return append([]string{"X-Actor", actor}, extra...)
}

const reviewWorkflow = `
apiVersion: workflow/v1
name: care-review
inputs:
  patient: {type: string, required: true}
nodes:
  - {id: plan, type: script, config: {}}
  - id: review
    type: human_task
    needs: [plan]
    on_timeout: escalate
    config:
      queue: rn-review
      form: care_plan_v2
      sla: SLA
      assignee: ASSIGNEE
      payload: {patient: '${input.patient}'}
  - {id: escalate, type: script, needs: [review], join: any, config: {}}
  - id: done
    type: transform
    needs: [review]
    config:
      value:
        approved: '${nodes.review.output.approved}'
outputs:
  approved: '${nodes.done.output.approved}'
`

func reviewSource(sla, assignee string) string {
	assignee = strings.TrimSpace(assignee)
	source := strings.Replace(reviewWorkflow, "SLA", sla, 1)
	if assignee == "" {
		return strings.Replace(source, "      assignee: ASSIGNEE\n", "", 1)
	}
	return strings.Replace(source, "ASSIGNEE", assignee, 1)
}

func (h *harness) openTaskID(c apiClient, queue string) string {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, body := c.json("GET", "/be/v1/tasks?status=open&queue="+queue, nil, as("alice")...)
		if items, _ := body["items"].([]any); status == 200 && len(items) > 0 {
			return items[0].(map[string]any)["id"].(string)
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("no open task in %s: %d %v", queue, status, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// M8: the task appears in its queue, can be claimed and released, and
// completing it delivers the payload as the node's output.
func TestHumanTaskWorklistClaimAndComplete(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(reviewSource("1h", ""))
	c := apiClient{t: t, base: h.apiServer(actorAuth{}), tenant: h.tenant}
	h.startWorkers(2)
	runID := h.start("care-review", map[string]any{"patient": "P-1"})
	id := h.openTaskID(c, "rn-review")

	status, task := c.json("GET", "/be/v1/tasks/"+id, nil, as("alice")...)
	if status != 200 || task["status"] != "open" || task["queue"] != "rn-review" || task["form"] != "care_plan_v2" ||
		task["workflow"] != "care-review" || task["node_id"] != "review" || task["run_id"] != idString(runID) ||
		task["payload"].(map[string]any)["patient"] != "P-1" || task["due_at"] == nil {
		t.Fatalf("task = %d %v", status, task)
	}
	if status, body := c.json("GET", "/be/v1/tasks?queue=other", nil, as("alice")...); status != 200 || len(body["items"].([]any)) != 0 {
		t.Fatalf("another queue = %d %v", status, body)
	}

	// Alice claims it; claiming again is a no-op; Bob cannot take or finish it.
	if status, body := c.json("POST", "/be/v1/tasks/"+id+"/claim", nil, as("alice")...); status != 200 || body["status"] != "claimed" || body["claimed_by"] != "alice" {
		t.Fatalf("claim = %d %v", status, body)
	}
	if status, _ := c.json("POST", "/be/v1/tasks/"+id+"/claim", nil, as("alice")...); status != 200 {
		t.Fatalf("re-claim by the owner = %d, want 200", status)
	}
	if status, body := c.json("POST", "/be/v1/tasks/"+id+"/claim", nil, as("bob")...); status != 409 || body["code"] != "task_claimed" {
		t.Fatalf("claim by bob = %d %v", status, body)
	}
	if status, body := c.json("POST", "/be/v1/tasks/"+id+"/complete", map[string]any{"payload": map[string]any{"approved": true}}, as("bob")...); status != 409 {
		t.Fatalf("complete by bob = %d %v, want 409", status, body)
	}
	if status, body := c.json("GET", "/be/v1/tasks?mine=true", nil, as("alice")...); status != 200 || len(body["items"].([]any)) != 1 {
		t.Fatalf("alice's tasks = %d %v", status, body)
	}
	if status, _ := c.json("POST", "/be/v1/tasks/"+id+"/release", nil, as("bob")...); status != 403 {
		t.Fatalf("release by bob = %d, want 403", status)
	}
	if status, body := c.json("POST", "/be/v1/tasks/"+id+"/release", nil, as("alice")...); status != 200 || body["status"] != "open" {
		t.Fatalf("release = %d %v", status, body)
	}
	if status, _ := c.json("POST", "/be/v1/tasks/"+id+"/claim", nil, as("bob")...); status != 200 {
		t.Fatalf("claim by bob after release = %d", status)
	}

	// A viewer cannot act; an admin can finish a task someone else claimed.
	if status, _ := c.json("POST", "/be/v1/tasks/"+id+"/complete", nil, as("vera", "X-Role", "viewer")...); status != 403 {
		t.Fatalf("complete by a viewer = %d, want 403", status)
	}
	status, body := c.json("POST", "/be/v1/tasks/"+id+"/complete", map[string]any{"payload": map[string]any{"approved": true}}, as("carol", "X-Role", "admin")...)
	if status != 200 || body["status"] != "completed" || body["run_id"] != idString(runID) {
		t.Fatalf("complete = %d %v", status, body)
	}
	if got := h.waitStatus(runID, 15*time.Second, "succeeded", "failed"); got != "succeeded" {
		t.Fatalf("run = %s, nodes %v", got, h.nodeStatuses(runID))
	}
	if out, _ := h.runOutput(runID); out["approved"] != true {
		t.Fatalf("outputs = %v", out)
	}
	if got := h.nodeStatuses(runID); got["escalate"] != "skipped" || got["review"] != "succeeded" {
		t.Fatalf("nodes = %v", got)
	}
	if status, task := c.json("GET", "/be/v1/tasks/"+id, nil, as("alice")...); status != 200 || task["status"] != "completed" {
		t.Fatalf("task after completion = %d %v", status, task)
	}
	var completedBy string
	if err := h.pool.QueryRow(ctx, `select completed_by from tasks where id = $1`, id).Scan(&completedBy); err != nil || completedBy != "carol" {
		t.Fatalf("completed_by = %q, %v", completedBy, err)
	}
	if status, body := c.json("POST", "/be/v1/tasks/"+id+"/complete", nil, as("carol", "X-Role", "admin")...); status != 409 || body["code"] != "task_closed" {
		t.Fatalf("second completion = %d %v", status, body)
	}
	h.checkInvariants(runID)
}

// M8: a task payload that fails nothing but a missing signal payload shape is
// the wait's business; an assigned task is only workable by its assignee or an
// admin.
func TestHumanTaskAssigneeRules(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(reviewSource("1h", "alice"))
	c := apiClient{t: t, base: h.apiServer(actorAuth{}), tenant: h.tenant}
	h.startWorkers(1)
	runID := h.start("care-review", map[string]any{"patient": "P-2"})
	id := h.openTaskID(c, "rn-review")

	if status, body := c.json("GET", "/be/v1/tasks?assignee=alice", nil, as("bob")...); status != 200 || len(body["items"].([]any)) != 1 {
		t.Fatalf("assignee filter = %d %v", status, body)
	}
	if status, _ := c.json("POST", "/be/v1/tasks/"+id+"/claim", nil, as("bob")...); status != 403 {
		t.Fatalf("claim by a non-assignee = %d, want 403", status)
	}
	if status, _ := c.json("POST", "/be/v1/tasks/"+id+"/complete", map[string]any{"payload": map[string]any{"approved": false}}, as("bob")...); status != 403 {
		t.Fatalf("complete by a non-assignee = %d, want 403", status)
	}
	if status, body := c.json("POST", "/be/v1/tasks/"+id+"/complete", map[string]any{"payload": map[string]any{"approved": false}}, as("alice")...); status != 200 {
		t.Fatalf("complete by the assignee = %d %v", status, body)
	}
	h.waitStatus(runID, 15*time.Second, "succeeded")
	if out, _ := h.runOutput(runID); out["approved"] != false {
		t.Fatalf("outputs = %v", out)
	}
}

// M8: when the SLA passes, the existing timeout path escalates; the task shows
// as expired and can no longer be completed.
func TestHumanTaskSLAEscalates(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	// No outputs: they would read the skipped "done" node on the timeout path.
	h.mustPublish(strings.Replace(reviewSource("1s", ""), "outputs:\n  approved: '${nodes.done.output.approved}'\n", "", 1))
	c := apiClient{t: t, base: h.apiServer(actorAuth{}), tenant: h.tenant}
	h.startWorkers(2)
	runID := h.start("care-review", map[string]any{"patient": "P-3"})
	id := h.openTaskID(c, "rn-review")

	if got := h.waitStatus(runID, 20*time.Second, "succeeded", "failed"); got != "succeeded" {
		t.Fatalf("run = %s, nodes %v", got, h.nodeStatuses(runID))
	}
	nodes := h.nodeStatuses(runID)
	if nodes["review"] != "failed" || nodes["escalate"] != "succeeded" || nodes["done"] != "skipped" {
		t.Fatalf("nodes = %v", nodes)
	}
	if status, task := c.json("GET", "/be/v1/tasks/"+id, nil, as("alice")...); status != 200 || task["status"] != "expired" {
		t.Fatalf("task = %d %v, want expired", status, task)
	}
	if status, body := c.json("POST", "/be/v1/tasks/"+id+"/complete", nil, as("alice")...); status != 409 || body["code"] != "task_closed" {
		t.Fatalf("complete after SLA = %d %v", status, body)
	}
	h.checkInvariants(runID)
}

// M8: cancelling the run cancels its open tasks, and another organization
// never sees a task.
func TestHumanTaskCancelledWithRunAndTenantIsolated(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(reviewSource("1h", ""))
	base := h.apiServer(actorAuth{})
	c := apiClient{t: t, base: base, tenant: h.tenant}
	h.startWorkers(1)
	runID := h.start("care-review", map[string]any{"patient": "P-4"})
	id := h.openTaskID(c, "rn-review")

	other := apiClient{t: t, base: base, tenant: newUUID(t)}
	if status, _ := other.json("GET", "/be/v1/tasks/"+id, nil, as("mallory")...); status != 404 {
		t.Fatalf("another organization reads the task: %d", status)
	}
	if status, body := other.json("GET", "/be/v1/tasks", nil, as("mallory")...); status != 200 || len(body["items"].([]any)) != 0 {
		t.Fatalf("another organization lists tasks: %d %v", status, body)
	}
	if status, _ := other.json("POST", "/be/v1/tasks/"+id+"/complete", nil, as("mallory")...); status != 404 {
		t.Fatalf("another organization completes the task: %d", status)
	}

	if err := h.flow.Cancel(ctx, h.tenant, runID, "test"); err != nil {
		t.Fatal(err)
	}
	if status, task := c.json("GET", "/be/v1/tasks/"+id, nil, as("alice")...); status != 200 || task["status"] != "cancelled" {
		t.Fatalf("task = %d %v, want cancelled", status, task)
	}
	if status, body := c.json("POST", "/be/v1/tasks/"+id+"/claim", nil, as("alice")...); status != 409 || body["code"] != "task_closed" {
		t.Fatalf("claim of a cancelled task = %d %v", status, body)
	}
}

// healthcareWorkflow is 02 §5.3 pointed at one test server, with the 24h SLA
// and the 30-day sleep parameterised so the test does not wait.
func healthcareWorkflow(base, sla string) string {
	return strings.NewReplacer("BASE", base, "SLA", sla).Replace(`
apiVersion: workflow/v1
name: post-discharge-followup
inputs:
  patient_id: {type: string, required: true}
  discharge_at: {type: string, required: true}
correlation: "patient:${input.patient_id}"
timeout: 2160h
nodes:
  - id: create_care_plan
    type: http
    config:
      method: POST
      url: "BASE/ehr/care-plans"
      body: {patient: '${input.patient_id}'}
  - id: nurse_review
    type: human_task
    needs: [create_care_plan]
    on_timeout: escalate
    config:
      queue: rn-review
      form: care_plan_v2
      sla: SLA
      payload: {plan: '${nodes.create_care_plan.output.body}'}
  - id: escalate
    type: http
    needs: [nurse_review]
    join: any
    config: {method: POST, url: "BASE/ehr/escalate"}
  - id: wait_30d
    type: sleep
    needs: [nurse_review]
    config: {duration: 720h}
  - id: outreach_call
    type: http
    needs: [wait_30d]
    config:
      method: POST
      url: "BASE/crm/tasks"
      body: {patient: '${input.patient_id}', kind: followup_call}
`)
}

// 02 §5.3 end to end: the nurse completes the task, the run sleeps thirty days
// (fast-forwarded), then makes the outreach call; escalation never runs.
func TestHealthcareExampleNurseReviewThenOutreach(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	up, base := newDeliveryUpstream(t, map[string]func(http.ResponseWriter){
		"/ehr/care-plans": func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"plan":"PLAN-1"}`)) },
	})
	h.mustPublish(healthcareWorkflow(base, "24h"))
	c := apiClient{t: t, base: h.apiServer(actorAuth{}), tenant: h.tenant}
	h.startWorkers(2)
	runID := h.start("post-discharge-followup", map[string]any{"patient_id": "P-9", "discharge_at": "2026-10-01"})
	id := h.openTaskID(c, "rn-review")

	if _, task := c.json("GET", "/be/v1/tasks/"+id, nil, as("nina")...); task["payload"].(map[string]any)["plan"].(map[string]any)["plan"] != "PLAN-1" {
		t.Fatalf("the task does not carry the care plan: %v", task)
	}
	if status, body := c.json("POST", "/be/v1/tasks/"+id+"/complete", map[string]any{"payload": map[string]any{"approved": true}}, as("nina")...); status != 200 {
		t.Fatalf("complete = %d %v", status, body)
	}
	waitNode(t, h, idString(runID), "wait_30d", "waiting")
	if _, err := h.pool.Exec(ctx, `update node_executions set run_after = now() where run_id = $1 and node_id = 'wait_30d' and status = 'waiting'`, runID); err != nil {
		t.Fatal(err)
	}
	if got := h.waitStatus(runID, 20*time.Second, "succeeded", "failed"); got != "succeeded" {
		t.Fatalf("run = %s, nodes %v", got, h.nodeStatuses(runID))
	}
	nodes := h.nodeStatuses(runID)
	if nodes["escalate"] != "skipped" || nodes["outreach_call"] != "succeeded" {
		t.Fatalf("nodes = %v", nodes)
	}
	calls := up.calls("/crm/tasks")
	if len(calls) != 1 || calls[0]["patient"] != "P-9" || len(up.calls("/ehr/escalate")) != 0 {
		t.Fatalf("crm calls = %v, escalations = %d", calls, len(up.calls("/ehr/escalate")))
	}
	h.checkInvariants(runID)
}

// 02 §5.3 SLA path: nobody reviews in time, so the run escalates and neither
// the sleep nor the outreach call happens.
func TestHealthcareExampleSLAEscalation(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	up, base := newDeliveryUpstream(t, map[string]func(http.ResponseWriter){
		"/ehr/care-plans": func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"plan":"PLAN-2"}`)) },
	})
	h.mustPublish(healthcareWorkflow(base, "1s"))
	h.startWorkers(2)
	runID := h.start("post-discharge-followup", map[string]any{"patient_id": "P-10", "discharge_at": "2026-10-01"})
	if got := h.waitStatus(runID, 20*time.Second, "succeeded", "failed"); got != "succeeded" {
		t.Fatalf("run = %s, nodes %v", got, h.nodeStatuses(runID))
	}
	nodes := h.nodeStatuses(runID)
	if nodes["nurse_review"] != "failed" || nodes["escalate"] != "succeeded" || nodes["wait_30d"] != "skipped" || nodes["outreach_call"] != "skipped" {
		t.Fatalf("nodes = %v", nodes)
	}
	if len(up.calls("/ehr/escalate")) != 1 || len(up.calls("/crm/tasks")) != 0 {
		t.Fatalf("escalations %d, crm calls %d", len(up.calls("/ehr/escalate")), len(up.calls("/crm/tasks")))
	}
	h.checkInvariants(runID)
}
