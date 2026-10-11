//go:build integration

package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/internal/services/runs"
	"github.com/kartikey/workflow_service/internal/services/workflows"
)

const doubleWorkflow = `
apiVersion: workflow/v1
name: double
inputs:
  n: {type: number, required: true}
nodes:
  - id: calc
    type: transform
    config: {value: {result: '${input.n * 2.0}'}}
outputs: {result: '${nodes.calc.output.result}'}
`

const parentWorkflow = `
apiVersion: workflow/v1
name: parent
inputs:
  n: {type: number, required: true}
nodes:
  - id: call
    type: workflow
    config:
      name: double
      input: {n: '${input.n}'}
  - id: after
    type: transform
    needs: [call]
    config: {value: '${nodes.call.output.output.result}'}
outputs:
  doubled: '${nodes.after.output}'
  child: '${nodes.call.output.run_id}'
`

// slowWorkflow never finishes on its own: it waits for a signal nobody sends.
const slowWorkflow = `
apiVersion: workflow/v1
name: slow
nodes:
  - id: hold
    type: wait_event
    config: {key: 'slow:${run.id}', timeout: 1h}
`

type childRow struct {
	ID      string
	Status  string
	Trigger string
	Parent  string
}

func (h *harness) children(parent pgtype.UUID) []childRow {
	h.t.Helper()
	rows, err := h.pool.Query(context.Background(), `
		select id::text, status, trigger, parent_run_id::text from runs where parent_run_id = $1 order by created_at`, parent)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []childRow
	for rows.Next() {
		var c childRow
		if err := rows.Scan(&c.ID, &c.Status, &c.Trigger, &c.Parent); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func (h *harness) childRun(parent pgtype.UUID) pgtype.UUID {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if c := h.children(parent); len(c) > 0 {
			return mustParse(h.t, c[0].ID)
		}
		if time.Now().After(deadline) {
			h.t.Fatal("no child run was started")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (h *harness) nodeError(runID pgtype.UUID, nodeID string) map[string]any {
	h.t.Helper()
	var e map[string]any
	err := h.pool.QueryRow(context.Background(), `
		select coalesce(error, '{}'::jsonb) from node_executions where run_id = $1 and node_id = $2 order by attempt desc limit 1`,
		runID, nodeID).Scan(&e)
	if err != nil {
		h.t.Fatal(err)
	}
	return e
}

// M9: a workflow node runs another workflow as a child run and its outputs come
// back as the node's output.
func TestSubworkflowRunsChildAndReturnsItsOutput(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(doubleWorkflow)
	h.mustPublish(parentWorkflow)
	h.startWorkers(2)
	runID := h.start("parent", map[string]any{"n": 21})
	if status := h.waitStatus(runID, 15*time.Second, "succeeded", "failed"); status != "succeeded" {
		t.Fatalf("run = %s, nodes %v", status, h.nodeStatuses(runID))
	}
	out, _ := h.runOutput(runID)
	kids := h.children(runID)
	if out["doubled"] != float64(42) || len(kids) != 1 || out["child"] != kids[0].ID {
		t.Fatalf("outputs = %v, children = %v", out, kids)
	}
	if kids[0].Status != "succeeded" || kids[0].Trigger != "subworkflow" || kids[0].Parent != idString(runID) {
		t.Fatalf("child = %+v", kids[0])
	}
	if events := h.eventTypes(runID); !contains(events, "node.waiting") || !contains(events, "node.succeeded") {
		t.Fatalf("parent events = %v", events)
	}
	h.checkInvariants(runID)
	h.checkInvariants(mustParse(t, kids[0].ID))
}

// A failed child fails the node (with the child's error attached), and the
// node's on_failure handler runs like for any other node.
func TestSubworkflowChildFailureFailsTheNodeAndRunsTheHandler(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(`
apiVersion: workflow/v1
name: breaks
nodes:
  - {id: boom, type: script, config: {}}
`)
	h.script.set("boom", func(context.Context, executor.Request) (executor.Result, error) {
		return executor.Result{}, executor.Permanent("child_broke", errors.New("kaput"))
	})
	h.mustPublish(`
apiVersion: workflow/v1
name: tolerant
nodes:
  - id: call
    type: workflow
    on_failure: recover
    config: {name: breaks}
  - id: recover
    type: script
    needs: [call]
    join: any
    config: {}
  - {id: tail, type: script, needs: [call], config: {}}
`)
	h.startWorkers(2)
	runID := h.start("tolerant", nil)
	if status := h.waitStatus(runID, 15*time.Second, "succeeded", "failed"); status != "succeeded" {
		t.Fatalf("run = %s, nodes %v", status, h.nodeStatuses(runID))
	}
	got := h.nodeStatuses(runID)
	if got["call"] != "failed" || got["recover"] != "succeeded" || got["tail"] != "skipped" {
		t.Fatalf("nodes = %v", got)
	}
	e := h.nodeError(runID, "call")
	kids := h.children(runID)
	if e["code"] != "E_SUBWORKFLOW_FAILED" || e["child_run_id"] != kids[0].ID || e["child_error"] == nil || kids[0].Status != "failed" {
		t.Fatalf("node error = %v, child = %+v", e, kids)
	}
	h.checkInvariants(runID)
}

// Cancelling the parent cancels the running child.
func TestSubworkflowParentCancelCancelsChild(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(slowWorkflow)
	h.mustPublish(`
apiVersion: workflow/v1
name: waits-for-slow
nodes:
  - {id: call, type: workflow, config: {name: slow}}
`)
	h.startWorkers(2)
	runID := h.start("waits-for-slow", nil)
	child := h.childRun(runID)
	waitNode(t, h, idString(child), "hold", "waiting")

	if err := h.flow.Cancel(ctx, h.tenant, runID, "stop"); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(runID, 10*time.Second, "cancelled")
	if status := h.waitStatus(child, 10*time.Second, "cancelled"); status != "cancelled" {
		t.Fatalf("child = %s", status)
	}
	h.checkInvariants(runID)
	h.checkInvariants(child)
}

// Cancelling the child directly fails the parent's node.
func TestSubworkflowChildCancelFailsTheParentNode(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(slowWorkflow)
	h.mustPublish(`
apiVersion: workflow/v1
name: waits-for-slow
nodes:
  - {id: call, type: workflow, config: {name: slow}}
`)
	h.startWorkers(2)
	runID := h.start("waits-for-slow", nil)
	child := h.childRun(runID)
	waitNode(t, h, idString(child), "hold", "waiting")

	if err := h.flow.Cancel(ctx, h.tenant, child, "nope"); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(runID, 10*time.Second, "failed")
	if e := h.nodeError(runID, "call"); e["code"] != "E_SUBWORKFLOW_CANCELLED" {
		t.Fatalf("node error = %v", e)
	}
	h.checkInvariants(runID)
}

// When the parent stops waiting (timeout), the child it started is cancelled.
func TestSubworkflowWaitTimeoutCancelsTheChild(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(slowWorkflow)
	h.mustPublish(`
apiVersion: workflow/v1
name: impatient
nodes:
  - {id: call, type: workflow, config: {name: slow, timeout: 1s}}
`)
	h.startWorkers(2)
	runID := h.start("impatient", nil)
	child := h.childRun(runID)
	h.waitStatus(runID, 20*time.Second, "failed")
	if status := h.waitStatus(child, 10*time.Second, "cancelled"); status != "cancelled" {
		t.Fatalf("child = %s, want cancelled after the parent gave up", status)
	}
	if e := h.nodeError(runID, "call"); e["code"] != "wait_timeout" {
		t.Fatalf("node error = %v", e)
	}
	h.checkInvariants(runID)
}

func TestSubworkflowConfigErrorsArePermanentAndStartNoChild(t *testing.T) {
	ctx := context.Background()
	for name, tt := range map[string]struct {
		config string
		code   string
	}{
		"unknown workflow": {`{name: no-such-workflow}`, "E_SUBWORKFLOW_NOT_FOUND"},
		"unknown version":  {`{name: double, version: 9, input: {n: 1}}`, "E_SUBWORKFLOW_NOT_FOUND"},
		"invalid input":    {`{name: double, input: {n: "x"}}`, "E_SUBWORKFLOW_INPUT"},
		"missing input":    {`{name: double}`, "E_SUBWORKFLOW_INPUT"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, ctx)
			h.mustPublish(doubleWorkflow)
			h.mustPublish(`
apiVersion: workflow/v1
name: caller
nodes:
  - id: call
    type: workflow
    retry: {max_attempts: 3, backoff: fixed, initial: 10ms}
    config: ` + tt.config + `
`)
			h.startWorkers(1)
			runID := h.start("caller", nil)
			h.waitStatus(runID, 15*time.Second, "failed")
			attempts := h.attempts(runID, "call")
			if len(attempts) != 1 || attempts[0]["code"] != tt.code {
				t.Fatalf("attempts = %v, want one attempt failing %s", attempts, tt.code)
			}
			if kids := h.children(runID); len(kids) != 0 {
				t.Fatalf("children = %v, want none", kids)
			}
		})
	}
}

// A workflow that calls itself is stopped by the nesting limit instead of
// running away; the failure travels up through every level.
func TestSubworkflowDepthLimitStopsRecursion(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(`
apiVersion: workflow/v1
name: recurse
nodes:
  - {id: again, type: workflow, config: {name: recurse}}
`)
	h.startWorkers(4)
	runID := h.start("recurse", nil)
	h.waitStatus(runID, 30*time.Second, "failed")
	var total int
	if err := h.pool.QueryRow(ctx, `select count(*) from runs where workflow_id = (select workflow_id from runs where id = $1)`, runID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if want := runs.MaxSubworkflowDepth + 1; total != want {
		t.Fatalf("runs = %d, want the top run plus %d nested levels (%d)", total, runs.MaxSubworkflowDepth, want)
	}
	var code string
	err := h.pool.QueryRow(ctx, `
		select ne.error->>'code' from node_executions ne join runs r on r.id = ne.run_id
		 where ne.node_id = 'again' and ne.status = 'failed' and not exists (select 1 from runs c where c.parent_run_id = r.id)`).Scan(&code)
	if err != nil || code != "E_SUBWORKFLOW_DEPTH" {
		t.Fatalf("deepest failure = %q, %v; want E_SUBWORKFLOW_DEPTH", code, err)
	}
}

// StartChild is idempotent on the parent node's key: a retried or redelivered
// attempt reuses the run the first one created.
func TestSubworkflowStartChildIsIdempotent(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(doubleWorkflow)
	h.mustPublish(`
apiVersion: workflow/v1
name: holder
nodes:
  - {id: a, type: script, config: {}}
`)
	parent := h.start("holder", nil)
	req := executor.ChildRequest{
		TenantID: uuidOf(h.tenant), ParentRunID: uuidOf(parent), Workflow: "double",
		Input: map[string]any{"n": 3}, IdempotencyKey: "wf_node_key",
	}
	first, err := h.flow.StartChild(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.flow.StartChild(ctx, req)
	if err != nil || second != first {
		t.Fatalf("second start = %v, %v; want the first run %v", second, err, first)
	}
	if kids := h.children(parent); len(kids) != 1 {
		t.Fatalf("children = %v, want exactly one", kids)
	}
	req.Input = map[string]any{"n": 4}
	var perm *executor.PermanentError
	if _, err := h.flow.StartChild(ctx, req); !errors.As(err, &perm) || perm.Code != "E_SUBWORKFLOW_CONFLICT" {
		t.Fatalf("start with different input = %v, want E_SUBWORKFLOW_CONFLICT", err)
	}
}

// foreach over a workflow node gives each element a multi-step body.
func TestSubworkflowInsideForeach(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(doubleWorkflow)
	h.mustPublish(`
apiVersion: workflow/v1
name: map-double
inputs: {numbers: {type: array}}
nodes:
  - id: each
    type: workflow
    foreach: {items: '${input.numbers}'}
    config:
      name: double
      input: {n: '${item}'}
  - id: collect
    type: transform
    needs: [each]
    config: {value: '${nodes.each.output.map(o, o.output.result)}'}
outputs: {doubled: '${nodes.collect.output}'}
`)
	h.startWorkers(4)
	runID := h.start("map-double", map[string]any{"numbers": []any{1, 2, 3, 4}})
	if status := h.waitStatus(runID, 20*time.Second, "succeeded", "failed"); status != "succeeded" {
		t.Fatalf("run = %s, nodes %v", status, h.nodeStatuses(runID))
	}
	out, _ := h.runOutput(runID)
	doubled := out["doubled"].([]any)
	if len(doubled) != 4 || doubled[0] != float64(2) || doubled[3] != float64(8) {
		t.Fatalf("outputs = %v", out)
	}
	if kids := h.children(runID); len(kids) != 4 {
		t.Fatalf("children = %d, want one per element", len(kids))
	}
	h.checkInvariants(runID)
}

// If propagation is lost (a crash right after the child commits), the sweeper
// pass still completes the parent's node.
func TestSubworkflowSweeperCompletesMissedPropagation(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(slowWorkflow)
	h.mustPublish(`
apiVersion: workflow/v1
name: waits-for-slow
nodes:
  - {id: call, type: workflow, config: {name: slow}}
`)
	h.startWorkers(2)
	runID := h.start("waits-for-slow", nil)
	child := h.childRun(runID)
	waitNode(t, h, idString(child), "hold", "waiting")

	// The workers' own sweepers would repair this within a moment, so stop them
	// and end the child behind the service's back: nothing notifies the parent.
	h.stopWorkers()
	if _, err := h.pool.Exec(ctx, `update runs set status = 'succeeded', output = '{"x":1}', ended_at = now() where id = $1`, child); err != nil {
		t.Fatal(err)
	}
	if got := h.nodeStatuses(runID)["call"]; got != "waiting" {
		t.Fatalf("call = %s before the sweep, want waiting", got)
	}
	done, err := h.flow.PropagateFinishedChildren(ctx)
	if err != nil || done != 1 {
		t.Fatalf("sweep handled %d, %v; want 1", done, err)
	}
	if status := h.waitStatus(runID, 10*time.Second, "succeeded", "failed"); status != "succeeded" {
		t.Fatalf("run = %s", status)
	}
	if again, err := h.flow.PropagateFinishedChildren(ctx); err != nil || again != 0 {
		t.Fatalf("second sweep handled %d, %v; want 0", again, err)
	}
}

// Many parents whose children finish almost instantly: whichever side of the
// parent's wait registration the child finishes on, every parent completes.
func TestSubworkflowManyConcurrentParents(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(doubleWorkflow)
	h.mustPublish(parentWorkflow)
	h.startWorkers(8)
	const parents = 30
	ids := make([]pgtype.UUID, parents)
	for i := range ids {
		ids[i] = h.start("parent", map[string]any{"n": i})
	}
	for i, id := range ids {
		if status := h.waitStatus(id, 60*time.Second, "succeeded", "failed"); status != "succeeded" {
			t.Fatalf("parent %d = %s, nodes %v", i, status, h.nodeStatuses(id))
		}
		if out, _ := h.runOutput(id); out["doubled"] != float64(i*2) {
			t.Fatalf("parent %d outputs = %v", i, out)
		}
		if kids := h.children(id); len(kids) != 1 {
			t.Fatalf("parent %d has %d children", i, len(kids))
		}
		h.checkInvariants(id)
	}
}

// A workflow can only call workflows of its own organization.
func TestSubworkflowDoesNotCrossOrganizations(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	if _, err := h.publish.Publish(ctx, workflows.PublishParams{TenantID: newUUID(t), Source: []byte(doubleWorkflow)}); err != nil {
		t.Fatal(err)
	}
	h.mustPublish(parentWorkflow) // this organization has no "double"
	h.startWorkers(1)
	runID := h.start("parent", map[string]any{"n": 1})
	h.waitStatus(runID, 15*time.Second, "failed")
	if attempts := h.attempts(runID, "call"); len(attempts) != 1 || attempts[0]["code"] != "E_SUBWORKFLOW_NOT_FOUND" {
		t.Fatalf("attempts = %v", attempts)
	}
}

// The engine's own wait keys cannot be chosen by an author or delivered by a
// caller.
func TestReservedWaitKeyPrefixes(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(`
apiVersion: workflow/v1
name: sneaky
nodes:
  - {id: w, type: wait_event, config: {key: 'task:forged', timeout: 1h}}
`)
	h.startWorkers(1)
	runID := h.start("sneaky", nil)
	h.waitStatus(runID, 10*time.Second, "failed")
	if attempts := h.attempts(runID, "w"); len(attempts) != 1 || attempts[0]["code"] != "bad_config" {
		t.Fatalf("attempts = %v", attempts)
	}

	c := apiClient{t: t, base: h.apiServer(nil), tenant: h.tenant}
	for _, key := range []string{"task:abc", "subworkflow:abc"} {
		if status, body := c.json("POST", "/be/v1/signals", map[string]any{"key": key}); status != 400 {
			t.Fatalf("signal %q = %d %v, want 400", key, status, body)
		}
	}
}

func uuidOf(id pgtype.UUID) uuid.UUID { return uuid.UUID(id.Bytes) }
