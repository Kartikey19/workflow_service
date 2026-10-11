//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/kartikey/workflow_service/internal/core/executor"
)

// echoItem makes the script node report the element it was given.
func echoItem(_ context.Context, req executor.Request) (executor.Result, error) {
	var config struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(req.Config, &config); err != nil {
		return executor.Result{}, err
	}
	out, _ := json.Marshal(map[string]any{"item": json.RawMessage(config.Value), "branch": req.BranchKey})
	return executor.Result{Output: out}, nil
}

func (h *harness) branchRows(runID pgtype.UUID, nodeID string) (controllers, branches int) {
	h.t.Helper()
	err := h.pool.QueryRow(context.Background(), `
		select count(*) filter (where branch_key = ''), count(*) filter (where branch_key <> '')
		  from node_executions where run_id = $1 and node_id = $2`, runID, nodeID).Scan(&controllers, &branches)
	if err != nil {
		h.t.Fatal(err)
	}
	return
}

const foreachWorkflow = `
apiVersion: workflow/v1
name: fanout
inputs:
  numbers: {type: array}
nodes:
  - id: each
    type: script
    foreach: {items: '${input.numbers}'}
    config: {value: '${item}'}
  - id: summary
    type: transform
    needs: [each]
    config:
      value:
        count: '${size(nodes.each.output)}'
        items: '${nodes.each.output.map(o, o.item)}'
outputs:
  summary: '${nodes.summary.output}'
`

// M9: every element runs the node once, the branches' outputs come back as a
// list in element order, and the successor runs once after all of them.
func TestForeachRunsOncePerElementAndFansIn(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(foreachWorkflow)
	h.script.set("each", echoItem)
	h.startWorkers(4)
	runID := h.start("fanout", map[string]any{"numbers": []any{10, 20, 30, 40, 50}})
	if status := h.waitStatus(runID, 20*time.Second, "succeeded", "failed"); status != "succeeded" {
		t.Fatalf("run = %s, nodes %v", status, h.nodeStatuses(runID))
	}
	out, _ := h.runOutput(runID)
	summary := out["summary"].(map[string]any)
	if summary["count"] != float64(5) {
		t.Fatalf("summary = %v", summary)
	}
	for i, want := range []float64{10, 20, 30, 40, 50} {
		if got := summary["items"].([]any)[i]; got != want {
			t.Fatalf("items = %v, want element order", summary["items"])
		}
	}
	if controllers, branches := h.branchRows(runID, "each"); controllers != 1 || branches != 5 {
		t.Fatalf("each has %d controller and %d branch rows, want 1 and 5", controllers, branches)
	}
	if h.script.callCount("each") != 5 {
		t.Fatalf("the executor ran %d times, want once per element", h.script.callCount("each"))
	}
	if got := h.nodeStatuses(runID); got["each"] != "succeeded" || got["summary"] != "succeeded" {
		t.Fatalf("nodes = %v", got)
	}
	// Each branch has its own stable idempotency key.
	keys := map[string]bool{}
	for _, key := range h.script.keys["each"] {
		keys[key] = true
	}
	if len(keys) != 5 {
		t.Fatalf("branch idempotency keys = %v, want 5 distinct", h.script.keys["each"])
	}
	h.checkInvariants(runID)
}

func TestForeachEmptyListSucceedsWithEmptyOutput(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(foreachWorkflow)
	h.script.set("each", echoItem)
	h.startWorkers(2)
	runID := h.start("fanout", map[string]any{"numbers": []any{}})
	if status := h.waitStatus(runID, 10*time.Second, "succeeded", "failed"); status != "succeeded" {
		t.Fatalf("run = %s, nodes %v", status, h.nodeStatuses(runID))
	}
	out, _ := h.runOutput(runID)
	if summary := out["summary"].(map[string]any); summary["count"] != float64(0) {
		t.Fatalf("summary = %v", summary)
	}
	if h.script.callCount("each") != 0 {
		t.Fatal("the executor ran for an empty list")
	}
	h.checkInvariants(runID)
}

// A node-level if is evaluated per branch, so it works as a filter; skipped
// branches contribute null.
func TestForeachIfFiltersPerElement(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(`
apiVersion: workflow/v1
name: evens
inputs: {numbers: {type: array}}
nodes:
  - id: each
    type: script
    if: '${int(item) % 2 == 0}'
    foreach: {items: '${input.numbers}'}
    config: {value: '${item}'}
  - id: summary
    type: transform
    needs: [each]
    config: {value: '${nodes.each.output}'}
outputs: {all: '${nodes.summary.output}'}
`)
	h.script.set("each", echoItem)
	h.startWorkers(2)
	runID := h.start("evens", map[string]any{"numbers": []any{1, 2, 3, 4}})
	if status := h.waitStatus(runID, 15*time.Second, "succeeded", "failed"); status != "succeeded" {
		t.Fatalf("run = %s, nodes %v", status, h.nodeStatuses(runID))
	}
	out, _ := h.runOutput(runID)
	list := out["all"].([]any)
	if len(list) != 4 || list[0] != nil || list[2] != nil || list[1].(map[string]any)["item"] != float64(2) || list[3].(map[string]any)["item"] != float64(4) {
		t.Fatalf("outputs = %v, want [null {2} null {4}]", list)
	}
	if h.script.callCount("each") != 2 {
		t.Fatalf("executor ran %d times, want 2", h.script.callCount("each"))
	}
	h.checkInvariants(runID)
}

// A retried branch works on the same element, with the same idempotency key.
func TestForeachBranchRetryKeepsItsElement(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(`
apiVersion: workflow/v1
name: retrying
inputs: {numbers: {type: array}}
nodes:
  - id: each
    type: script
    retry: {max_attempts: 3, backoff: fixed, initial: 10ms}
    foreach: {items: '${input.numbers}'}
    config: {value: '${item}'}
`)
	var mu sync.Mutex
	seen := map[string][]float64{}
	h.script.set("each", func(c context.Context, req executor.Request) (executor.Result, error) {
		result, err := echoItem(c, req)
		var out struct{ Item float64 }
		_ = json.Unmarshal(result.Output, &out)
		mu.Lock()
		seen[req.BranchKey] = append(seen[req.BranchKey], out.Item)
		first := len(seen[req.BranchKey]) == 1
		mu.Unlock()
		if req.BranchKey == "1" && first {
			return executor.Result{}, executor.Retryable("flaky", errors.New("try again"))
		}
		return result, err
	})
	h.startWorkers(2)
	runID := h.start("retrying", map[string]any{"numbers": []any{7, 8, 9}})
	if status := h.waitStatus(runID, 15*time.Second, "succeeded", "failed"); status != "succeeded" {
		t.Fatalf("run = %s, nodes %v", status, h.nodeStatuses(runID))
	}
	mu.Lock()
	defer mu.Unlock()
	if got := seen["1"]; len(got) != 2 || got[0] != 8 || got[1] != 8 {
		t.Fatalf("branch 1 saw elements %v, want [8 8]", got)
	}
	h.checkInvariants(runID)
}

const handledForeach = `
apiVersion: workflow/v1
name: handled
inputs: {numbers: {type: array}}
nodes:
  - id: each
    type: script
    on_failure: cleanup
    foreach: {items: '${input.numbers}'}
    config: {value: '${item}'}
  - id: summary
    type: transform
    needs: [each]
    config: {value: ok}
  - id: cleanup
    type: script
    needs: [each]
    join: any
    config: {}
`

func failOn(item float64) func(context.Context, executor.Request) (executor.Result, error) {
	return func(c context.Context, req executor.Request) (executor.Result, error) {
		result, err := echoItem(c, req)
		var out struct{ Item float64 }
		_ = json.Unmarshal(result.Output, &out)
		if out.Item == item {
			return executor.Result{}, executor.Permanent("bad_item", fmt.Errorf("item %v is rejected", item))
		}
		return result, err
	}
}

// One failed branch fails the foreach node; its on_failure handler runs, and
// the error says which branch failed and why.
func TestForeachBranchFailureRunsTheHandler(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(handledForeach)
	h.script.set("each", failOn(3))
	h.startWorkers(2)
	runID := h.start("handled", map[string]any{"numbers": []any{1, 2, 3, 4}})
	if status := h.waitStatus(runID, 15*time.Second, "succeeded", "failed"); status != "succeeded" {
		t.Fatalf("run = %s, nodes %v", status, h.nodeStatuses(runID))
	}
	got := h.nodeStatuses(runID)
	if got["each"] != "failed" || got["cleanup"] != "succeeded" || got["summary"] != "skipped" {
		t.Fatalf("nodes = %v", got)
	}
	var errorJSON map[string]any
	err := h.pool.QueryRow(ctx, `select error from node_executions where run_id = $1 and node_id = 'each' and branch_key = ''`, runID).Scan(&errorJSON)
	if err != nil || errorJSON["code"] != "E_FOREACH_BRANCH_FAILED" {
		t.Fatalf("controller error = %v, %v", errorJSON, err)
	}
	branches := errorJSON["branches"].([]any)
	if len(branches) != 1 || branches[0].(map[string]any)["index"] != float64(2) {
		t.Fatalf("failed branches = %v, want index 2", branches)
	}
	h.checkInvariants(runID)
}

func TestForeachUnhandledBranchFailureFailsTheRun(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(foreachWorkflow)
	h.script.set("each", failOn(20))
	h.startWorkers(2)
	runID := h.start("fanout", map[string]any{"numbers": []any{10, 20, 30}})
	if status := h.waitStatus(runID, 15*time.Second, "failed"); status != "failed" {
		t.Fatalf("run = %s", status)
	}
	if got := h.nodeStatuses(runID); got["summary"] == "succeeded" {
		t.Fatalf("summary ran after a failed foreach: %v", got)
	}
	h.checkInvariants(runID)
}

func TestForeachLimitsAndBadItems(t *testing.T) {
	ctx := context.Background()
	for name, tt := range map[string]struct {
		foreach string
		input   map[string]any
		code    string
	}{
		"over max_items":   {`{items: '${input.numbers}', max_items: 3}`, map[string]any{"numbers": []any{1, 2, 3, 4}}, "E_FOREACH_TOO_MANY"},
		"not a list":       {`{items: '${input.numbers}'}`, map[string]any{"numbers": "nope"}, "E_EXPR_EVAL"},
		"expression error": {`{items: '${input.numbers.map(n, n.x)}'}`, map[string]any{"numbers": []any{1}}, "E_EXPR_EVAL"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, ctx)
			h.mustPublish(`
apiVersion: workflow/v1
name: limited
inputs: {numbers: {}}
nodes:
  - id: each
    type: script
    foreach: ` + tt.foreach + `
    config: {value: '${item}'}
`)
			h.script.set("each", echoItem)
			h.startWorkers(1)
			runID := h.start("limited", tt.input)
			h.waitStatus(runID, 15*time.Second, "failed")
			attempts := h.attempts(runID, "each")
			if len(attempts) != 1 || attempts[0]["code"] != tt.code {
				t.Fatalf("attempts = %v, want one attempt failing %s", attempts, tt.code)
			}
			if h.script.callCount("each") != 0 {
				t.Fatal("the executor ran although fan-out failed")
			}
		})
	}
}

// Each element of a foreach over human_task gets its own task and wait key.
func TestForeachOverHumanTasks(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(`
apiVersion: workflow/v1
name: reviews
inputs: {patients: {type: array}}
nodes:
  - id: review
    type: human_task
    foreach: {items: '${input.patients}'}
    config:
      queue: rn-review
      sla: 1h
      payload: {patient: '${item}', position: '${index}'}
  - id: done
    type: transform
    needs: [review]
    config: {value: '${nodes.review.output.map(o, o.ok)}'}
outputs: {oks: '${nodes.done.output}'}
`)
	c := apiClient{t: t, base: h.apiServer(actorAuth{}), tenant: h.tenant}
	h.startWorkers(2)
	runID := h.start("reviews", map[string]any{"patients": []any{"p-a", "p-b", "p-c"}})

	deadline := time.Now().Add(10 * time.Second)
	var items []any
	for {
		_, body := c.json("GET", "/be/v1/tasks?status=open&queue=rn-review", nil, as("nina")...)
		if items, _ = body["items"].([]any); len(items) == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("open tasks = %d, want 3", len(items))
		}
		time.Sleep(25 * time.Millisecond)
	}
	for _, item := range items {
		task := item.(map[string]any)
		payload := task["payload"].(map[string]any)
		if payload["patient"] == nil || payload["position"] == nil {
			t.Fatalf("task payload = %v", payload)
		}
		status, body := c.json("POST", "/be/v1/tasks/"+task["id"].(string)+"/complete", map[string]any{"payload": map[string]any{"ok": payload["patient"]}}, as("nina")...)
		if status != 200 {
			t.Fatalf("complete = %d %v", status, body)
		}
	}
	if got := h.waitStatus(runID, 15*time.Second, "succeeded", "failed"); got != "succeeded" {
		t.Fatalf("run = %s, nodes %v", got, h.nodeStatuses(runID))
	}
	out, _ := h.runOutput(runID)
	oks := out["oks"].([]any)
	if len(oks) != 3 || oks[0] != "p-a" || oks[1] != "p-b" || oks[2] != "p-c" {
		t.Fatalf("outputs = %v, want element order p-a p-b p-c", oks)
	}
	h.checkInvariants(runID)
}

// Cancelling a run mid fan-out stops the branches and leaves nothing live.
func TestForeachCancelStopsBranches(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(foreachWorkflow)
	release := make(chan struct{})
	var once sync.Once
	h.script.set("each", func(c context.Context, req executor.Request) (executor.Result, error) {
		select {
		case <-release:
			return echoItem(c, req)
		case <-c.Done():
			return executor.Result{}, c.Err()
		}
	})
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	h.startWorkers(1)
	runID := h.start("fanout", map[string]any{"numbers": []any{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}})
	deadline := time.Now().Add(10 * time.Second)
	for h.script.callCount("each") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no branch started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := h.flow.Cancel(ctx, h.tenant, runID, "test"); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(runID, 10*time.Second, "cancelled")
	// Running branches notice the cancellation at their next heartbeat.
	deadline = time.Now().Add(10 * time.Second)
	for {
		var live int
		if err := h.pool.QueryRow(ctx, `select count(*) from node_executions where run_id = $1 and status in ('ready','running','waiting')`, runID).Scan(&live); err != nil {
			t.Fatal(err)
		}
		if live == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("live node rows after cancel = %d", live)
		}
		time.Sleep(50 * time.Millisecond)
	}
	once.Do(func() { close(release) })
	h.checkInvariants(runID)
}

// Many runs fanning out and in concurrently across workers: the controller
// completes once, the successor runs once, and no branch is duplicated.
func TestForeachConcurrentFanInRunsSuccessorOnce(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(foreachWorkflow)
	h.script.set("each", echoItem)
	h.startWorkers(8)
	const runs, width = 40, 12
	numbers := make([]any, width)
	for i := range numbers {
		numbers[i] = i
	}
	ids := make([]pgtype.UUID, runs)
	for i := range ids {
		ids[i] = h.start("fanout", map[string]any{"numbers": numbers})
	}
	for _, id := range ids {
		if status := h.waitStatus(id, 60*time.Second, "succeeded", "failed"); status != "succeeded" {
			t.Fatalf("run %v = %s", id, status)
		}
		if controllers, branches := h.branchRows(id, "each"); controllers != 1 || branches != width {
			t.Fatalf("rows = %d controller, %d branches", controllers, branches)
		}
		var summaries int
		if err := h.pool.QueryRow(ctx, `select count(*) from node_executions where run_id = $1 and node_id = 'summary'`, id).Scan(&summaries); err != nil || summaries != 1 {
			t.Fatalf("summary rows = %d, %v; want exactly 1", summaries, err)
		}
		h.checkInvariants(id)
	}
	if got := h.script.callCount("each"); got != runs*width {
		t.Fatalf("branches executed %d times, want %d", got, runs*width)
	}
}
