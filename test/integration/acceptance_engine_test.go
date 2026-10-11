//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/internal/services/runs"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/db"
	"github.com/kartikey/workflow_service/pkg/obs"
	"github.com/kartikey/workflow_service/pkg/uid"
)

// M2 fencing: the lease is lost mid-execution. The stale worker's result is
// discarded (its fenced write cannot land), the node is re-delivered as the
// same attempt, and only the second execution's output is kept.
func TestLostLeaseResultIsDiscarded(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(`
apiVersion: workflow/v1
name: fenced
nodes:
  - {id: work, type: script, config: {}}
outputs: {by: '${nodes.work.output.by}'}
`)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	h.script.set("work", func(_ context.Context, _ executor.Request) (executor.Result, error) {
		call := calls.Add(1)
		started <- struct{}{}
		if call == 1 {
			<-release // ignores cancellation, like a slow upstream call
			return executor.Result{Output: json.RawMessage(`{"by":"stale"}`)}, nil
		}
		return executor.Result{Output: json.RawMessage(`{"by":"fresh"}`)}, nil
	})
	lostBefore := testutil.ToFloat64(obs.LeaseLost)
	h.startWorkers(2)
	runID := h.start("fenced", nil)
	<-started
	if _, err := h.pool.Exec(ctx, `update node_executions set lease_expires_at = now() - interval '1 second' where run_id = $1`, runID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started: // the sweeper reclaimed the row and another worker ran it
	case <-time.After(10 * time.Second):
		t.Fatal("the expired lease was never re-delivered")
	}
	if status := h.waitStatus(runID, 10*time.Second, "succeeded", "failed"); status != "succeeded" {
		t.Fatalf("run = %s", status)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for testutil.ToFloat64(obs.LeaseLost) == lostBefore {
		if time.Now().After(deadline) {
			t.Fatalf("the stale result was not counted as a lost lease (calls %d, lost %v→%v)", calls.Load(), lostBefore, testutil.ToFloat64(obs.LeaseLost))
		}
		time.Sleep(20 * time.Millisecond)
	}
	output, _ := h.runOutput(runID)
	var rows, attempt int
	_ = h.pool.QueryRow(ctx, `select count(*), max(attempt) from node_executions where run_id = $1`, runID).Scan(&rows, &attempt)
	if output["by"] != "fresh" || rows != 1 || attempt != 1 {
		t.Fatalf("output %v, rows %d, attempt %d; want the fresh result on one row, attempt 1", output, rows, attempt)
	}
	h.checkInvariants(runID)
}

// M2: the run status is derived from the latest attempt of every node (01 §3.2)
// for every node population.
func TestRunStatusTransitions(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(`
apiVersion: workflow/v1
name: populations
nodes:
  - {id: a, type: script, config: {}}
  - {id: b, type: script, config: {}}
  - {id: handled, type: script, on_failure: handler, config: {}}
  - {id: handler, type: script, needs: [handled], config: {}}
`)
	cases := []struct {
		name  string
		nodes map[string]string // node → status of its latest attempt
		want  string
	}{
		{"ready keeps running", map[string]string{"a": "ready", "b": "waiting"}, "running"},
		{"running wins over waiting", map[string]string{"a": "running", "b": "waiting"}, "running"},
		{"only waiting", map[string]string{"a": "succeeded", "b": "waiting"}, "waiting"},
		{"unhandled failure", map[string]string{"a": "failed", "b": "succeeded"}, "failed"},
		{"handled failure", map[string]string{"a": "succeeded", "b": "skipped", "handled": "failed", "handler": "succeeded"}, "succeeded"},
		{"all terminal", map[string]string{"a": "succeeded", "b": "skipped"}, "succeeded"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run, err := h.flow.Start(ctx, runs.StartParams{TenantID: h.tenant, Workflow: "populations"})
			if err != nil {
				t.Fatal(err)
			}
			runID := run.Run.ID
			if _, err := h.pool.Exec(ctx, `delete from node_executions where run_id = $1`, runID); err != nil {
				t.Fatal(err)
			}
			for node, status := range c.nodes {
				owner := pgtype.Text{}
				if status == "running" {
					owner = pgtype.Text{String: "w", Valid: true}
				}
				var ended any
				if status != "ready" && status != "running" && status != "waiting" {
					ended = time.Now()
				}
				if _, err := h.pool.Exec(ctx, `insert into node_executions (id, run_id, tenant_id, node_id, node_type, status,
					idempotency_key, lease_owner, lease_expires_at, ended_at, error)
					values ($1, $2, $3, $4, 'script', $5, $6, $7, case when $7::text is null then null else now() + interval '1 minute' end, $8,
					case when $5 = 'failed' then '{"code":"boom","message":"boom"}'::jsonb end)`,
					uid.New(), runID, h.tenant, node, status, runs.NodeKey(runID, node, ""), owner, ended); err != nil {
					t.Fatal(err)
				}
			}
			err = db.InTx(ctx, h.pool, func(tx pgx.Tx) error {
				q := h.flow.Q.WithTx(tx)
				locked, err := runs.LockRun(ctx, q, runID)
				if err != nil {
					return err
				}
				compiled, err := h.flow.Specs.Get(ctx, q, locked.WorkflowVersionID, h.tenant)
				if err != nil {
					return err
				}
				return h.flow.Recompute(ctx, q, locked, compiled)
			})
			if err != nil {
				t.Fatal(err)
			}
			var status string
			if err := h.pool.QueryRow(ctx, `select status from runs where id = $1`, runID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status != c.want {
				t.Fatalf("run status = %s, want %s", status, c.want)
			}
		})
	}
}

// M6 (I6): seq stays gap-free with 8 workers racing to finish nodes of one run.
func TestEventSeqGapFreeUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	var yaml strings.Builder
	yaml.WriteString("apiVersion: workflow/v1\nname: wide\nnodes:\n  - {id: root, type: script, config: {}}\n")
	needs := make([]string, 0, 40)
	for i := range 40 {
		fmt.Fprintf(&yaml, "  - {id: n%d, type: script, needs: [root], config: {}}\n", i)
		needs = append(needs, fmt.Sprintf("n%d", i))
	}
	fmt.Fprintf(&yaml, "  - {id: join, type: script, needs: [%s], config: {}}\n", strings.Join(needs, ", "))
	h.mustPublish(yaml.String())
	h.startWorkers(8)
	for range 5 {
		runID := h.start("wide", nil)
		if status := h.waitStatus(runID, 20*time.Second, "succeeded", "failed"); status != "succeeded" {
			t.Fatalf("run = %s", status)
		}
		var events, maxSeq int
		if err := h.pool.QueryRow(ctx, `select count(*), max(seq) from run_events where run_id = $1`, runID).Scan(&events, &maxSeq); err != nil {
			t.Fatal(err)
		}
		if events != maxSeq || events < 42*3 {
			t.Fatalf("events = %d, max seq = %d", events, maxSeq)
		}
		h.checkInvariants(runID)
	}
}

// M6: cancelling a run whose node is mid-flight settles within one heartbeat,
// and a result returned after the cancel is discarded, never recorded.
func TestCancelDiscardsInFlightResult(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(`
apiVersion: workflow/v1
name: cancel-me
nodes:
  - {id: slow, type: script, config: {}}
  - {id: after, type: script, needs: [slow], config: {}}
`)
	started := make(chan struct{})
	release := make(chan struct{})
	h.script.set("slow", func(context.Context, executor.Request) (executor.Result, error) {
		close(started)
		<-release // ignores cancellation
		return executor.Result{Output: json.RawMessage(`{"late":true}`)}, nil
	})
	h.startWorkers(1)
	runID := h.start("cancel-me", nil)
	<-started
	cancelled := time.Now()
	if err := h.flow.Cancel(ctx, h.tenant, runID, "test"); err != nil {
		t.Fatal(err)
	}
	close(release)
	heartbeat := workerConfig("x").LeaseTTL / 3
	deadline := cancelled.Add(heartbeat + time.Second)
	for h.nodeStatuses(runID)["slow"] != "cancelled" {
		if time.Now().After(deadline) {
			t.Fatalf("node did not settle within a heartbeat: %v", h.nodeStatuses(runID))
		}
		time.Sleep(20 * time.Millisecond)
	}
	var output []byte
	if err := h.pool.QueryRow(ctx, `select output from node_executions where run_id = $1 and node_id = 'slow'`, runID).Scan(&output); err != nil {
		t.Fatal(err)
	}
	if output != nil || h.script.callCount("after") != 0 {
		t.Fatalf("in-flight result was recorded (%s) or the run advanced", output)
	}
	h.checkInvariants(runID)
}

// M6: every metric in 05 §6 is emitted by an engine that has done the work
// each one describes.
func TestEveryMetricIsEmitted(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(`
apiVersion: workflow/v1
name: metrics
nodes:
  - id: flaky
    type: script
    retry: {max_attempts: 2, backoff: fixed, initial: 10ms, max: 10ms}
    config: {}
  - {id: await, type: wait_event, needs: [flaky], config: {key: metrics-key, timeout: 1h}}
`)
	var calls atomic.Int32
	h.script.set("flaky", func(context.Context, executor.Request) (executor.Result, error) {
		if calls.Add(1) == 1 {
			return executor.Result{}, executor.Retryable("upstream_5xx", fmt.Errorf("503"))
		}
		return executor.Result{Output: json.RawMessage(`{}`)}, nil
	})
	obs.RegisterEngineGauges(h.pool)
	h.startWorkers(1)
	runID := h.start("metrics", nil)
	waitNode(t, h, idString(runID), "await", "waiting")
	if _, err := h.flow.Signal(ctx, h.tenant, "metrics-key", nil); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(runID, 10*time.Second, "succeeded")
	_, _ = h.flow.Signal(ctx, h.tenant, "no-such-key", nil)

	families, err := obs.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, family := range families {
		have[family.GetName()] = true
	}
	for _, name := range []string{
		"wf_nodes_claimed_total", "wf_node_duration_seconds", "wf_node_attempts_total",
		"wf_lease_expired_total", "wf_lease_lost_total", "wf_claim_batch_size", "wf_ready_backlog",
		"wf_runs_active", "wf_run_duration_seconds", "wf_signal_total", "wf_sweeper_actions_total",
	} {
		if !have[name] {
			t.Errorf("metric %s is not emitted", name)
		}
	}
	h.checkInvariants(runID)
	_ = gen.Run{}
}
