//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/internal/services/workflows"
	"github.com/kartikey/workflow_service/internal/spec"
)

// M4: join: all after a branch is skipped (and the compiler warned), while the
// untaken arm's whole subtree is skipped; if: false cascades the same way.
func TestBranchJoinAllSkippedAndIfFalseCascades(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	source := `
apiVersion: workflow/v1
name: join-all-branch
nodes:
  - id: route
    type: branch
    config:
      cases: [{when: '${true}', goto: left}]
      default: right
  - {id: left, type: script, needs: [route], config: {}}
  - {id: right, type: script, needs: [route], config: {}}
  - {id: right_child, type: script, needs: [right], config: {}}
  - {id: merge_all, type: script, needs: [left, right_child], config: {}}
  - {id: after_merge, type: script, needs: [merge_all], config: {}}
`
	result, err := h.publish.Publish(ctx, workflows.PublishParams{TenantID: h.tenant, Source: []byte(source)})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(result.Diagnostics, func(d spec.Diagnostic) bool { return d.Code == "W_JOIN_ALL_ACROSS_BRANCH" && d.NodeID == "merge_all" }) {
		t.Fatalf("publish warnings = %+v, want W_JOIN_ALL_ACROSS_BRANCH on merge_all", result.Diagnostics)
	}
	h.mustPublish(`
apiVersion: workflow/v1
name: if-false
nodes:
  - {id: start, type: script, config: {}}
  - {id: gated, type: script, needs: [start], if: '${false}', config: {}}
  - {id: child, type: script, needs: [gated], config: {}}
  - {id: grandchild, type: script, needs: [child], config: {}}
  - {id: merge_any, type: script, needs: [start, grandchild], join: any, config: {}}
`)
	h.startWorkers(2)
	branchRun := h.start("join-all-branch", nil)
	h.waitStatus(branchRun, 10*time.Second, "succeeded", "failed")
	want := map[string]string{"route": "succeeded", "left": "succeeded", "right": "skipped", "right_child": "skipped", "merge_all": "skipped", "after_merge": "skipped"}
	if got := h.nodeStatuses(branchRun); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("branch nodes = %v, want %v", got, want)
	}
	h.checkInvariants(branchRun)

	ifRun := h.start("if-false", nil)
	if status := h.waitStatus(ifRun, 10*time.Second, "succeeded", "failed"); status != "succeeded" {
		t.Fatalf("if-false run = %s", status)
	}
	want = map[string]string{"start": "succeeded", "gated": "skipped", "child": "skipped", "grandchild": "skipped", "merge_any": "succeeded"}
	if got := h.nodeStatuses(ifRun); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("if-false nodes = %v, want %v", got, want)
	}
	if h.script.callCount("gated") != 0 || h.script.callCount("child") != 0 {
		t.Fatal("a skipped node executed")
	}
	h.checkInvariants(ifRun)
}

// M4: an on_failure that loops back is a cycle the compiler rejects, and a run
// that would fill the table trips E_RUN_BUDGET, including through retries.
func TestRunBudget(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	_, err := h.publish.Publish(ctx, workflows.PublishParams{TenantID: h.tenant, Source: []byte(`
apiVersion: workflow/v1
name: loop-back
nodes:
  - {id: start, type: script, config: {}}
  - {id: work, type: script, needs: [start, compensate], on_failure: compensate, config: {}}
  - {id: compensate, type: script, needs: [work], config: {}}
`)})
	var compileErr *workflows.CompileError
	if !errors.As(err, &compileErr) || !slices.ContainsFunc(compileErr.Diagnostics, func(d spec.Diagnostic) bool { return d.Code == "E_CYCLE" }) {
		t.Fatalf("loop-back publish err = %v, want E_CYCLE", err)
	}

	h.flow.MaxNodesPerRun = 4
	h.mustPublish(`
apiVersion: workflow/v1
name: too-long
nodes:
  - {id: n1, type: script, config: {}}
  - {id: n2, type: script, needs: [n1], config: {}}
  - {id: n3, type: script, needs: [n2], config: {}}
  - {id: n4, type: script, needs: [n3], config: {}}
  - {id: n5, type: script, needs: [n4], config: {}}
  - {id: n6, type: script, needs: [n5], config: {}}
`)
	h.mustPublish(`
apiVersion: workflow/v1
name: poison
nodes:
  - id: poison
    type: script
    retry: {max_attempts: 20, backoff: fixed, initial: 1ms, max: 1ms}
    config: {}
`)
	h.script.set("poison", func(context.Context, executor.Request) (executor.Result, error) {
		return executor.Result{}, executor.Retryable("upstream_5xx", errors.New("still down"))
	})
	h.startWorkers(2)
	for _, name := range []string{"too-long", "poison"} {
		runID := h.start(name, nil)
		if status := h.waitStatus(runID, 10*time.Second, "succeeded", "failed"); status != "failed" {
			t.Fatalf("%s run = %s", name, status)
		}
		_, runErr := h.runOutput(runID)
		var rows int
		_ = h.pool.QueryRow(ctx, `select count(*) from node_executions where run_id = $1`, runID).Scan(&rows)
		if runErr["code"] != "E_RUN_BUDGET" || rows > h.flow.MaxNodesPerRun+1 {
			t.Fatalf("%s: error %v with %d rows, want E_RUN_BUDGET within budget", name, runErr, rows)
		}
		h.checkInvariants(runID)
	}
}

// M5 / v1 DoD: a parked node holds one row, no lease and no goroutine. A
// 30-day sleep is "fast-forwarded" by moving run_after, never by sleeping.
func TestParkedNodesHoldNoLeaseOrGoroutine(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(`
apiVersion: workflow/v1
name: followup
timeout: 2160h
nodes:
  - {id: wait_30d, type: sleep, config: {duration: 720h}}
  - {id: call, type: script, needs: [wait_30d], config: {}}
`)
	h.startWorkers(2)
	time.Sleep(300 * time.Millisecond) // let the worker goroutines settle
	before := runtime.NumGoroutine()
	const parked = 50
	runs := make([]pgtype.UUID, parked)
	for i := range runs {
		runs[i] = h.start("followup", nil)
	}
	for _, id := range runs {
		waitNode(t, h, idString(id), "wait_30d", "waiting")
	}
	time.Sleep(300 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+5 {
		t.Fatalf("goroutines grew from %d to %d with %d parked nodes", before, after, parked)
	}
	var rows, leased int
	if err := h.pool.QueryRow(ctx, `select count(*), count(lease_owner) from node_executions where status = 'waiting'`).Scan(&rows, &leased); err != nil {
		t.Fatal(err)
	}
	if rows != parked || leased != 0 {
		t.Fatalf("parked rows = %d, leased = %d; want %d rows and no leases", rows, leased, parked)
	}
	var until time.Time
	if err := h.pool.QueryRow(ctx, `select run_after from node_executions where run_id = $1 and node_id = 'wait_30d'`, runs[0]).Scan(&until); err != nil {
		t.Fatal(err)
	}
	if d := time.Until(until); d < 719*time.Hour || d > 721*time.Hour {
		t.Fatalf("sleep parks until %v from now, want 720h", d)
	}
	// Thirty days pass.
	if _, err := h.pool.Exec(ctx, `update node_executions set run_after = now() where node_id = 'wait_30d' and status = 'waiting'`); err != nil {
		t.Fatal(err)
	}
	for _, id := range runs {
		if status := h.waitStatus(id, 15*time.Second, "succeeded", "failed"); status != "succeeded" {
			t.Fatalf("run = %s", status)
		}
		h.checkInvariants(id)
	}
}

// M5: a signal completes the node and enqueues its successor in the same
// transaction, before any worker runs; a second live waiter on the same key
// fails E_WAIT_KEY_CONFLICT at wait time.
func TestSignalAdvancesInOneTransactionAndWaitKeyConflict(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(`
apiVersion: workflow/v1
name: pickup
nodes:
  - {id: await, type: wait_event, config: {key: shared-key, timeout: 1h}}
  - {id: next, type: script, needs: [await], config: {}}
`)
	h.startWorkers(1)
	first := h.start("pickup", nil)
	waitNode(t, h, idString(first), "await", "waiting")
	second := h.start("pickup", nil)
	if status := h.waitStatus(second, 10*time.Second, "failed"); status != "failed" {
		t.Fatalf("second run = %s", status)
	}
	if rows := h.attempts(second, "await"); len(rows) != 1 || rows[0]["code"] != "E_WAIT_KEY_CONFLICT" {
		t.Fatalf("second run attempts = %v", rows)
	}
	h.checkInvariants(second)

	h.stopWorkers()
	if _, err := h.flow.Signal(ctx, h.tenant, "shared-key", json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	// No worker is running: the successor must already exist, written by the
	// signal's own transaction.
	if got := h.nodeStatuses(first); got["await"] != "succeeded" || got["next"] != "ready" {
		t.Fatalf("after signal: %v, want await succeeded and next ready", got)
	}
	h.startWorkers(1)
	h.waitStatus(first, 10*time.Second, "succeeded")
	h.checkInvariants(first)
}

// M5: a wait_event timeout routes to on_timeout; without it the node fails.
// A passed deadline fails the run and cancels its pending nodes.
func TestWaitTimeoutsAndRunDeadline(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(`
apiVersion: workflow/v1
name: timeout-routed
nodes:
  - {id: await, type: wait_event, on_timeout: reassign, config: {key: 'routed', timeout: 1h}}
  - {id: reassign, type: script, needs: [await], join: any, config: {}}
  - {id: notify, type: script, needs: [await], config: {}}
`)
	h.mustPublish(`
apiVersion: workflow/v1
name: timeout-unhandled
nodes:
  - {id: await, type: wait_event, config: {key: 'unhandled', timeout: 1h}}
  - {id: notify, type: script, needs: [await], config: {}}
`)
	h.mustPublish(`
apiVersion: workflow/v1
name: deadline
timeout: 1h
nodes:
  - {id: a, type: script, config: {}}
  - {id: nap, type: sleep, needs: [a], config: {duration: 30m}}
  - {id: b, type: script, needs: [nap], config: {}}
`)
	h.startWorkers(1)
	routed := h.start("timeout-routed", nil)
	unhandled := h.start("timeout-unhandled", nil)
	deadline := h.start("deadline", nil)
	waitNode(t, h, idString(routed), "await", "waiting")
	waitNode(t, h, idString(unhandled), "await", "waiting")
	waitNode(t, h, idString(deadline), "nap", "waiting")
	// The hour passes for the waits; the run deadline passes for the third run.
	if _, err := h.pool.Exec(ctx, `update node_executions set run_after = now() where node_id = 'await' and status = 'waiting'`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx, `update runs set deadline_at = now() where id = $1`, deadline); err != nil {
		t.Fatal(err)
	}

	if status := h.waitStatus(routed, 10*time.Second, "succeeded", "failed"); status != "succeeded" {
		t.Fatalf("routed run = %s", status)
	}
	want := map[string]string{"await": "failed", "reassign": "succeeded", "notify": "skipped"}
	if got := h.nodeStatuses(routed); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("routed nodes = %v, want %v", got, want)
	}
	if status := h.waitStatus(unhandled, 10*time.Second, "succeeded", "failed"); status != "failed" {
		t.Fatalf("unhandled run = %s", status)
	}
	if rows := h.attempts(unhandled, "await"); rows[0]["code"] != "wait_timeout" {
		t.Fatalf("unhandled await = %v", rows)
	}
	if status := h.waitStatus(deadline, 10*time.Second, "succeeded", "failed"); status != "failed" {
		t.Fatalf("deadline run = %s", status)
	}
	_, runErr := h.runOutput(deadline)
	if runErr["code"] != "run_deadline_exceeded" || h.nodeStatuses(deadline)["nap"] != "cancelled" {
		t.Fatalf("deadline run error %v, nodes %v", runErr, h.nodeStatuses(deadline))
	}
	for _, id := range []pgtype.UUID{routed, unhandled, deadline} {
		h.checkInvariants(id)
	}
}

func idString(id pgtype.UUID) string {
	b := id.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
