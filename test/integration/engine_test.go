//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/kartikey/workflow_service/internal/engine"
	"github.com/kartikey/workflow_service/internal/executor"
	"github.com/kartikey/workflow_service/internal/executors/core"
	"github.com/kartikey/workflow_service/internal/flow"
	"github.com/kartikey/workflow_service/internal/workflows"
	"github.com/kartikey/workflow_service/pkg/config"
)

// scripted runs a per-node behaviour so engine tests can fail, hang or count.
type scripted struct {
	mu       sync.Mutex
	behave   map[string]func(context.Context, executor.Request) (executor.Result, error)
	calls    map[string]int
	keys     map[string][]string
	executed atomic.Int64
}

func newScripted() *scripted {
	return &scripted{behave: map[string]func(context.Context, executor.Request) (executor.Result, error){}, calls: map[string]int{}, keys: map[string][]string{}}
}

func (*scripted) Type() string                         { return "script" }
func (*scripted) ValidateConfig(json.RawMessage) error { return nil }
func (s *scripted) Execute(ctx context.Context, req executor.Request) (executor.Result, error) {
	s.executed.Add(1)
	s.mu.Lock()
	s.calls[req.NodeID]++
	s.keys[req.NodeID] = append(s.keys[req.NodeID], req.IdempotencyKey)
	behave := s.behave[req.NodeID]
	s.mu.Unlock()
	if behave != nil {
		return behave(ctx, req)
	}
	return executor.Result{Output: json.RawMessage(fmt.Sprintf(`{"node":%q}`, req.NodeID))}, nil
}

func (s *scripted) set(nodeID string, fn func(context.Context, executor.Request) (executor.Result, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.behave[nodeID] = fn
}

func (s *scripted) callCount(nodeID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[nodeID]
}

type harness struct {
	t        *testing.T
	pool     *pgxpool.Pool
	flow     *flow.Flow
	script   *scripted
	tenant   pgtype.UUID
	registry *executor.Registry
	publish  *workflows.Service
	stop     []func()
}

func newHarness(t *testing.T, ctx context.Context) *harness {
	t.Helper()
	pool := newIsolatedPool(t, ctx)
	f, err := flow.New(pool, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	script := newScripted()
	registry := executor.NewRegistry()
	registry.MustRegister(core.All()...)
	registry.MustRegister(script)
	h := &harness{t: t, pool: pool, flow: f, script: script, tenant: newUUID(t), registry: registry, publish: workflows.NewService(pool, registry)}
	t.Cleanup(h.stopWorkers)
	return h
}

func workerConfig(id string) config.WorkerConfig {
	return config.WorkerConfig{
		ID: id, Concurrency: 8, BatchSize: 4, PollInterval: 50 * time.Millisecond,
		LeaseTTL: 3 * time.Second, SweepInterval: 200 * time.Millisecond,
	}
}

// startWorkers runs n workers until the test ends.
func (h *harness) startWorkers(n int) {
	for i := range n {
		cfg := workerConfig(fmt.Sprintf("worker-%d-%s", i, randomHex(h.t, 3)))
		dispatcher := engine.NewDispatcher(cfg, h.flow, h.registry, nil, zap.NewNop())
		worker := engine.NewWorker(cfg, zap.NewNop(), h.pool, h.flow, dispatcher.Run)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = worker.Run(ctx, 2*time.Second)
		}()
		h.stop = append(h.stop, func() { cancel(); <-done })
	}
}

func (h *harness) stopWorkers() {
	for _, stop := range h.stop {
		stop()
	}
	h.stop = nil
}

func (h *harness) mustPublish(source string) {
	h.t.Helper()
	_, err := h.publish.Publish(context.Background(), workflows.PublishParams{TenantID: h.tenant, Source: []byte(source)})
	if err != nil {
		var compileErr *workflows.CompileError
		if errors.As(err, &compileErr) {
			h.t.Fatalf("publish: %v", compileErr.Diagnostics)
		}
		h.t.Fatalf("publish: %v", err)
	}
}

func (h *harness) start(workflow string, input map[string]any) pgtype.UUID {
	h.t.Helper()
	result, err := h.flow.Start(context.Background(), flow.StartParams{TenantID: h.tenant, Workflow: workflow, Input: input})
	if err != nil {
		h.t.Fatalf("start run: %v", err)
	}
	return result.Run.ID
}

func (h *harness) waitStatus(runID pgtype.UUID, timeout time.Duration, statuses ...string) string {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var status string
		if err := h.pool.QueryRow(context.Background(), `select status from runs where id = $1`, runID).Scan(&status); err != nil {
			h.t.Fatal(err)
		}
		for _, want := range statuses {
			if status == want {
				return status
			}
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("run status = %q after %v, want one of %v; nodes: %v", status, timeout, statuses, h.nodeStatuses(runID))
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (h *harness) nodeStatuses(runID pgtype.UUID) map[string]string {
	rows, err := h.pool.Query(context.Background(), `
		select distinct on (node_id) node_id, status from node_executions
		 where run_id = $1 order by node_id, attempt desc`, runID)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			h.t.Fatal(err)
		}
		out[id] = status
	}
	return out
}

func (h *harness) runOutput(runID pgtype.UUID) (map[string]any, map[string]any) {
	var output, runError map[string]any
	if err := h.pool.QueryRow(context.Background(), `select output, error from runs where id = $1`, runID).Scan(&output, &runError); err != nil {
		h.t.Fatal(err)
	}
	return output, runError
}

// checkInvariants asserts I1, I3, I4, I5 and I6 (05 §4) for one run.
func (h *harness) checkInvariants(runID pgtype.UUID) {
	h.t.Helper()
	ctx := context.Background()
	checks := map[string]string{
		"I1 duplicate identity": `select count(*) from (select 1 from node_executions where run_id = $1
			group by node_id, branch_key, attempt having count(*) > 1) d`,
		"I3 lease iff running": `select count(*) from node_executions where run_id = $1
			and ((lease_owner is not null) <> (status = 'running'))`,
		"I4 terminal run with live nodes": `select count(*) from runs r join node_executions ne on ne.run_id = r.id
			where r.id = $1 and r.status in ('succeeded','failed','cancelled') and ne.status in ('ready','running','waiting')`,
		"I5 keys per node": `select count(*) from (select node_id from node_executions where run_id = $1
			group by node_id, branch_key having count(distinct idempotency_key) > 1) d`,
		"I6 seq gaps": `select count(*) from (select seq, row_number() over (order by seq) as n
			from run_events where run_id = $1) s where seq <> n`,
		"two succeeded attempts": `select count(*) from (select node_id from node_executions where run_id = $1 and status = 'succeeded'
			group by node_id, branch_key having count(*) > 1) d`,
	}
	for name, query := range checks {
		var violations int
		if err := h.pool.QueryRow(ctx, query, runID).Scan(&violations); err != nil {
			h.t.Fatalf("%s: %v", name, err)
		}
		if violations != 0 {
			h.t.Errorf("invariant %s violated %d time(s)", name, violations)
		}
	}
}

func TestEngineLinearWorkflowWithOutputs(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(`
apiVersion: workflow/v1
name: linear
inputs:
  name: {type: string, required: true}
  greeting: {type: string, default: Hello}
nodes:
  - id: greet
    type: transform
    config: {value: {text: "${input.greeting}, ${input.name}"}}
  - id: shout
    type: transform
    needs: [greet]
    config: {value: '${nodes.greet.output.text.upperAscii()}'}
  - id: count
    type: transform
    needs: [shout]
    config: {value: '${size(nodes.shout.output)}'}
outputs:
  message: ${nodes.shout.output}
  length: ${nodes.count.output}
`)
	h.startWorkers(1)
	runID := h.start("linear", map[string]any{"name": "Ada"})
	h.waitStatus(runID, 10*time.Second, "succeeded", "failed")
	output, runErr := h.runOutput(runID)
	if output["message"] != "HELLO, ADA" || output["length"] != float64(10) {
		t.Fatalf("output = %v, error = %v", output, runErr)
	}
	h.checkInvariants(runID)
}

func TestEngineDiamondRunsJoinExactlyOnce(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(`
apiVersion: workflow/v1
name: diamond
nodes:
  - {id: a, type: script, config: {}}
  - {id: b, type: script, needs: [a], config: {}}
  - {id: c, type: script, needs: [a], config: {}}
  - {id: d, type: script, needs: [b, c], config: {}}
`)
	h.startWorkers(8)
	const runs = 60
	ids := make([]pgtype.UUID, runs)
	for i := range runs {
		ids[i] = h.start("diamond", nil)
	}
	for _, id := range ids {
		if status := h.waitStatus(id, 30*time.Second, "succeeded", "failed"); status != "succeeded" {
			t.Fatalf("run %v = %s", id, status)
		}
		var count int
		if err := h.pool.QueryRow(ctx, `select count(*) from node_executions where run_id = $1 and node_id = 'd'`, id).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("d rows = %d, want exactly 1", count)
		}
		h.checkInvariants(id)
	}
	if got := h.script.callCount("d"); got != runs {
		t.Fatalf("d executed %d times for %d runs", got, runs)
	}
}

func TestEngineBranchSkipsUntakenArm(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(`
apiVersion: workflow/v1
name: triage
inputs:
  severity: {type: string, enum: [P0, P1]}
nodes:
  - id: route
    type: branch
    config:
      cases:
        - when: ${input.severity == "P0"}
          goto: page
      default: reply
  - {id: page, type: script, needs: [route], config: {}}
  - {id: reply, type: script, needs: [route], config: {}}
  - {id: after_reply, type: script, needs: [reply], config: {}}
  - {id: record, type: script, needs: [page, after_reply], join: any, config: {}}
`)
	h.startWorkers(2)
	runID := h.start("triage", map[string]any{"severity": "P0"})
	h.waitStatus(runID, 10*time.Second, "succeeded", "failed")
	want := map[string]string{"route": "succeeded", "page": "succeeded", "reply": "skipped", "after_reply": "skipped", "record": "succeeded"}
	if got := h.nodeStatuses(runID); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("nodes = %v, want %v", got, want)
	}
	h.checkInvariants(runID)
}

func TestEngineRetriesWithStableKeyThenSucceeds(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(`
apiVersion: workflow/v1
name: flaky
nodes:
  - id: call
    type: script
    retry: {max_attempts: 4, backoff: fixed, initial: 10ms, max: 10ms}
    config: {}
`)
	var attempts atomic.Int32
	h.script.set("call", func(context.Context, executor.Request) (executor.Result, error) {
		if attempts.Add(1) < 3 {
			return executor.Result{}, executor.Retryable("upstream_5xx", errors.New("503"))
		}
		return executor.Result{Output: json.RawMessage(`{"ok":true}`)}, nil
	})
	h.startWorkers(2)
	runID := h.start("flaky", nil)
	if status := h.waitStatus(runID, 10*time.Second, "succeeded", "failed"); status != "succeeded" {
		t.Fatalf("status = %s", status)
	}
	h.script.mu.Lock()
	keys := h.script.keys["call"]
	h.script.mu.Unlock()
	if len(keys) != 3 || keys[0] != keys[1] || keys[1] != keys[2] {
		t.Fatalf("idempotency keys across attempts = %v, want 3 identical", keys)
	}
	h.checkInvariants(runID)
}

func TestEngineFailureHandlingAndCascade(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(`
apiVersion: workflow/v1
name: compensate
nodes:
  - {id: charge, type: script, on_failure: refund, config: {}}
  - {id: ship, type: script, needs: [charge], config: {}}
  - {id: refund, type: script, needs: [charge], config: {}}
`)
	h.mustPublish(`
apiVersion: workflow/v1
name: unhandled
nodes:
  - {id: charge, type: script, config: {}}
  - {id: ship, type: script, needs: [charge], config: {}}
  - id: notify
    type: script
    needs: [charge]
    if: ${false}
    config: {}
`)
	h.script.set("charge", func(context.Context, executor.Request) (executor.Result, error) {
		return executor.Result{}, executor.Permanent("card_declined", errors.New("declined"))
	})
	h.startWorkers(2)

	handled := h.start("compensate", nil)
	if status := h.waitStatus(handled, 10*time.Second, "succeeded", "failed"); status != "succeeded" {
		t.Fatalf("handled run = %s, nodes %v", status, h.nodeStatuses(handled))
	}
	if got := h.nodeStatuses(handled); got["refund"] != "succeeded" || got["ship"] != "skipped" {
		t.Fatalf("handled nodes = %v", got)
	}
	h.checkInvariants(handled)

	failed := h.start("unhandled", nil)
	if status := h.waitStatus(failed, 10*time.Second, "succeeded", "failed"); status != "failed" {
		t.Fatalf("unhandled run = %s", status)
	}
	_, runErr := h.runOutput(failed)
	if runErr["node_id"] != "charge" || runErr["node_code"] != "card_declined" {
		t.Fatalf("run error = %v", runErr)
	}
	h.checkInvariants(failed)
}

func TestEngineSleepAndSignal(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(`
apiVersion: workflow/v1
name: approval
inputs:
  id: {type: string, required: true}
nodes:
  - {id: nap, type: sleep, config: {duration: 300ms}}
  - id: approve
    type: wait_event
    needs: [nap]
    config: {key: "approval:${input.id}", timeout: 1h}
  - id: done
    type: transform
    needs: [approve]
    config: {value: '${nodes.approve.output.by}'}
outputs:
  approved_by: ${nodes.done.output}
`)
	h.startWorkers(1)
	runID := h.start("approval", map[string]any{"id": "R-1"})
	h.waitStatus(runID, 10*time.Second, "waiting")
	if h.nodeStatuses(runID)["approve"] != "waiting" {
		// The run is waiting on the sleep first; wait for the wait_event to park.
		deadline := time.Now().Add(10 * time.Second)
		for h.nodeStatuses(runID)["approve"] != "waiting" {
			if time.Now().After(deadline) {
				t.Fatalf("approve never parked: %v", h.nodeStatuses(runID))
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	if _, err := h.flow.Signal(ctx, h.tenant, "approval:R-2", json.RawMessage(`{}`)); !errors.Is(err, flow.ErrNoWaiter) {
		t.Fatalf("signal to unknown key err = %v, want ErrNoWaiter", err)
	}
	if _, err := h.flow.Signal(ctx, h.tenant, "approval:R-1", json.RawMessage(`{"by":"grace"}`)); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(runID, 10*time.Second, "succeeded")
	output, _ := h.runOutput(runID)
	if output["approved_by"] != "grace" {
		t.Fatalf("output = %v", output)
	}
	duplicate, err := h.flow.Signal(ctx, h.tenant, "approval:R-1", json.RawMessage(`{"by":"grace"}`))
	if err != nil || !duplicate.Duplicate {
		t.Fatalf("duplicate signal = %+v, %v", duplicate, err)
	}
	h.checkInvariants(runID)
}

func TestEngineCancelStopsRunningNode(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(`
apiVersion: workflow/v1
name: slow
nodes:
  - {id: hang, type: script, config: {}}
  - {id: next, type: script, needs: [hang], config: {}}
`)
	stopped := make(chan struct{})
	h.script.set("hang", func(ctx context.Context, _ executor.Request) (executor.Result, error) {
		<-ctx.Done()
		close(stopped)
		return executor.Result{}, ctx.Err()
	})
	h.startWorkers(1)
	runID := h.start("slow", nil)
	deadline := time.Now().Add(5 * time.Second)
	for h.nodeStatuses(runID)["hang"] != "running" {
		if time.Now().After(deadline) {
			t.Fatal("hang never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := h.flow.Cancel(ctx, h.tenant, runID, "test"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("running node was not cancelled within a heartbeat")
	}
	deadline = time.Now().Add(5 * time.Second)
	for h.nodeStatuses(runID)["hang"] != "cancelled" {
		if time.Now().After(deadline) {
			t.Fatalf("nodes = %v, want hang cancelled", h.nodeStatuses(runID))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := h.flow.Cancel(ctx, h.tenant, runID, "again"); !errors.Is(err, flow.ErrRunTerminal) {
		t.Fatalf("second cancel err = %v", err)
	}
	h.checkInvariants(runID)
}

func TestEngineIdempotentStartAndInputValidation(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(`
apiVersion: workflow/v1
name: idem
inputs:
  order: {type: string, required: true}
  qty: {type: integer, minimum: 1}
correlation: ${input.order}
nodes:
  - {id: a, type: script, config: {}}
`)
	params := flow.StartParams{TenantID: h.tenant, Workflow: "idem", Input: map[string]any{"order": "O-1"}, IdempotencyKey: "k1"}
	first, err := h.flow.Start(ctx, params)
	if err != nil || !first.Created || first.Run.CorrelationID.String != "O-1" {
		t.Fatalf("first = %+v, %v", first, err)
	}
	second, err := h.flow.Start(ctx, params)
	if err != nil || second.Created || second.Run.ID != first.Run.ID {
		t.Fatalf("second = %+v, %v", second, err)
	}
	params.Input = map[string]any{"order": "O-2"}
	if _, err := h.flow.Start(ctx, params); !errors.Is(err, flow.ErrIdempotencyConflict) {
		t.Fatalf("conflicting reuse err = %v", err)
	}
	var inputErr *flow.InputError
	_, err = h.flow.Start(ctx, flow.StartParams{TenantID: h.tenant, Workflow: "idem", Input: map[string]any{"qty": 0}})
	if !errors.As(err, &inputErr) || len(inputErr.Fields) != 2 {
		t.Fatalf("input err = %v", err)
	}
}

func TestWorkerShutdownReleasesLeases(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(`
apiVersion: workflow/v1
name: handoff
nodes:
  - {id: long, type: script, config: {}}
`)
	release := make(chan struct{})
	h.script.set("long", func(ctx context.Context, _ executor.Request) (executor.Result, error) {
		select {
		case <-ctx.Done():
			return executor.Result{}, ctx.Err()
		case <-release:
			return executor.Result{Output: json.RawMessage(`{}`)}, nil
		}
	})
	cfg := workerConfig("leaving")
	dispatcher := engine.NewDispatcher(cfg, h.flow, h.registry, nil, zap.NewNop())
	worker := engine.NewWorker(cfg, zap.NewNop(), h.pool, h.flow, dispatcher.Run)
	workerCtx, stopWorker := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- worker.Run(workerCtx, 100*time.Millisecond) }()

	runID := h.start("handoff", nil)
	deadline := time.Now().Add(5 * time.Second)
	for h.nodeStatuses(runID)["long"] != "running" {
		if time.Now().After(deadline) {
			t.Fatal("node never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stopWorker()
	<-done
	var status string
	var owner pgtype.Text
	var attempt int32
	if err := h.pool.QueryRow(ctx, `select status, lease_owner, attempt from node_executions where run_id = $1`, runID).Scan(&status, &owner, &attempt); err != nil {
		t.Fatal(err)
	}
	if status != "ready" || owner.Valid || attempt != 1 {
		t.Fatalf("after shutdown: status=%s owner=%v attempt=%d; want ready, no owner, same attempt", status, owner, attempt)
	}
	close(release)
	h.startWorkers(1)
	h.waitStatus(runID, 10*time.Second, "succeeded")
	h.checkInvariants(runID)
}
