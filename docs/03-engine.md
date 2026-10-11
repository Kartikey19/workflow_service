# 03 — Engine Runtime

This is the part that must be correct. Everything else is I/O plumbing around it.

Read `01-data-model.md` first — this document assumes the schema and the
invariants I1–I7.

---

## 1. The whole engine in one page

```
┌─ worker process ────────────────────────────────────────────────────┐
│                                                                     │
│  ┌── claim loop ──────────┐   ┌── sweeper (every 5s) ────────────┐   │
│  │ every 200ms, or on     │   │ • reclaim expired leases         │   │
│  │ LISTEN wf_ready:       │   │ • wake elapsed sleeps            │   │
│  │   ClaimReady(batch)    │   │ • time out expired waits         │   │
│  │   → chan *NodeExec     │   │ • fail runs past deadline_at     │   │
│  └────────┬───────────────┘   └──────────────────────────────────┘   │
│           │                                                          │
│           ▼   N goroutines (semaphore-bounded)                       │
│  ┌── dispatcher ───────────────────────────────────────────────┐     │
│  │ 1. load spec (cached by version id)                         │     │
│  │ 2. build eval env from run.input + completed node outputs   │     │
│  │ 3. resolve ${...} in config      → node.input               │     │
│  │ 4. evaluate `if`  → false ⇒ skip                            │     │
│  │ 5. heartbeat goroutine extends lease while running          │     │
│  │ 6. executor.Execute(ctx, req)                               │     │
│  │ 7. classify error / read Control                            │     │
│  │ 8. ── TX ──────────────────────────────────────────────┐    │     │
│  │      fenced write of result                            │    │     │
│  │      append run_events                                 │    │     │
│  │      resolver.Advance(...)  ← enqueue/skip successors   │    │     │
│  │      recompute run status                              │    │     │
│  │    ─────────────────────────────────────────────────────┘    │     │
│  └─────────────────────────────────────────────────────────────┘     │
└─────────────────────────────────────────────────────────────────────┘
```

No leader. No coordinator. No partition assignment. Workers are interchangeable
and stateless; scaling is `replicas: N`.

---

## 2. Package layout

```
internal/core/engine/
  worker.go       // lifecycle: start claim loop + sweeper + shutdown
  claim.go        // ClaimReady loop, LISTEN/NOTIFY wakeup
  dispatch.go     // the per-node pipeline (§4)
  resolve.go      // DAG advancement + skip cascade + run completion (§5)
  retry.go        // backoff computation, error classification (§6)
  sweep.go        // leases, sleeps, wait timeouts, run deadlines (§8)
  lease.go        // heartbeat + fencing helpers
  registry.go     // type -> Executor
```

---

## 3. The Executor interface

This is the extension seam. Get it right and adding `agent` in M7 touches no
engine code at all.

```go
package executor

// Executor implements exactly one node type.
// Implementations MUST be safe for concurrent use and MUST be idempotent
// with respect to Request.IdempotencyKey (see 01-data-model §5).
type Executor interface {
    // Type is the `type:` value in YAML. Must be unique in the registry.
    Type() string

    // ValidateConfig is called at PUBLISH time with the raw, unresolved config
    // (expressions still present as literal ${...} strings). It checks shape,
    // required fields, and ranges — never anything requiring I/O.
    ValidateConfig(raw json.RawMessage) error

    // Execute runs one attempt. ctx carries the per-attempt timeout.
    Execute(ctx context.Context, req Request) (Result, error)
}

type Request struct {
    RunID           uuid.UUID
    NodeExecutionID uuid.UUID
    TenantID        uuid.UUID
    NodeID          string
    BranchKey       string
    Attempt         int

    // Config is fully template-resolved: no ${...} remains.
    Config json.RawMessage

    // IdempotencyKey is STABLE across attempts. Forward it to any downstream
    // system that supports deduplication.
    IdempotencyKey string

    // Deadline mirrors ctx's deadline; useful for setting client timeouts.
    Deadline time.Time

    // Emit records a progress event. Best-effort, non-transactional.
    // Reserved for streaming node output in v2.
    Emit func(ctx context.Context, kind string, payload any)
}

type Result struct {
    Output  json.RawMessage
    Metrics map[string]any  // duration_ms is added by the engine; add your own

    // Control, if non-nil, asks the engine to do something other than
    // "mark succeeded and advance". Exactly one field may be set.
    Control *Control
}

type Control struct {
    Goto  string      // branch: enqueue only this successor, skip the other arms
    Sleep *time.Time  // sleep: park until this instant
    Wait  *WaitSpec   // wait_event: park until signalled or timed out
}

type WaitSpec struct {
    Key     string
    Expires time.Time
    Schema  json.RawMessage // optional; validates the signal payload
}
```

**Why `Control` is a return value.** The engine has no `switch nodeType` anywhere.
`branch`, `sleep`, and `wait_event` are ordinary executors that happen to return a
directive. Adding a new control-flow primitive in a year — `race`, `checkpoint`,
`spawn_subworkflow` — is a new executor plus one new `Control` field, not surgery
on the core. This is principle P6 and it is the single highest-leverage decision
in the runtime design.

### 3.1 Error contract

```go
// Retryable: transient. The engine will schedule attempt+1 if budget remains.
type RetryableError struct {
    Code       string        // "timeout" | "upstream_5xx" | "rate_limited" | ...
    Err        error
    RetryAfter time.Duration // optional; honours an upstream Retry-After header
}

// Permanent: no retry, regardless of budget. 4xx, validation failures, bad config.
type PermanentError struct {
    Code string  // "bad_request" | "not_found" | "invalid_output" | ...
    Err  error
}
```

Executors should classify explicitly. Unclassified errors are treated as
**retryable** (§6.2) — the wrong default costs you a duplicate call; the other
wrong default costs you a failed run on a blip.

### 3.2 Registry

```go
type Registry struct{ m map[string]Executor }

func (r *Registry) Register(e Executor) {
    if _, dup := r.m[e.Type()]; dup { panic("duplicate executor type " + e.Type()) }
    r.m[e.Type()] = e
}
func (r *Registry) Get(t string) (Executor, bool) { e, ok := r.m[t]; return e, ok }
```

The registry is shared by the compiler (for `ValidateConfig` and `E_UNKNOWN_TYPE`)
and the dispatcher. Build it once in `main` and pass it to both. A workflow
referencing a type this binary doesn't have must fail at publish, not at 3am.

---

## 4. The dispatcher

### 4.1 Pipeline

```go
func (d *Dispatcher) Run(ctx context.Context, ne *store.NodeExecution) {
    start := time.Now()

    spec, err := d.specs.Get(ctx, ne.WorkflowVersionID)     // LRU, immutable
    node, ok := spec.Node(ne.NodeID)
    if !ok {
        // spec/DB disagree — impossible unless someone mutated a version.
        d.failPermanent(ctx, ne, "E_NODE_NOT_IN_SPEC"); return
    }

    // per-attempt deadline; always shorter than the lease TTL is NOT required —
    // the heartbeat (§4.3) extends the lease for as long as we're alive.
    attemptCtx, cancel := context.WithTimeout(ctx, node.Timeout)
    defer cancel()

    stop := d.heartbeat(ctx, ne)   // extends lease every TTL/3
    defer stop()

    env, err := d.buildEnv(ctx, ne)                  // §4.2
    resolvedCfg, err := expr.Resolve(node.Config, env)

    if node.If != nil {
        ok, err := expr.EvalBool(node.If, env)
        if err == nil && !ok { d.skip(ctx, ne, "if_false"); return }
    }

    d.store.SetNodeInput(ctx, ne.ID, resolvedCfg)     // what we actually sent

    ex, _ := d.registry.Get(node.Type)
    res, execErr := ex.Execute(attemptCtx, executor.Request{
        RunID: ne.RunID, NodeExecutionID: ne.ID, NodeID: ne.NodeID,
        BranchKey: ne.BranchKey, Attempt: ne.Attempt,
        Config: resolvedCfg, IdempotencyKey: ne.IdempotencyKey,
        Deadline: time.Now().Add(node.Timeout), Emit: d.emitter(ne),
    })

    res.Metrics = withDuration(res.Metrics, time.Since(start))

    switch {
    case execErr != nil:
        d.handleError(ctx, ne, node, execErr, res.Metrics)   // §6
    case res.Control != nil:
        d.handleControl(ctx, ne, node, res)                  // §7
    default:
        d.complete(ctx, ne, res)                             // §4.4
    }
}
```

### 4.2 Building the evaluation environment

```go
func (d *Dispatcher) buildEnv(ctx, ne) (*expr.Env, error) {
    run  := d.store.GetRun(ctx, ne.RunID)
    deps := d.store.LatestAttemptsForRun(ctx, ne.RunID, ne.BranchKey)  // all terminal nodes
    return expr.NewEnv(expr.Bindings{
        Input:  run.Input,
        Nodes:  viewsOf(deps),          // map[nodeID]{output,status,attempt,metrics}
        Run:    runView(run),
        Secrets: d.secrets.For(ctx, run.TenantID),   // lazy; never persisted
        Now:    time.Now(),
    }), nil
}
```

One query for all completed nodes of the run, not one per referenced node. For a
1 000-node run this is the only place you could accidentally write an N+1, and it
would be invisible until it wasn't.

**Secrets** are resolved here and must never reach `node_executions.input`. Two
options: (a) resolve secret expressions *inside* the executor from a separate
binding the walker keeps out of the persisted copy, or (b) resolve fully but
redact by path before writing `input`. Do (a) — redaction by path is a leak
waiting for the first user who nests a secret somewhere you didn't expect. In
practice: run `expr.Resolve` twice, once with real secrets (passed to the
executor, never stored) and once with each secret of 8 or more characters
replaced by `"***"` (stored). Shorter values are only allowed as keys of a
secret group (an effort, a temperature); they are settings and stay as they are,
so `double(secrets.group.temperature)` still evaluates in the stored copy.

### 4.3 Lease heartbeat

```go
func (d *Dispatcher) heartbeat(ctx context.Context, ne *store.NodeExecution) func() {
    ctx, cancel := context.WithCancel(ctx)
    go func() {
        t := time.NewTicker(d.leaseTTL / 3)
        defer t.Stop()
        for {
            select {
            case <-ctx.Done(): return
            case <-t.C:
                n, _ := d.store.ExtendLease(ctx, ne.ID, d.workerID, d.leaseTTL)
                if n == 0 {
                    // We lost the lease. Another worker owns this node now.
                    // Cancel the attempt so we don't do redundant work.
                    d.log.Warn("lease lost", "node_execution", ne.ID)
                    cancel()
                    return
                }
            }
        }
    }()
    return cancel
}
```

```sql
-- name: ExtendLease :execrows
update node_executions
   set lease_expires_at = now() + @ttl::interval
 where id = @id and lease_owner = @worker_id and status = 'running';
```

Lease TTL of 60s with a 20s heartbeat is a good default. The TTL bounds how long a
crashed worker's node sits stuck; the heartbeat lets a legitimately slow node (a
90-second agent call) run past the TTL without being stolen.

### 4.4 Completion transaction

Every terminal transition runs in **one** transaction, and it does four things:

```go
func (d *Dispatcher) complete(ctx context.Context, ne *store.NodeExecution, res executor.Result) error {
    return d.store.InTx(ctx, func(tx store.Tx) error {
        // 1. fenced write — if 0 rows, we lost the lease; abandon quietly
        n, err := tx.CompleteNode(ctx, store.CompleteNodeParams{
            ID: ne.ID, WorkerID: d.workerID, Status: "succeeded",
            Output: res.Output, Metrics: res.Metrics,
        })
        if err != nil { return err }
        if n == 0 { return errLostLease }   // caller logs and drops the result

        // 2. append the event (seq allocated under the run lock taken in step 4)
        // 3. advance the DAG
        // 4. recompute run status
        return d.resolver.Advance(ctx, tx, ne.RunID, ne.NodeID, ne.BranchKey, "succeeded")
    })
}
```

Why one transaction: if the result write commits but the successor enqueue does
not, the run stalls forever with no ready nodes and no waiting nodes — a state the
sweeper cannot distinguish from a legitimately finished run. Atomicity here is not
an optimisation, it is a correctness requirement.

Why this transaction is safe to keep short: it does no I/O beyond Postgres, and
the executor call already happened outside it. **Never hold a transaction open
across an executor call.** A 90-second agent node holding a Postgres transaction
will exhaust your pool and pin the xmin horizon, blocking autovacuum.

---

## 5. The resolver

The heart of the engine. Given "node X of run R just reached terminal status S",
decide what happens next.

### 5.1 Algorithm

```go
func (r *Resolver) Advance(ctx context.Context, tx store.Tx,
    runID uuid.UUID, nodeID, branchKey, status string, ctrl *executor.Control) error {

    run  := tx.LockRun(ctx, runID)        // SELECT ... FOR UPDATE — serialises §5.4
    spec := r.specs.MustGet(run.WorkflowVersionID)

    // Work queue, because skipping cascades.
    type item struct{ nodeID, status string }
    queue := []item{{nodeID, status}}

    for len(queue) > 0 {
        cur := queue[0]; queue = queue[1:]
        curNode := spec.Node(cur.nodeID)

        // 1. determine candidate successors
        var successors []string
        switch {
        case ctrl != nil && ctrl.Goto != "" && cur.nodeID == nodeID:
            // branch: only the chosen arm proceeds; other arms get skipped
            successors = spec.Edges[cur.nodeID]
        case cur.status == "failed" && curNode.OnFailure != "":
            successors = []string{curNode.OnFailure}
        default:
            successors = spec.Edges[cur.nodeID]
        }

        for _, sid := range successors {
            s := spec.Node(sid)

            // branch pruning: arms not chosen are skipped, not enqueued
            if ctrl != nil && ctrl.Goto != "" && cur.nodeID == nodeID && sid != ctrl.Goto {
                if tx.EnqueueNode(ctx, runID, s, branchKey, "skipped") != nil { continue }
                queue = append(queue, item{sid, "skipped"})
                continue
            }

            // 2. are all of s's dependencies terminal?
            depStates := tx.DepStates(ctx, runID, branchKey, s.Needs)  // latest attempt each
            if len(depStates) < len(s.Needs) || anyNonTerminal(depStates) {
                continue    // not our turn; whoever finishes last will enqueue s
            }

            // 3. join semantics
            var next string
            if s.Join == "any" {
                next = pick(anySucceeded(depStates), "ready", "skipped")
            } else {
                next = pick(allSucceeded(depStates), "ready", "skipped")
            }

            // 4. insert. ON CONFLICT DO NOTHING makes the diamond race safe.
            inserted, err := tx.EnqueueNode(ctx, runID, s, branchKey, next)
            if err != nil { return err }
            if !inserted { continue }     // another worker got there first

            tx.AppendEvent(ctx, runID, eventFor(next), sid)
            if next == "skipped" {
                queue = append(queue, item{sid, "skipped"})   // cascade
            } else {
                r.notify(ctx, tx, runID)                       // pg_notify('wf_ready')
            }
        }
    }

    return r.recomputeRunStatus(ctx, tx, run, spec)   // §5.4
}
```

### 5.2 The three races, and why they're safe

**Race A — diamond convergence.** Nodes A and B (both feeding C) complete
simultaneously on workers 1 and 2. Both see all of C's deps terminal. Both call
`EnqueueNode(C)`. The unique index `(run_id, node_id, branch_key, attempt)` lets
exactly one insert win; the other's `ON CONFLICT DO NOTHING` returns zero rows and
that worker moves on. **C runs exactly once.** This is invariant I1 doing its job,
and it is why the unique index is not optional.

**Race B — partial dependency visibility.** A completes; B is still `running`.
A's resolver sees B non-terminal and does nothing. B completes moments later; B's
resolver sees both terminal and enqueues C. The rule "whoever finishes last
enqueues" needs no coordination because both transactions read committed state and
the last one to commit necessarily sees the other. (Read Committed is sufficient
here precisely because of the `LockRun` in step 0 — without it, two concurrent
resolvers on the same run could both read a stale dep state. Take the lock.)

**Race C — lease theft mid-execution.** Worker 1's lease expires while it is still
executing; the sweeper resets the node to `ready`; worker 2 claims and completes
it. Worker 1 then tries to write its result — the fenced `CompleteNode` matches
zero rows (`lease_owner` no longer matches) and worker 1 discards. The executor
may have performed its side effect twice, which is why the idempotency key is
stable across attempts and why executors must be idempotent (P5).

### 5.3 Skip cascade

When a node is skipped, its successors must be evaluated too — otherwise a
downstream node with `join: all` waits forever on a dependency that will never
produce a row. The loop in §5.1 handles this by pushing skipped nodes back onto
the work queue. The cascade terminates because the graph is acyclic (guaranteed by
`E_CYCLE` at compile time) and each node is inserted at most once (I1).

Worked example, `support-triage` (`02 §5.2`), when `severity == "P0"`:

```
route succeeds, Control.Goto = page_oncall
  → successors of route: [page_oncall, auto_reply]
      page_oncall  → enqueued ready
      auto_reply   → enqueued SKIPPED, pushed to cascade queue
  → cascade: auto_reply(skipped)
      successors of auto_reply: [record]
        record.needs = [page_oncall, auto_reply]
        DepStates: page_oncall=ready(non-terminal) → not all terminal → skip for now
  → done
page_oncall succeeds
  → successors: [record]
      DepStates: page_oncall=succeeded, auto_reply=skipped → all terminal ✓
      record.join = "any", anySucceeded ✓ → enqueue record READY
```

Had `record` used the default `join: all`, it would have been enqueued `skipped`
and the run would end without recording the triage — which is why the compiler
emits `W_JOIN_ALL_ACROSS_BRANCH` (`02 §6.2`). Make sure that warning exists.

### 5.4 Run completion

Still inside the resolver's transaction, holding the run row lock:

```go
func (r *Resolver) recomputeRunStatus(ctx, tx, run, spec) error {
    counts := tx.CountNodeStatuses(ctx, run.ID)   // group by status, latest attempt only

    switch {
    case counts["ready"] > 0 || counts["running"] > 0:
        return tx.SetRunStatus(ctx, run.ID, "running")

    case counts["waiting"] > 0:
        return tx.SetRunStatus(ctx, run.ID, "waiting")

    default:
        // nothing is runnable and nothing is pending: the run is over
        status := "succeeded"
        if hasUnhandledFailure(ctx, tx, run.ID, spec) { status = "failed" }
        out, err := expr.Resolve(spec.Outputs, envFor(run, tx))
        if err != nil { out, status = nil, "failed" }
        tx.FinishRun(ctx, run.ID, status, out)
        tx.AppendEvent(ctx, run.ID, "run."+status, "")
        return nil
    }
}
```

```sql
-- name: CountNodeStatuses :many
select status, count(*) from (
  select distinct on (node_id, branch_key) status
    from node_executions
   where run_id = @run_id
   order by node_id, branch_key, attempt desc
) s group by status;
```

`distinct on ... order by attempt desc` is the "latest attempt wins" rule
expressed once, in SQL. A node with `attempt=1 failed, attempt=2 succeeded` counts
as succeeded exactly once.

`hasUnhandledFailure` means: a node ended `failed` and its `on_failure` was empty.
A failure that was handled by an `on_failure` edge does not fail the run — the
compensation path ran, which is the whole point.

**The `LockRun` at the top of `Advance` is what makes this correct.** Without it,
the last two nodes of a run completing simultaneously could both observe
"everything else is terminal" and both call `FinishRun`, double-writing the run
output and emitting two `run.succeeded` events with colliding `seq`. One row lock
per run, held for a few milliseconds, buys you all of that. It also serialises
`run_events.seq` allocation (I6).

---

## 6. Retries

### 6.1 Scheduling a retry

A retry is **a new row**, never a mutation:

```go
func (d *Dispatcher) handleError(ctx, ne, node, err error, metrics map[string]any) error {
    code, retryable, retryAfter := classify(err)

    budget := node.Retry.MaxAttempts
    canRetry := retryable && ne.Attempt < budget && allowedByRetryOn(node.Retry, code)

    return d.store.InTx(ctx, func(tx store.Tx) error {
        n, _ := tx.CompleteNode(ctx, ...Status: "failed", Error: errJSON(code, err, retryable), Metrics: metrics)
        if n == 0 { return errLostLease }

        if canRetry {
            delay := backoff(node.Retry, ne.Attempt)
            if retryAfter > delay { delay = retryAfter }   // honour upstream Retry-After
            tx.EnqueueRetry(ctx, ne, ne.Attempt+1, time.Now().Add(delay))
            tx.AppendEvent(ctx, ne.RunID, "node.retrying",
                map[string]any{"attempt": ne.Attempt + 1, "delay_ms": delay.Milliseconds()})
            return tx.SetRunStatus(ctx, ne.RunID, "running")
        }

        tx.AppendEvent(ctx, ne.RunID, "node.failed", map[string]any{"code": code})
        return d.resolver.Advance(ctx, tx, ne.RunID, ne.NodeID, ne.BranchKey, "failed", nil)
    })
}
```

`EnqueueRetry` reuses the identity insert with `attempt+1` and — critically — the
**same `idempotency_key`** (I5).

### 6.2 Backoff

```go
func backoff(p RetryPolicy, attempt int) time.Duration {
    var d time.Duration
    switch p.Backoff {
    case "fixed":       d = p.Initial
    case "linear":      d = p.Initial * time.Duration(attempt)
    default:            d = p.Initial << (attempt - 1)   // exponential
    }
    d = min(d, p.Max)
    // full jitter — avoids the thundering herd when 500 runs retry the same
    // upstream at the same instant. This matters more than the base curve.
    if p.Jitter > 0 {
        d = time.Duration(float64(d) * (1 - p.Jitter + rand.Float64()*2*p.Jitter))
    }
    return d
}
```

### 6.3 Default classification

```go
func classify(err error) (code string, retryable bool, after time.Duration) {
    var re *executor.RetryableError
    var pe *executor.PermanentError
    switch {
    case errors.As(err, &re): return re.Code, true,  re.RetryAfter
    case errors.As(err, &pe): return pe.Code, false, 0
    case errors.Is(err, context.DeadlineExceeded): return "timeout", true, 0
    case errors.Is(err, context.Canceled):         return "cancelled", false, 0
    default:                                       return "unknown", true, 0   // §3.1
    }
}
```

### 6.4 Poison-node protection

`max_attempts` is capped at 20 by the compiler (`E_RETRY_RANGE`). Additionally,
enforce a **per-run node budget**: if a single run has produced more than, say,
1 000 node executions, fail it with `E_RUN_BUDGET`. A workflow with an
`on_failure` that loops back to its own failing dependency will otherwise consume
your database. Add this in M4 when `on_failure` lands, not after you learn why.

---

## 7. Control directives

### 7.1 `Goto` (branch)

Handled entirely by the resolver (§5.1, §5.3). The branch node itself is written
`succeeded` with `output = {"chosen": "page_oncall"}` — useful in the timeline.

### 7.2 `Sleep`

```go
tx.ParkNode(ctx, ne.ID, d.workerID, "waiting", *res.Control.Sleep) // run_after = until
tx.AppendEvent(ctx, ne.RunID, "node.waiting", ...)
tx.SetRunStatus(ctx, ne.RunID, "waiting")   // if nothing else is runnable
```

```sql
-- name: ParkNode :execrows
update node_executions
   set status = 'waiting', run_after = @until,
       lease_owner = null, lease_expires_at = null
 where id = @id and status = 'running' and lease_owner = @worker_id;
```

Still fenced. Still one row. No timer, no goroutine, no scheduled job. The sweeper
(§8.2) wakes it.

### 7.3 `Wait` (external event)

Same park, plus a `run_waits` row, in the same transaction:

```go
tx.ParkNode(ctx, ne.ID, d.workerID, "waiting", ctrl.Wait.Expires)
err := tx.InsertWait(ctx, store.InsertWaitParams{
    RunID: ne.RunID, NodeExecutionID: ne.ID, TenantID: ne.TenantID,
    WaitKey: ctrl.Wait.Key, ExpiresAt: ctrl.Wait.Expires,
})
if isUniqueViolation(err) {
    // another live run already waits on this key — a workflow authoring bug.
    // fail LOUDLY here rather than misrouting a signal later.
    return d.failPermanent(ctx, ne, "E_WAIT_KEY_CONFLICT")
}
```

**Delivery** (`POST /be/v1/signals`), one transaction (`01 §6.7`), then
`resolver.Advance(..., "succeeded")` inside the same tx.

**The early-signal problem.** A courier can plausibly deliver
`order.picked_up:ORD-1` before the run reaches `await_pickup` — a fast webhook
racing a slow reserve-stock retry. With no waiter registered, the signal is lost
and the run hangs until timeout.

Two acceptable answers; pick one and document it in the API:

- **(a) Reject.** `404 no_waiter`, and require callers to retry. Simple, honest,
  and pushes durability onto the (usually retrying) caller. **Do this in v1.**
- **(b) Park it.** Insert a `run_waits` row with `status='fulfilled'` and no
  `node_execution_id`, keyed by `wait_key`, with a short TTL. `InsertWait` then
  checks for a pre-delivered signal and completes immediately. More robust,
  and it needs a garbage collector for unclaimed parked signals.

Do not silently accept-and-drop. That is the version that produces a bug report
six months later that nobody can reproduce.

---

## 8. The sweeper

One goroutine per worker, every ~5 seconds, jittered. All four queries use
`FOR UPDATE SKIP LOCKED`, so running the sweeper on every worker is safe and
requires no leader election.

### 8.1 Reclaim expired leases

`01 §6.5`. `running` + `lease_expires_at < now()` → `ready`, **same attempt**
(this is re-delivery, not a retry — see `01 §3.1`). Emit a
`node.lease_expired` metric; a nonzero rate means your lease TTL is shorter than
your slowest node, or workers are being OOM-killed.

### 8.2 Wake sleeps and time out waits

`01 §6.6` returns `waiting` nodes whose `run_after <= now()`. Dispatch by node type:

```go
for _, n := range due {
    node := spec.Node(n.NodeID)
    switch node.Type {
    case "sleep":
        // the sleep elapsed: the node has succeeded
        tx.CompleteNodeUnfenced(ctx, n.ID, "succeeded", nil)
        resolver.Advance(ctx, tx, n.RunID, n.NodeID, n.BranchKey, "succeeded", nil)

    default: // wait_event, human_task
        tx.ExpireWait(ctx, n.ID)
        tx.CompleteNodeUnfenced(ctx, n.ID, "failed", errJSON("wait_timeout", nil, false))
        tx.AppendEvent(ctx, n.RunID, "node.timeout", n.NodeID)
        // on_timeout is handled by the resolver via the same path as on_failure
        resolver.Advance(ctx, tx, n.RunID, n.NodeID, n.BranchKey, "failed", nil)
    }
}
```

`CompleteNodeUnfenced` is the one place a write bypasses lease fencing — a
`waiting` node holds no lease by construction, so the guard is
`where id = $1 and status = 'waiting'` instead. Keep it a separate, obviously
named function so nobody reaches for it by accident.

`on_timeout` needs one line in the resolver's successor selection, symmetric with
`on_failure`:

```go
case cur.status == "failed" && errCode(cur) == "wait_timeout" && curNode.OnTimeout != "":
    successors = []string{curNode.OnTimeout}
```

### 8.3 Fail runs past their deadline

```sql
select id from runs
 where status in ('running','waiting') and deadline_at <= now()
 for update skip locked limit 100;
```

Cancel every non-terminal node, mark the run `failed` with
`{"code":"run_deadline_exceeded"}`. Without this, one bad `wait_event` key means a
row that lives forever.

### 8.4 Stuck-run detector (observability, not correction)

A run in `running` with no node in `ready|running` for more than a minute is an
engine bug — a resolver that failed to enqueue a successor. It should be
impossible given §4.4's single transaction. Alert on it anyway; it is your canary
for a transaction boundary someone loosened during a refactor.

---

## 9. The claim loop

```go
func (w *Worker) claimLoop(ctx context.Context) error {
    sem := make(chan struct{}, w.concurrency)   // e.g. 32
    ticker := time.NewTicker(w.pollInterval)    // 200ms
    defer ticker.Stop()

    wake := w.listenReady(ctx)   // pg LISTEN wf_ready; nil if not enabled

    for {
        // don't claim work we have no capacity to run
        free := cap(sem) - len(sem)
        if free > 0 {
            batch, err := w.store.ClaimReady(ctx, w.id, w.leaseTTL, min(free, w.batchSize))
            if err != nil { w.log.Error("claim failed", "err", err) }
            for _, ne := range batch {
                sem <- struct{}{}
                go func(ne *store.NodeExecution) {
                    defer func() { <-sem }()
                    defer recoverPanic(ctx, w, ne)     // §9.2
                    w.dispatcher.Run(ctx, ne)
                }(ne)
            }
            if len(batch) == w.batchSize { continue }  // more work; skip the sleep
        }

        select {
        case <-ctx.Done(): return w.drain(sem)
        case <-ticker.C:
        case <-wake:
        }
    }
}
```

Three details that matter:

**Capacity-aware claiming.** Claiming more than you can run means leases start
ticking on nodes sitting in a channel. At best you waste lease TTL; at worst the
sweeper reclaims work you are about to start. Claim `min(free, batchSize)`.

**Immediate re-poll on a full batch.** If you got `batchSize` rows there is
probably more; going to sleep for 200ms with a full queue adds latency for no
reason.

**`LISTEN/NOTIFY` for latency.** The resolver calls
`pg_notify('wf_ready', run_id)` when it enqueues a ready node, and idle workers
wake instantly instead of waiting up to 200ms. This is a pure optimisation —
**the polling ticker must remain**, because NOTIFY is not durable and is dropped
on connection loss. Never let correctness depend on it.

### 9.1 Graceful shutdown

On `SIGTERM`:

1. Stop claiming immediately.
2. Wait up to `shutdownGrace` (30s) for in-flight nodes to finish and commit.
3. For anything still running, **release the lease** rather than letting it
   expire: `update ... set status='ready', lease_owner=null where lease_owner=$1
   and status='running'`. This turns a 60-second stall on every deploy into a
   sub-second handoff.
4. Exit.

Step 3 is a two-line query that makes rolling deploys invisible. Do not skip it.

### 9.2 Panic containment

An executor panic must fail one node, not the worker:

```go
func recoverPanic(ctx context.Context, w *Worker, ne *store.NodeExecution) {
    if r := recover(); r != nil {
        w.log.Error("executor panic", "node", ne.NodeID, "panic", r,
            "stack", string(debug.Stack()))
        w.dispatcher.failPermanent(ctx, ne, "executor_panic")
    }
}
```

Classify panics as **permanent**. A panic is a bug, and retrying a bug three times
just produces three stack traces.

---

## 10. Cancellation

`POST /be/v1/runs/{id}/cancel`, one transaction:

```sql
update runs set status='cancelled', ended_at=now()
 where id = $1 and status in ('pending','running','waiting');

update node_executions set status='cancelled', ended_at=now(),
       lease_owner=null, lease_expires_at=null
 where run_id = $1 and status in ('ready','waiting');

update run_waits set status='cancelled' where run_id = $1 and status='waiting';
```

Nodes already `running` are **not** touched by the UPDATE — instead, the
dispatcher's heartbeat checks run status and cancels the attempt context:

```sql
-- name: ExtendLease :execrows   (final form)
update node_executions ne
   set lease_expires_at = now() + @ttl::interval
 where ne.id = @id and ne.lease_owner = @worker_id and ne.status = 'running'
   and exists (select 1 from runs r where r.id = ne.run_id
                and r.status in ('running','waiting'));
```

A cancelled run makes the heartbeat return 0 rows, which cancels the attempt
context (§4.3), which propagates into the executor's HTTP client or LLM call. The
result write then finds no matching row and is discarded. **Cancellation costs no
extra machinery** — it reuses fencing.

---

## 11. Concurrency & transaction summary

| Operation | Isolation | Locks | Duration |
|---|---|---|---|
| `ClaimReady` | Read Committed | `FOR UPDATE SKIP LOCKED` on ≤N rows | ~1ms |
| Executor call | *no transaction* | none | ms → minutes |
| Complete + advance | Read Committed | `FOR UPDATE` on 1 run row | ~2–10ms |
| Signal delivery | Read Committed | `FOR UPDATE` on 1 wait + 1 run row | ~5ms |
| Sweeper pass | Read Committed | `FOR UPDATE SKIP LOCKED` | ~10ms |
| Publish version | Read Committed | `FOR UPDATE` on 1 workflow row | ~5ms |

Read Committed is sufficient everywhere. Every place that would otherwise need
Serializable is covered by an explicit row lock plus a unique index. **Two rules:**

1. **Never open a transaction around an executor call.**
2. **Always take the run row lock before advancing the DAG**, and always in that
   order (run → nodes → waits) so you cannot deadlock against yourself.

---

## 12. Capacity notes

The claim query is a single index scan on a partial index containing only `ready`
rows — typically a few hundred entries regardless of whether the table holds
10 000 or 10 000 000 rows. Rough figures on a modest instance (4 vCPU):

- ~2 000 claims/sec at batch size 20
- Node throughput bound by executor latency, not by Postgres
- 30 workers × 32 concurrency = ~960 concurrent nodes on one database

The first thing to break at scale is **connection count**, not query throughput.
Use pgbouncer in transaction mode, or cap `pgxpool` at ~`4 × vCPU` per worker.

When you genuinely outgrow this, the seam is `store.ClaimReady`: swap in a real
queue and keep everything else. That day is much further away than it feels.

---

**Next:** `04-api.md` — the HTTP surface.
