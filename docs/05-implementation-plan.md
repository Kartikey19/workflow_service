# 05 — Implementation Plan

Go 1.27.0, PostgreSQL 16+. Roughly five weeks to a shippable v1 at a steady pace,
milestone by milestone, each independently demoable.

---

## 1. Repository layout

```
workflow_service/
  go.mod                        // module github.com/kartikey/workflow_service
  main.go                       // config, signals, exit status (APP_MODE=api today)
  Makefile
  .github/workflows/ci.yml       // checks with a PostgreSQL 16 service

  cmd/
    migrate/main.go             // forward-only embedded SQL migrations
    wfctl/main.go               // CLI: publish, lint, run, tail  (M6)

  pkg/
    config/       config.go             // environment loading and validation
    logger/       logger.go             // process logger construction
    db/           db.go                 // pgxpool connection construction
    migrate/      migrate.go            // transactional migration runner

  internal/
    app/          app.go                // composition and resource lifetime
    spec/         doc.go
                  document.go           // the YAML structs
                  compile.go            // decode → normalise → validate → graph → cel
                  validate.go           // the rule table from 02 §6.2
                  graph.go              // adjacency, topo sort, cycle detect, reachability
                  spec.go               // the compiled Spec type + accessors
                  input.go              // input schema validation
                  cache.go              // LRU by workflow_version_id (immutable ⇒ trivial)

    expr/         env.go                // CEL environment construction + bindings
                  resolve.go            // the ${...} template walker (02 §6.3)
                  funcs.go              // the closed function library

    executor/     executor.go           // Executor, Request, Result, Control, errors
                  registry.go

    executors/    noop/    http/    transform/
                  branch/  sleep/   waitevent/
                  agent/   humantask/                    // M7, M8

    engine/       worker.go  claim.go   dispatch.go
                  resolve.go lease.go   retry.go   sweep.go

    store/        queries/*.sql         // sqlc source of truth
                  gen/                  // sqlc output — never hand-edited
                  store.go              // workflow persistence interfaces + InTx

    api/          server.go  routes.go
                  workflows.go  runs.go  signals.go  ops.go
                  problem/              // RFC 7807

    events/       writer.go             // seq allocation, typed payloads
    obs/          metrics.go trace.go

  migrations/     0001_workflows.sql … 0006_blobs.sql
  docs/           00-overview.md … 05-implementation-plan.md
  test/
    integration/  // DATABASE_URL + isolated schemas, //go:build integration
    golden/       // workflow YAML fixtures + expected compiled specs
```

**Package boundary.** Put common infrastructure in `pkg/`: configuration,
logging, database connectivity, and other packages that can be reused without
depending on workflow-domain code. Keep workflow-specific implementation in
`internal/`; it may depend on `pkg/`, but `pkg/` must never depend on
`internal/`. `internal/app` is the composition root and wires those packages
together; `main.go` owns configuration loading, signals, and exit status.

**Dependency direction is strictly one-way:**

```
api ──▶ spec, store, events
engine ──▶ spec, store, executor, events
executors ──▶ executor          (never store, never engine)
spec ──▶ expr, executor(registry, for ValidateConfig only)
store ──▶ nothing
```

An executor that imports `store` is a design failure — it means a node type is
reaching into engine state instead of returning a `Control`. Enforce it with a
lint rule (`depguard`) so the review conversation happens automatically.

---

## 2. Dependencies

Deliberately small. Every one of these is load-bearing.

```
github.com/jackc/pgx/v5              // driver + pool. not database/sql
github.com/google/cel-go             // expressions
sigs.k8s.io/yaml                     // YAML→JSON→struct; gives you strict decoding
github.com/google/uuid               // UUIDv7
github.com/prometheus/client_golang  // metrics
github.com/stretchr/testify          // assertions

tool: github.com/sqlc-dev/sqlc       // SQL → typed Go
tool: github.com/golangci/golangci-lint
```

Standard library for the rest: `log/slog` for logging, `net/http` + `http.ServeMux`
for routing (method+wildcard patterns since 1.22 cover everything here — you do not
need chi), `encoding/json`, `context`.

Declare the tools with the `tool` directive in `go.mod` (Go 1.24+) so
`go tool sqlc generate` works with no separate install step and the version is
pinned in the module:

```
go 1.27.0

tool (
    github.com/sqlc-dev/sqlc/cmd/sqlc
    github.com/golangci/golangci-lint/cmd/golangci-lint
)
```

**Why sqlc and not an ORM.** The queries in `01 §6` are the engine — `FOR UPDATE
SKIP LOCKED`, `ON CONFLICT DO NOTHING`, `DISTINCT ON`, fenced conditional updates.
Every one of them is something an ORM will obscure or fight you on. sqlc keeps the
SQL as the source of truth and hands you typed Go for free.

---

## 3. Milestones

Each has a **demo** (what you can show) and **acceptance criteria** (what must be
true, expressed as tests you can write before the code).

### M0 — Skeleton (2–3 days)

Build: module, a local PostgreSQL migration command, migrations 0001–0004, sqlc
config, the store layer with the M0 queries from `01 §6`, `InTx` helper,
structured logging, config loading, `/healthz` + `/readyz`, CI (build, vet,
lint, test, integration test).

**Progress (2026-09-06)**
- [x] Created migrations `0001_workflows.sql` through `0004_run_events.sql`.
- [x] Applied all four migrations to an isolated PostgreSQL 16 database and
      verified that a second pass skipped all four recorded migration versions.
- [x] Added `sqlc.yaml`, the `ClaimReady` query, and generated the pgx/v5 store
      package in `internal/store/gen/`.
- [x] Added the integration-tagged concurrent `ClaimReady` test. It creates a
      fresh schema in the PostgreSQL instance named by `DATABASE_URL`, applies
      all four migrations, and was compile-checked with
      `go test -tags=integration -run '^$' ./test/integration`.
- [x] Ran `go test -v -tags=integration ./test/integration` against the local
      PostgreSQL 16 Docker container. The 100-row / four-claimer test passed.
      The harness uses the already-running PostgreSQL via `DATABASE_URL`.
- [x] Added `CompleteNode`, `EnqueueNode`, `DepStates`, and
      `ReclaimExpiredLeases`; regenerated sqlc code; and added their
      integration tests, plus `InTx` rollback-on-error and rollback-on-panic
      tests. The `CompleteNode` assertion compares JSONB structurally rather
      than relying on its formatting. `go vet ./...` and integration-test
      compilation pass.
- [x] Ran the expanded integration suite against the local PostgreSQL 16 Docker
      container. `ClaimReady`, `CompleteNode`, `EnqueueNode`, `DepStates`,
      `ReclaimExpiredLeases`, and `InTx` coverage passed.
- [x] Added unit tests for `pkg/config`, `pkg/db`, and `pkg/logger`; corrected
      the `testing` environment validation message and added positive
      `SHUTDOWN_GRACE` validation.
- [x] Added the standard-library HTTP server in `internal/api`, with tested
      `GET /healthz` and `GET /readyz` endpoints. `main.go` owns signals;
      `internal/app` constructs dependencies and `internal/api.Server.Run` owns
      graceful shutdown and connection closure using `SHUTDOWN_GRACE`. Readiness failures return `503` without exposing
      database errors.
- [x] Verified the current code with `go test ./...`, `go vet ./...`, and
      `go test -tags=integration -run '^$' ./test/integration`.

**Demo:** `make migrate && make test` against an existing PostgreSQL database.
Export `DATABASE_URL` first; see [local development](README.md#local-development).
`make test-integration` tests real PostgreSQL in temporary schemas.

**Acceptance**
- [x] Migrations apply cleanly to an empty DB and are idempotent on re-run.
- [x] `/healthz` reports process health independently of PostgreSQL; `/readyz`
      performs a bounded PostgreSQL ping and returns `503` when unavailable.
- [x] `store` has a test per query against real PostgreSQL via `DATABASE_URL`,
      with a fresh schema and isolated `search_path` for each test.
- [x] `ClaimReady` test: 4 goroutines claiming 100 ready rows concurrently yields
      **exactly 100 claims total, no row claimed twice.** This is the single most
      important test in the repo. Write it now, and never let it get skipped.
- [x] `InTx` rolls back on error and on panic.

**M0 tooling completed (2026-09-06)**
- [x] Added `cmd/migrate`, embedded SQL, and `pkg/migrate`: forward-only,
      transactional application with an advisory lock, version/checksum ledger,
      and rejection of changed, missing, or out-of-order applied history.
- [x] Added Makefile targets for migrations, formatting, sqlc generation freshness,
      vet, unit tests, and integration tests.
- [x] Retained the existing `DATABASE_URL` real-PostgreSQL harness; tests use
      random schemas without a `public` fallback and the production migration runner.
- [x] Added GitHub Actions CI with a PostgreSQL 16 service and all local checks,
      migration-command smoke checks, and race-enabled integration tests.
      Hosted execution remains unverified until the project is pushed to GitHub.
- [x] Local `make check` and `go test -race -count=1 -tags=integration ./test/integration`
      passed, including the live PostgreSQL integration suite.
      Migration tests cover concurrent runners, repeat runs, history validation,
      transactional rollback, and the command's first and second executions.
- Verification note: the initial command smoke test omitted its temporary schema
  from the subprocess connection and applied migrations 0001–0004 to the configured
  database's default schema. The test now explicitly passes `search_path`; both
  the normal and race-enabled suites passed after this correction.


**Application lifecycle follow-up (2026-09-06)**
- [x] Added typed application composition, synchronous listener binding, failure
      cleanup, and standard HTTP shutdown with connection closure on timeout.
- [x] API configuration no longer requires worker settings; startup explicitly
      rejects unimplemented worker/all modes. Added module-tidiness CI checks.
- [x] Added listener failure, occupied-port, dependency failure cleanup, graceful
      drain, forced cancellation, and real-PostgreSQL application readiness tests.

### M1 — The compiler (4–5 days)

Build: `spec` and `expr` packages end to end. `POST /v1/workflows` with
`?dry_run`. The diagnostic model. `GET /v1/workflows/...`.

**Demo:** publish the delivery workflow from `02 §5.1`; publish a broken one and
get all six errors in one response.

**Acceptance**
- [ ] Golden tests: `test/golden/*.yaml` → expected compiled spec JSON. One
      golden per feature; `-update` flag to regenerate.
- [ ] Every rule in `02 §6.2` has a failing-fixture test asserting the exact code.
- [x] `expr.Resolve` table test, ≥30 cases, covering `$${literal}`,
      `"a${x}b${y}c"`, whole-value objects/arrays/numbers/null, nested braces
      inside string literals, and unclosed `${`.
- [x] Cycle detection reports the actual cycle path, not just "cycle found".
- [ ] `spec_hash` is stable across map iteration order — compile the same YAML
      1 000 times, assert one distinct hash. (This will fail the first time you
      write it. Canonicalise before hashing.)
- [ ] Republishing identical YAML returns 200 `unchanged: true` and creates no row.

**M1 progress (2026-09-06)**
- [x] Implemented strict decoding, defaults, structural validation, graph building,
      topological ordering, cycle paths, and transitive dependencies.
- [x] Implemented template parsing, reusable CEL programs, recursive resolution,
      and typed whole-expression results with resolver tests.
- [x] Added `spec.Compile`: YAML through graph and expression compilation, with
      source paths and node IDs on diagnostics, and retained warnings.
- [x] Added AST-based expression scope checks: static dot/bracket node references,
      self-reference rejection, and rejection of dynamic/whole-map node access.
      Node configs and conditions see ancestors; correlation sees no nodes;
      workflow outputs may reference all nodes. Conditions must produce booleans.
- [x] Separated serializable normalized spec data from in-memory CEL programs.
      Durations are nanoseconds and normalized node lists are sorted.
- [ ] Add input-schema typing, executor registry/config validation, and remaining
      structural/expression validation fixtures before treating compilation as
      sufficient for publication. Dynamic input/output field types remain runtime checked.
- [ ] Add canonical hashing, golden fixtures, and publish/read APIs. This compiler
      stage does not yet validate registered executor types or persist workflows.


### M2 — Engine core (1 week) ← the hard one

Build: `Executor`/registry, `noop` + `transform`, claim loop, lease + heartbeat,
dispatcher, resolver, run completion, `POST /v1/runs`, `GET /v1/runs/{id}`,
`GET /v1/runs/{id}/nodes`.

**Demo:** run a 6-node diamond workflow to completion; kill -9 a worker mid-run
and watch another finish it.

**Acceptance**
- [ ] Linear 3-node workflow succeeds; `runs.output` matches `spec.outputs`.
- [ ] **Diamond test:** A → (B, C) → D. Assert D has exactly one node_execution
      row. Run it 200 times with 8 workers. Zero duplicates. (Race A, `03 §5.2`.)
- [ ] **Crash test:** `SIGKILL` a worker holding a lease. After the lease TTL, the
      sweeper resets the node and another worker completes the run. Assert
      `attempt` did **not** increment — lease expiry is re-delivery, not a retry.
- [ ] **Fencing test:** force a lost lease (expire it manually mid-execution),
      confirm `CompleteNode` affects 0 rows and the stale result is discarded.
- [ ] Run status transitions match `01 §3.2` for every node population.
- [ ] Invariants I1–I4 asserted by a `checkInvariants(t, db, runID)` helper called
      at the end of **every** engine test. Write the helper once; use it everywhere.

### M3 — The `http` executor (3–4 days)

Build: the executor, error classification, retry scheduling, backoff with jitter,
idempotency-key forwarding, egress allowlist, response size caps.

**Demo:** a workflow against `httptest` servers that 503 twice then succeed.

**Acceptance**
- [ ] 503 → retryable; 400 → permanent, no retry rows created.
- [ ] Attempts 1..N share **byte-identical** `idempotency_key`. (I5. This is the
      test that protects you from double-charging a customer.)
- [ ] `Retry-After: 30` on a 429 overrides the computed backoff.
- [ ] `max_attempts` exhausted → node `failed` → run `failed`, no extra rows.
- [ ] Egress allowlist blocks `169.254.169.254`, RFC1918, and loopback by default;
      test each explicitly.
- [ ] A response body over 256 KB fails cleanly with a typed error rather than
      writing a giant row.

### M4 — Branching (3 days)

Build: `branch` executor, `Control.Goto`, skip cascade, `join: any`, `on_failure`,
`if:`, the per-run node budget, `W_JOIN_ALL_ACROSS_BRANCH`.

**Demo:** the `support-triage` workflow (`02 §5.2`) taking both arms.

**Acceptance**
- [ ] Untaken arm is `skipped`, and the skip cascades to its whole subtree.
- [ ] `join: any` merge after a branch becomes `ready`; `join: all` becomes
      `skipped` (and the compiler warned about it at publish).
- [ ] `on_failure` runs the compensation node and the run ends **succeeded**.
- [ ] `if: false` skips and cascades identically to a branch prune.
- [ ] A workflow whose `on_failure` loops back trips `E_RUN_BUDGET` instead of
      filling the table.

### M5 — Time and events (4–5 days)

Build: `sleep`, `wait_event`, `run_waits`, the sweeper (all four passes),
`POST /v1/signals`, run deadlines.

**Demo:** the full `order-fulfilment` workflow, signalled by curl; then the same
workflow timing out into `reassign_rider`.

**Acceptance**
- [ ] A parked node holds **no lease and no goroutine** — assert
      `lease_owner is null` and that worker goroutine count is flat while parked.
- [ ] Signal delivery completes the node and advances the DAG in one transaction.
- [ ] Duplicate signal → 200 with the original result (not 404).
- [ ] Signal with no waiter → 404 `no_waiter`.
- [ ] Two runs registering the same `wait_key` → the second fails
      `E_WAIT_KEY_CONFLICT` at wait time, loudly.
- [ ] `wait_event` timeout routes to `on_timeout`; without it, the node fails.
- [ ] `deadline_at` passing fails the run and cancels its pending nodes.
- [ ] Use `testing/synctest` for the time-dependent tests if your toolchain has it
      (`go doc testing/synctest`) — it makes a 30-day sleep test run in
      microseconds and deterministically. Otherwise inject a `Clock` interface.
      **Do not write tests that actually sleep.**

### M6 — Observability & polish (3–4 days)

Build: `run_events` writer with gap-free seq, `GET /runs/{id}/events` + SSE,
cancellation, Prometheus metrics, `wfctl`, graceful shutdown with lease release.

**Demo:** `wfctl run order-fulfilment --input order_id=X --tail` streaming a live
timeline.

**Acceptance**
- [ ] `seq` is gap-free under 8 concurrent workers on one run (I6).
- [ ] Cancelling a run with a node mid-flight settles within one heartbeat and the
      in-flight result is discarded.
- [ ] `SIGTERM` releases leases; a rolling restart mid-run adds <1s of latency,
      not a full lease TTL.
- [ ] Every metric in §6 is emitted.

**v1 ships here.**

### M7 — `agent` (1 week)

Anthropic client, the tool-calling loop inside one executor, a tool registry,
`max_iterations` enforcement, structured output validation, token/cost metrics.

**Acceptance**
- [ ] `max_iterations` is enforced and reported in `metrics.iterations`.
- [ ] `cost_usd` populated; `select sum((metrics->>'cost_usd')::numeric) ... group
      by tenant_id` produces a plausible bill.
- [ ] Output failing `output_schema` is a **permanent** error, not retryable —
      retrying a schema-violating model response usually reproduces it, and you
      would rather see it fail once.
- [ ] Rate limits (429) classify as retryable and honour `Retry-After`.
- [ ] A spec with an `agent` node and no `max_iterations` fails to publish.

### M8 — `human_task` (1 week)

Task read-model table, queues, claim/complete API, SLA escalation via the existing
timeout path.

### M9 — `foreach` and sub-workflows (1–2 weeks)

Where `branch_key` finally earns the column you added in M0. Fan-out inserts N
child executions; a fan-in node with `join: all` over `branch_key='*'` collects
them. Sub-workflows use `parent_run_id` and a `wait_event` on child completion —
which means they need no new engine mechanism at all.

---

## 4. Testing strategy

Four layers. The middle two are where the bugs actually are.

**1. Unit — pure functions, no I/O.** `spec` compilation, `expr.Resolve`, backoff,
error classification. Table-driven. Should be thousands of cases and run in under
a second.

**2. Store — real Postgres via `DATABASE_URL`.** Use an existing local database
or a CI PostgreSQL service, with a fresh schema per test
(`create schema test_<random>; set search_path`). Never mock
the database: every interesting query here uses a Postgres-specific concurrency
primitive, and a mock will happily tell you your broken `SKIP LOCKED` works.

**3. Engine — real Postgres, real workers, fake executors.** Register a
`testExecutor` whose behaviour is scripted per node (`succeed`, `fail(code)`,
`hang(d)`, `panic`). This layer is where you prove the invariants, and it is where
you should spend most of your testing effort.

```go
func TestDiamondNoDuplicates(t *testing.T) {
    db := testdb.New(t)
    reg := executor.NewRegistry(); reg.Register(testexec.New(map[string]testexec.Behaviour{
        "a": testexec.Succeed(), "b": testexec.Delay(50 * time.Millisecond),
        "c": testexec.Succeed(), "d": testexec.Succeed(),
    }))
    workers := startWorkers(t, db, reg, 8)
    defer workers.Stop()

    runID := publishAndRun(t, db, diamondYAML, nil)
    waitForTerminal(t, db, runID, 10*time.Second)

    require.Equal(t, "succeeded", statusOf(t, db, runID))
    require.Len(t, nodeExecutions(t, db, runID, "d"), 1)   // exactly one
    checkInvariants(t, db, runID)                          // I1–I6, every test
}
```

**4. End-to-end — full binary, HTTP, `httptest` upstreams.** One test per example
workflow in `02 §5`. These are your regression suite for the YAML surface, and
they double as the documentation nobody has to keep up to date manually.

### Chaos tests (worth the day they cost)

Run these in CI nightly, not per-PR:

- Kill a random worker every 2s during a 500-run load. Assert every run reaches a
  terminal state and no node has two `succeeded` rows.
- Inject 20% `ErrConnClosed` at the pool layer. Assert the same.
- Run the whole suite with `-race`. Always.

### The invariant helper

Write this in M0 and call it at the end of every engine test:

```go
func checkInvariants(t *testing.T, db *pgxpool.Pool, runID uuid.UUID) {
    t.Helper()
    // I1: no duplicate identity
    // I3: lease_owner non-null ⟺ status='running'
    // I4: run terminal ⟺ no node in (ready,running,waiting)
    // I5: one distinct idempotency_key per (node_id, branch_key)
    // I6: run_events.seq is 1..n with no gaps
}
```

Six SQL queries, twenty minutes to write, and it will catch more real bugs than
any other single thing in this plan.

---

## 5. Configuration

Environment-driven, validated at startup, fail fast on anything missing.

```go
type Config struct {
    DatabaseURL     string        `env:"DATABASE_URL,required"`
    Mode            string        `env:"MODE" default:"all"`        // api|worker|all
    HTTPAddr        string        `env:"HTTP_ADDR" default:":8080"`

    WorkerID        string        `env:"WORKER_ID"`                 // default: hostname+pid
    Concurrency     int           `env:"CONCURRENCY" default:"32"`
    BatchSize       int           `env:"BATCH_SIZE" default:"20"`
    PollInterval    time.Duration `env:"POLL_INTERVAL" default:"200ms"`
    LeaseTTL        time.Duration `env:"LEASE_TTL" default:"60s"`
    SweepInterval   time.Duration `env:"SWEEP_INTERVAL" default:"5s"`
    ShutdownGrace   time.Duration `env:"SHUTDOWN_GRACE" default:"30s"`

    MaxPayloadBytes int           `env:"MAX_PAYLOAD_BYTES" default:"262144"`
    MaxNodesPerRun  int           `env:"MAX_NODES_PER_RUN" default:"1000"`
    DefaultRunTTL   time.Duration `env:"DEFAULT_RUN_TTL" default:"720h"`

    EgressAllowlist []string      `env:"EGRESS_ALLOWLIST"`          // CIDRs / hostnames
}
```

Tuning notes:

- `LeaseTTL` must exceed your slowest node's p99, or the heartbeat must be
  reliable. With the heartbeat (`03 §4.3`) 60s is fine even for slow agent calls.
- `Concurrency × replicas` is your concurrent-node ceiling. Each running node
  holds one goroutine, not one DB connection — connections are held only during
  the short transactions.
- `BatchSize` larger than `Concurrency` is always wrong (`03 §9`).

---

## 6. Observability

**Metrics** (Prometheus):

```
wf_nodes_claimed_total{worker}
wf_node_duration_seconds{node_type,status}          histogram
wf_node_attempts_total{node_type,status,code}
wf_lease_expired_total{node_type}                   ← nonzero means trouble
wf_lease_lost_total{node_type}                      ← fenced writes discarded
wf_claim_batch_size                                 histogram
wf_ready_backlog{}                                  gauge, from the sweeper
wf_runs_active{status}                              gauge
wf_run_duration_seconds{workflow,status}            histogram
wf_signal_total{result}                             delivered|no_waiter|duplicate
wf_sweeper_actions_total{action}
wf_agent_cost_usd_total{tenant,model}               (M7)
```

The two to alert on:
- `wf_ready_backlog` rising steadily ⇒ under-provisioned workers.
- `wf_lease_expired_total` above ~0 ⇒ workers dying, or `LeaseTTL` shorter than
  real node latency. Both are silent correctness-adjacent problems that show up as
  mysterious duplicate side effects.

**Logging** (`log/slog`, JSON). Every log line from the engine carries
`run_id`, `node_id`, `attempt`, `node_execution_id`, `worker_id`. Add them once
with `slog.With` in the dispatcher and never think about it again.

**Tracing** (OpenTelemetry, optional but cheap): one span per run (link, don't
nest — runs outlive requests), one per node execution, propagate context into the
`http` executor so downstream services join the trace. For a workflow engine this
pays for itself the first time someone asks why a run took nine minutes.

---

## 7. Ops runbook (write it now, not after the incident)

| Symptom | Query | Fix |
|---|---|---|
| Runs stuck `running`, nothing ready | `select status,count(*) from node_executions where run_id=$1 group by 1` | Resolver failed to enqueue — engine bug. `03 §8.4` should have alerted. |
| Backlog growing | `select count(*) from node_executions where status='ready' and run_after<=now()` | Scale workers, or raise `Concurrency` |
| One workflow eating capacity | group the above by `node_type` | Rate-limit that tenant; consider a separate worker pool |
| Duplicate side effects | `wf_lease_expired_total` | Raise `LeaseTTL`; verify executor idempotency |
| Signals 404-ing | `wf_signal_total{result="no_waiter"}` | Early signals (`03 §7.3`) — caller must retry, or implement option (b) |
| DB CPU high | `pg_stat_statements` | Almost always a missing partial index; check `01 §2.5` |

**Manual interventions**, as `wfctl` subcommands so nobody hand-writes UPDATEs
against production at 3am:

```
wfctl run cancel <run-id>
wfctl node retry <node-execution-id>      -- insert attempt+1 as ready
wfctl node skip  <node-execution-id>      -- force-skip a wedged node, then advance
wfctl signal <key> --payload @file.json
wfctl lease release --worker <id>         -- after a hard crash
```

---

## 8. Order of work, concretely

If you want a single sequence to follow without re-reading everything:

1. Export `DATABASE_URL` for an existing PostgreSQL instance; run `make migrate`.
2. Write `01 §6`'s queries as `.sql`, generate with sqlc, test each against a real
   DB. **Write the concurrent `ClaimReady` test first** — it defines the shape of
   everything after it.
3. `spec` package: structs → decode → normalise → validate → graph. Golden tests.
4. `expr`: CEL env + the template walker. The 30-case table test.
5. Wire `POST /v1/workflows` with `?dry_run`. You can now lint YAML in CI.
6. `executor` interface, registry, `noop`, `transform`.
7. The claim loop and dispatcher, single worker, no retries. Run a linear
   workflow end to end. **This is the first real milestone — everything after it
   is filling in.**
8. The resolver: successors, joins, run completion. Diamond test with 8 workers.
9. Lease, heartbeat, fencing, sweeper pass 1. Crash test.
10. `http` executor, retries, idempotency keys.
11. `branch` + skip cascade.
12. `sleep`, `wait_event`, signals, sweeper passes 2–4.
13. Events, SSE, cancellation, metrics, `wfctl`.
14. Ship. Then `agent`.

Step 7 is the moment the thing becomes real. Everything before it is scaffolding
and everything after it is a plugin. If you are going to timebox one part of this,
make sure it is not steps 8–9 — the resolver and the lease are where correctness
lives, and they are almost impossible to retrofit once workflows are in
production.

---

## 9. Things you will be tempted to do — don't

- **Pre-create all node rows at run start.** Feels tidier. Breaks the moment you
  add `foreach` in M9, and by then a lot of code assumes it.
- **Store the run's variables in a mutable `context` column.** It is the obvious
  design and it is a lost-update race between every pair of parallel nodes (P3).
- **Derive the idempotency key from `node_executions.id`.** Per-attempt keys make
  retries duplicate side effects. This is the bug you find in production, on a
  payment (`01 §5`).
- **Hold a transaction across the executor call.** Works fine at 10 runs/day, then
  pins your xmin horizon and stops autovacuum at 10 000.
- **Skip the run row lock in the resolver.** Everything works until two nodes
  finish in the same millisecond.
- **Let `run_events` carry full payloads.** Your event log becomes 10× your data
  and the retention conversation becomes urgent.
- **Add "just one" expression helper function that does I/O.** It is permanent and
  it makes every expression un-analysable.
- **Build the UI before the event log.** You will build the wrong UI.

---

## 10. Definition of done for v1

- [ ] All three example workflows in `02 §5` run end to end against real HTTP
      upstreams, including the timeout and compensation paths.
- [ ] Invariants I1–I7 asserted by `checkInvariants` in every engine test.
- [ ] 8-worker diamond test, 200 iterations, zero duplicates, with `-race`.
- [ ] Kill -9 chaos test: every run terminal, no duplicate `succeeded` rows.
- [ ] Rolling restart mid-run costs <1s, not a lease TTL.
- [ ] A 30-day `sleep` holds one row, zero goroutines (asserted, not assumed).
- [ ] `?dry_run=true` wired into CI as a workflow lint.
- [ ] Runbook §7 written, and every `wfctl` command in it actually exists.
