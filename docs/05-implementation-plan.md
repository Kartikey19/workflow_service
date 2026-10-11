# 05 — Implementation Plan

Go 1.27.0, PostgreSQL 16+. Roughly five weeks to a shippable engine v1 at a steady
pace, milestone by milestone, each independently demoable. Milestones M10–M19
(§3.1) then turn the engine into a hosted product.

---

## 0. Product scope (updated 2026-10-10)

The engine (M0–M9) is the core. The product built around it must let people:

| # | Requirement | Milestone |
|---|---|---|
| 1 | Create workflows easily in a UI; an assistant agent turns written requirements into YAML that can be reviewed and published from the UI | M17, M18 |
| 2 | Update workflows in the UI (each save publishes a new immutable version) | M18 |
| 3 | See run status: list, detail, node timeline, inputs/outputs, live events | M6, M18 |
| 4 | Create agents: pick a provider and model (4.1), declare input (4.2) and output (4.3), use the agent as a workflow node (4.4), and add provider API keys with a connection test before saving (4.5) | M13 |
| 5 | Send email through a configured integration, or with credentials passed in the request | M14 |
| 6 | Integrations with third-party apps: Gmail, Dropbox, Google Drive, OneDrive, and database connectors to save data | M14 |
| 7 | Define data-exploration agents (an agent with read-only database tools) | M15 |
| 8 | MCP connectors (agents call tools on remote MCP servers) | M15 |
| 9 | Trigger a workflow run from a webhook | M16 |
| 10 | A vault for credentials (API keys and so on) that workflows can reference, e.g. `${secrets.API_KEY}` | M12 |
| 11 | Separate entry points for the API server and the worker; horizontally scalable and controllable | M10 |
| 12 | Cheap infrastructure; price is a concern | M19 |
| 13 | Free observability | M6, M19 |
| 14 | Production deployment | M19 |

### 0.1 Decisions

- **Tenancy.** An *organization* is the tenant (`tenant_id` everywhere is the
  organization ID). A user can belong to several organizations and switch
  between them, so a team designs and runs workflows together.
- **Roles.** `owner`, `admin`, `editor` (create and edit workflows, agents and
  connections, start runs), `viewer` (read only). A platform-admin flag on the user
  allows inviting new organizations.
- **Sign-up.** Invite-only at launch: a platform admin invites a user to a new
  organization, and organization admins invite members. Self sign-up with paid
  plans comes later behind a `SIGNUP_ENABLED` flag; the data model supports it
  from day one.
- **Plans and limits.** Plans are rows, not code. Limits are enforced at publish
  time (workflow count, steps per workflow) and at run creation (runs per calendar
  month). Starting values:

  | Plan | Workflows | Steps per workflow | Runs per month |
  |---|---|---|---|
  | Free | 1 | 5 | 20 |
  | Pro (paid) | 50 | 50 | 10 000 |
  | Team (paid) | unlimited | 200 | 100 000 |

  Paid limits are placeholders until pricing is decided. Billing (Stripe) is
  out of scope until self sign-up opens; an admin assigns plans manually meanwhile.
- **LLM providers at launch.** Anthropic, OpenAI, any OpenAI-compatible endpoint
  (Groq, Together, OpenRouter, Ollama, vLLM and others via base URL), and Google
  Gemini. Clients are plain `net/http`, with no vendor SDKs, behind one internal
  interface.
- **Credentials.** Provider keys, integration credentials and OAuth tokens are
  stored encrypted (AES-256-GCM, master key from the environment, key version
  column for rotation). Secrets are decrypted only inside the worker, passed to
  executors, and never written to `node_executions.input`, logs or events.
- **Integrations that use OAuth** (Gmail, Google Drive, Dropbox, OneDrive) need an
  OAuth app registered by the operator with each provider. Client IDs and secrets
  come from the environment; integrations without them are hidden in the UI.
- **MCP.** Only remote MCP servers over HTTP (streamable HTTP and SSE). Stdio MCP
  servers are not supported in a multi-tenant service because they would run
  tenant-supplied commands on our hosts.
- **Egress.** Every outbound connection made on a tenant's behalf (http nodes,
  MCP, OpenAI-compatible base URLs, database connectors) goes through
  `pkg/netguard`. It blocks loopback, private, link-local and metadata
  addresses unless they are allowed by `EGRESS_ALLOW_CIDRS`.
- **UI.** React, Vite and TypeScript, built into `web/dist` and embedded in the API
  binary with `go:embed`, so one image serves both. Same-origin session cookies
  (HttpOnly, SameSite=Lax); API keys (`Authorization: Bearer`) for programmatic
  use and webhooks.
- **Infrastructure.** No decision yet; see the comparison in §11. The
  recommendation is one small dedicated droplet running Docker Compose (Caddy,
  API, N workers, PostgreSQL) with nightly `pg_dump` to object storage, at roughly
  $12/month. Scale-to-zero serverless is a poor fit because workers must stay up to
  poll, hold leases and run long LLM calls. Everything ships as Docker images
  configured by environment, so moving host is configuration, not code.
- **Observability.** Prometheus `/metrics` on the API and the worker, JSON logs,
  and Grafana Cloud's free tier (10k series, 50 GB logs, 14-day retention) fed by
  a Grafana Alloy agent on the host. Tracing (OpenTelemetry) is optional, later.

---

## 1. Repository layout

```
workflow_service/
  go.mod                        // module github.com/kartikey/workflow_service
  Makefile
  .github/workflows/ci.yml       // checks with a PostgreSQL 16 service

  cmd/                          // one directory per binary; main.go stays tiny
    api/main.go                 // HTTP API + UI; `api migrate` applies migrations (M10)
    worker/main.go              // claim loop, dispatcher, sweeper (M10)

  pkg/                          // generic, domain-free packages; no import of internal/
    auth/         principal.go          // Principal, roles, Authenticator, DevHeader
    exception/    exception.go          // HTTP-aware exceptions, RFC 7807 bodies
    httpx/        json.go recorder.go   // JSON read/write helpers, status recorder
    uid/          uid.go                // UUIDv7 generation, parse, format
    logger/       logger.go             // zap JSON logger construction
    db/           db.go migrate.go tx.go // pgxpool, goose migrations, InTx
    jsonschema/   jsonschema.go         // JSON Schema subset: wait payloads, agent output
    netguard/     netguard.go           // SSRF-safe dialer and HTTP client
    cache/        lru.go                // generic LRU: Get, Add, Len
    obs/          metrics.go            // Prometheus registry and metrics
    events/       hub.go                // shared LISTEN wf_events fan-out for SSE

  internal/
    app/          app.go dependencies.go main.go // composition root, wiring, signals
    config/       config.go             // env struct tags (caarlos0/env) + .env
    server/       server.go middleware.go // http.Server lifecycle, health, auth middleware
    api/          handler.go errors.go  // /be/v1 handlers only: routes, domain→exception map;
                  workflows.go runs.go signals.go events.go executors.go
                  query.go views.go     // query-string/cursor helpers, JSON views
                  // defines the service interfaces it consumes
    spec/         document.go compile.go validate.go graph.go input.go ...
                  // YAML structs → decode → normalise → validate → graph → cel
    core/         // the workflow engine and what it executes
      engine/     config.go worker.go claim.go dispatch.go persist.go lease.go retry.go sweep.go
      expr/       env.go resolve.go funcs.go // CEL environment, ${...} walker (02 §6.3)
      executor/   executor.go registry.go fields.go // Executor, Request, Result, Control, errors
        builtin/  (noop, transform, branch, sleep, wait_event)
        httpexec/ (http)
        agent/ (M13)  email/ gmail/ gdrive/ dropbox/ onedrive/
        dbquery/ mcp/ (M14, M15)   humantask/ (M8)
    services/     // application use-cases shared by the API and the worker
      runs/       service.go queries.go start.go resolve.go control.go specs.go nodekey.go
                  // run transitions (start, advance, recompute, cancel, signal, wake
                  // sleeps, deadlines) plus plan allowance, reads, retry
      workflows/  publish.go query.go plans.go // publish (compile → hash → version max+1),
                  // list/get/archive, plan limits
    identity/     // users, orgs, sessions, API keys, invites, plans  (M11)
    vault/        // AES-GCM secret storage, SecretSource             (M12)
    llm/          // anthropic, openai, openaicompat, gemini clients  (M13)
    agents/       // agent definitions, test-connection               (M13)
    connections/  // integration credentials, OAuth flows             (M14)
    mcpclient/    // MCP over HTTP: initialize, tools/list, call      (M15)
    assistant/    // requirements → YAML generation loop              (M17)
    store/        queries/*.sql         // sqlc source of truth
                  gen/                  // sqlc output — never hand-edited

  web/            // React + Vite SPA, embedded into the API binary   (M18)
  deploy/         // Dockerfile, compose, Caddyfile, Alloy, backups   (M19)
  migrations/     0001_workflows.sql …  // embedded, goose format
  docs/           00-overview.md … 05-implementation-plan.md
  test/
    integration/  // DATABASE_URL + isolated schemas, //go:build integration
    golden/       // workflow YAML fixtures + expected compiled specs
```

**Layout (restructured 2026-10-11).** This follows the standard Go application
layout: every binary is a directory under `cmd/` whose `main.go` only calls
`internal/app.Main`. Generic, domain-free packages that could be reused unchanged
(`auth`, `exception`, `httpx`, `uid`, `logger`, `db`, `jsonschema`, `netguard`,
`cache`, `obs`, `events`) live
in `pkg/` and never import `internal/`. Everything that knows about workflows,
runs or nodes is under `internal/`. `internal/api` holds HTTP handlers only; the
server lifecycle and authentication middleware are in `internal/server`, and
`internal/core` groups the engine, the expression language and the single
`executor` tree (contract at the top, node types in subdirectories), and
`internal/services` holds the use-cases (`runs`, `workflows`) that the API and
the worker share. There are two binaries, `api` and `worker`. Well-known libraries replace hand-written infrastructure:
`caarlos0/env` reads configuration from struct tags, `joho/godotenv` loads
`.env`, and `pressly/goose` applies migrations. `internal/app` is the
composition root and wires packages together.

**Dependency direction is strictly one-way:**

```
server ──▶ api, pkg/*
api ──▶ pkg/*; talks to services/runs, services/workflows and core/executor
        through interfaces it defines itself
services/runs ──▶ core/expr, spec, store, pkg/*
services/workflows ──▶ spec, core/executor, store, pkg/*
core/engine ──▶ services/runs, spec, store, core/executor, core/expr
core/executor/* ──▶ core/executor, pkg/*  (never store, never engine)
spec ──▶ core/expr, core/executor(registry, for ValidateConfig only)
pkg/* ──▶ nothing in internal/
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
- [x] Added the standard-library HTTP server in `internal/server`, with tested
      `GET /healthz` and `GET /readyz` endpoints. `main.go` owns signals;
      `internal/app` constructs dependencies and `internal/server.Server.Run` owns
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
- [x] Added `api migrate` (formerly `cmd/migrate`), embedded SQL, and `pkg/migrate`: forward-only,
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

Build: `spec` and `expr` packages end to end. `POST /be/v1/workflows` with
`?dry_run`. The diagnostic model. `GET /be/v1/workflows/...`.

**Demo:** publish the delivery workflow from `02 §5.1`; publish a broken one and
get all six errors in one response.

**Acceptance**
- [x] Golden tests: `test/golden/*.yaml` → expected compiled spec JSON. One
      golden per feature; `-update` flag to regenerate.
- [x] Every rule in `02 §6.2` has a failing-fixture test asserting the exact code.
- [x] `expr.Resolve` table test, ≥30 cases, covering `$${literal}`,
      `"a${x}b${y}c"`, whole-value objects/arrays/numbers/null, nested braces
      inside string literals, and unclosed `${`.
- [x] Cycle detection reports the actual cycle path, not just "cycle found".
- [x] `spec_hash` is stable across map iteration order — compile the same YAML
      1 000 times, assert one distinct hash. (This will fail the first time you
      write it. Canonicalise before hashing.)
- [x] Republishing identical YAML returns 200 `unchanged: true` and creates no row.

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
- [x] Add input-schema typing, executor registry/config validation, and remaining
      structural/expression validation fixtures before treating compilation as
      sufficient for publication. Dynamic input/output field types remain runtime checked.
- [x] Add canonical hashing, golden fixtures, and publish/read APIs. This compiler
      stage does not yet validate registered executor types or persist workflows.

**M1 progress (2026-10-10)**
- [x] `spec.CompileWith(source, Options{Registry})`: unknown node types
      (`E_UNKNOWN_TYPE`) and executor `ValidateConfig` (`E_CONFIG`) at publish.
- [x] `E_BRANCH_TARGET` and `E_FAILURE_TARGET`: `goto`, `on_failure` and
      `on_timeout` targets must list their source in `needs`.
- [x] `W_JOIN_ALL_ACROSS_BRANCH` warning.
- [x] Input schema validation at publish (`E_INPUT_SCHEMA`) and
      `spec.ValidateInput` at run creation (defaults, types, enum, format,
      min/max, pattern; undeclared fields pass through for webhook payloads).
- [x] `Spec.Hash()` (sha256 of canonical JSON) and `spec.Load` (rebuild programs
      from the stored spec, not the YAML).
- [x] `internal/services/workflows.Service.Publish`: row lock, hash dedupe
      (`unchanged`), version max+1, plan limits (workflows, steps).
- [x] Expression library: CEL strings, encoders (base64), math, lists, sets,
      optional types and `list.sum()`.
- [x] Golden fixtures; spec_hash 1 000-iteration stability test; HTTP publish/read
      endpoints (M10).


### M2 — Engine core (1 week) ← the hard one

Build: `Executor`/registry, `noop` + `transform`, claim loop, lease + heartbeat,
dispatcher, resolver, run completion, `POST /be/v1/runs`, `GET /be/v1/runs/{id}`,
`GET /be/v1/runs/{id}/nodes`.

**Demo:** run a 6-node diamond workflow to completion; kill -9 a worker mid-run
and watch another finish it.

**Acceptance**
- [x] Linear 3-node workflow succeeds; `runs.output` matches `spec.outputs`.
- [x] **Diamond test:** A → (B, C) → D. Assert D has exactly one node_execution
      row. Run it 200 times with 8 workers. Zero duplicates. (Race A, `03 §5.2`.)
- [x] **Crash test:** `SIGKILL` a worker holding a lease. After the lease TTL, the
      sweeper resets the node and another worker completes the run. Assert
      `attempt` did **not** increment — lease expiry is re-delivery, not a retry.
- [x] **Fencing test:** force a lost lease (expire it manually mid-execution),
      confirm `CompleteNode` affects 0 rows and the stale result is discarded.
- [x] Run status transitions match `01 §3.2` for every node population.
- [x] Invariants I1–I4 asserted by a `checkInvariants(t, db, runID)` helper called
      at the end of **every** engine test. Write the helper once; use it everywhere.

**M2–M5 progress (2026-10-10)**
- [x] `internal/services/runs`: spec cache (LRU by version ID), `Start` (run plus roots,
      idempotency key with request-hash conflict detection, correlation,
      deadline), `Advance` (successors, joins, failure handlers, branch pruning,
      skip cascade, run budget), `Recompute` (run status, outputs, unhandled
      failure), `Cancel`, `Signal`, `WakeDue`, `FailPastDeadline`; every
      transition under the run row lock; gap-free `run_events`; `pg_notify`.
- [x] Dispatcher rewritten on `flow`: cached programs, `if` skip, secrets resolved
      twice (real for the executor, `***` for the stored input), 256 KB input and
      output caps, `Control` for branch/sleep/wait, `E_WAIT_KEY_CONFLICT`, panic
      containment, metrics, cancelled-run handling.
- [x] Worker: capacity-aware claim loop with `LISTEN wf_ready` wakeups, sweeper
      (reclaim leases, wake sleeps and time out waits, run deadlines, jittered),
      graceful shutdown that releases leases.
- [x] Executors: `noop`, `transform`, `branch`, `sleep`, `wait_event`, `http`
      (error classification, Retry-After, Idempotency-Key, 256 KB cap, SSRF guard).
- [x] Migration `0005_engine.sql`: `run_waits`, `runs.request_hash`,
      `runs.created_by`, list indexes, `workflow_versions` immutability trigger (I7).
- [x] Engine integration tests with a `checkInvariants` helper (I1, I3–I6, no
      double success): linear with outputs, 60-run diamond on 8 workers, branch
      with `join: any`, retries with a stable key, `on_failure` and unhandled
      failure, sleep plus signal plus duplicate signal, cancel mid-node,
      idempotent start, shutdown lease release. All passed with `-race`.
- [x] `test/integration/migrations_test.go` counts embedded migrations instead
      of hard-coding 4.
- [x] Re-run the full integration suite after that change (not yet re-run).
- [x] Kill -9 crash test; 200-iteration diamond; wait payload `schema` validation.

### M3 — The `http` executor (3–4 days)

Build: the executor, error classification, retry scheduling, backoff with jitter,
idempotency-key forwarding, egress allowlist, response size caps.

**Demo:** a workflow against `httptest` servers that 503 twice then succeed.

**Acceptance**
- [x] 503 → retryable; 400 → permanent, no retry rows created.
- [x] Attempts 1..N share **byte-identical** `idempotency_key`. (I5. This is the
      test that protects you from double-charging a customer.)
- [x] `Retry-After: 30` on a 429 overrides the computed backoff.
- [x] `max_attempts` exhausted → node `failed` → run `failed`, no extra rows.
- [x] Egress allowlist blocks `169.254.169.254`, RFC1918, and loopback by default;
      test each explicitly.
- [x] A response body over 256 KB fails cleanly with a typed error rather than
      writing a giant row.

### M4 — Branching (3 days)

Build: `branch` executor, `Control.Goto`, skip cascade, `join: any`, `on_failure`,
`if:`, the per-run node budget, `W_JOIN_ALL_ACROSS_BRANCH`.

**Demo:** the `support-triage` workflow (`02 §5.2`) taking both arms.

**Acceptance**
- [x] Untaken arm is `skipped`, and the skip cascades to its whole subtree.
- [x] `join: any` merge after a branch becomes `ready`; `join: all` becomes
      `skipped` (and the compiler warned about it at publish).
- [x] `on_failure` runs the compensation node and the run ends **succeeded**.
- [x] `if: false` skips and cascades identically to a branch prune.
- [x] A workflow whose `on_failure` loops back trips `E_RUN_BUDGET` instead of
      filling the table.

### M5 — Time and events (4–5 days)

Build: `sleep`, `wait_event`, `run_waits`, the sweeper (all four passes),
`POST /be/v1/signals`, run deadlines.

**Demo:** the full `order-fulfilment` workflow, signalled by curl; then the same
workflow timing out into `reassign_rider`.

**Acceptance**
- [x] A parked node holds **no lease and no goroutine** — assert
      `lease_owner is null` and that worker goroutine count is flat while parked.
- [x] Signal delivery completes the node and advances the DAG in one transaction.
- [x] Duplicate signal → 200 with the original result (not 404).
- [x] Signal with no waiter → 404 `no_waiter`.
- [x] Two runs registering the same `wait_key` → the second fails
      `E_WAIT_KEY_CONFLICT` at wait time, loudly.
- [x] `wait_event` timeout routes to `on_timeout`; without it, the node fails.
- [x] `deadline_at` passing fails the run and cancels its pending nodes.
- [ ] Use `testing/synctest` for the time-dependent tests if your toolchain has it
      (`go doc testing/synctest`) — it makes a 30-day sleep test run in
      microseconds and deterministically. Otherwise inject a `Clock` interface.
      **Do not write tests that actually sleep.**

### M6 — Observability & polish (3–4 days)

Build: `run_events` writer with gap-free seq, `GET /runs/{id}/events` + SSE,
cancellation, Prometheus metrics, the admin API (`04 §5.1`), graceful shutdown with lease release.

**Demo:** `POST /be/v1/runs` for `order-fulfilment`, then
`curl -N /be/v1/runs/{id}/events` streaming a live timeline.

**Acceptance**
- [x] `seq` is gap-free under 8 concurrent workers on one run (I6).
- [x] Cancelling a run with a node mid-flight settles within one heartbeat and the
      in-flight result is discarded.
- [x] `SIGTERM` releases leases; a rolling restart mid-run adds <1s of latency,
      not a full lease TTL.
- [x] Every metric in §6 is emitted.

**v1 ships here.**

### M7 — `agent` (1 week)

Anthropic client, the tool-calling loop inside one executor, a tool registry,
`max_iterations` enforcement, structured output validation, token/cost metrics.

**Acceptance**
- [x] `max_iterations` is enforced and reported in `metrics.iterations`.
- [x] `cost_usd` populated; `select sum((metrics->>'cost_usd')::numeric) ... group
      by tenant_id` produces a plausible bill.
- [x] Output failing `output_schema` is a **permanent** error, not retryable —
      retrying a schema-violating model response usually reproduces it, and you
      would rather see it fail once.
- [x] Rate limits (429) classify as retryable and honour `Retry-After`.
- [x] A spec with an `agent` node and no `max_iterations` fails to publish.

**Built (2026-10-11).** `pkg/llm` is the provider-neutral interface (messages,
tool calls, usage, errors, per-model price estimates) and `pkg/llm/anthropic` its
plain `net/http` client. `internal/core/executor/agent` holds the executor, the
tool registry (`current_time` ships built in) and the `Clients` seam where
per-organization provider connections plug in at M13. The platform key comes from
`ANTHROPIC_API_KEY`; without it agent nodes publish but fail permanently with
`llm_not_configured`. Prices are estimates per model family and can be overridden
with `LLM_PRICING_JSON`. Without `output_schema` the output is `{"text": ...}`.

### M8 — `human_task` (1 week)

Task read-model table, queues, claim/complete API, SLA escalation via the existing
timeout path.

**Built (2026-10-11).** Migration `0008_tasks.sql` adds `tasks` and a
`task_details` view that derives status from the wait (open, claimed, completed,
expired, cancelled), so timeouts and cancelled runs need no extra bookkeeping.
`executor.WaitSpec` carries an optional `TaskSpec`; the engine writes the task row
in the same transaction as the wait. `human_task` is a built-in node,
`internal/services/tasks` is the service (list, get, claim, release, complete over
the signal path) and `/be/v1/tasks` the routes (`04 §4.2`). `on_timeout` is now also
allowed on `human_task`.

**Acceptance**
- [x] A task appears in its queue, can be claimed, released and completed, and its
      payload becomes the node's output.
- [x] An SLA timeout escalates through `on_timeout`; the task shows as expired and
      can no longer be completed.
- [x] Assigned tasks are workable only by the assignee or an admin; another
      organization never sees a task.
- [x] The healthcare example (`02 §5.3`) runs end to end, including the SLA path.

### M9 — `foreach` and sub-workflows (1–2 weeks)

Where `branch_key` finally earns the column you added in M0. Fan-out inserts N
child executions; a fan-in node with `join: all` over `branch_key='*'` collects
them. Sub-workflows use `parent_run_id` and a `wait_event` on child completion —
which means they need no new engine mechanism at all.

**foreach — built (2026-10-11).** Spec in `02 §4.9`. A controller row evaluates
`items` and parks with no wake-up time; branch rows (`branch_key` = index, element
stored in the new `node_executions.item`, migration `0009`) run the node; the last
branch to finish completes the controller inside `runs.Service.Advance`, and the
node then advances normally with the list of branch outputs. Compile-time scope
makes `item` and `index` legal only inside a foreach node.

**Acceptance (foreach)**
- [x] One execution per element, outputs in element order, successor runs once.
- [x] Empty list, per-element `if`, retries keep their element and key.
- [x] A failed branch fails the node and runs `on_failure`; unhandled, it fails the run.
- [x] 40 runs × 12 branches across 8 workers: one controller, one successor, no
      duplicate branch, under `-race`.
- [x] A foreach over `human_task` creates one task per element.

**Sub-workflows — built (2026-10-11).** The `workflow` node (`02 §4.10`) starts a
child run through `executor.ChildStarter` (the executor never touches the store;
the child's idempotency key is the node's stable key) and parks on a wait keyed
`subworkflow:<child run id>`. `runs.Service.CompleteSubworkflow` completes that
node when the child ends, called after the child's transaction commits and backed
by a sweeper pass for missed notifications. Cancelling or ending a parent cancels
its unfinished children (parent lock first, then child, everywhere); a parent that
gives up waiting cancels the child it started. Nesting is capped at 5. The wait
prefixes `task:` and `subworkflow:` are reserved for the engine: `wait_event` and
`POST /be/v1/signals` reject them, which also closes a way to bypass task assignee
rules through the signals endpoint.

**Acceptance (sub-workflows)**
- [x] A child's outputs come back as the node's output; the child is a run with
      `trigger: subworkflow` and `parent_run_id` set.
- [x] A failed or cancelled child fails the node and runs `on_failure`; cancelling
      the parent, or the parent timing out, cancels the child.
- [x] Missing workflow, bad version or bad input fail permanently without creating a run.
- [x] A self-calling workflow stops at the nesting limit; the failure travels up.
- [x] A retried or redelivered start reuses the child run (no duplicate children).
- [x] 30 concurrent parents with near-instant children all complete; the sweeper
      completes a node whose notification was lost.
- [x] A foreach over a `workflow` node starts one child per element.

### 3.1 Product milestones (M10–M19)

Each milestone keeps the engine rules: executors never import `store`, every
query takes `tenant_id`, and credentials never reach `node_executions.input`,
logs or events.

### M10 — Entry points, API surface, scaling (3–4 days)

Build: `cmd/api` and `cmd/worker` (run both for local development); `WORKER_ID` defaults to hostname plus a random suffix so
`docker compose up --scale worker=N` works; worker `/metrics` and `/healthz` on
`WORKER_METRICS_ADDRESS`. API: RFC 7807 errors; `POST /be/v1/workflows` (YAML or
JSON, `?dry_run`), `GET /be/v1/workflows[/{name}[/versions/{n}]]`, archive;
`POST /be/v1/runs`, `GET /be/v1/runs`, `GET /be/v1/runs/{id}`, `/nodes`, `/events`
(polling and SSE), `/cancel`; `POST /be/v1/signals`; `GET /be/v1/executors`;
`GET /metrics`.

**Control:** `WORKER_CONCURRENCY`, `WORKER_BATCH_SIZE` and the replica count set
throughput. Workers can be drained with `SIGTERM` (leases are released). Separate
worker pools by node type (for example, agent-only workers) are a later option
through a `WORKER_NODE_TYPES` filter on `ClaimReady`.

**Acceptance**
- [x] API and worker run as separate processes against one database; two worker
      replicas share load with no duplicate execution.
- [x] `SIGTERM` on a worker mid-run hands its nodes to another replica in under a second.
- [x] Every endpoint is tenant-scoped; one tenant can never read another tenant's runs.

### M11 — Identity, organizations, plans (4–5 days)

Tables: `users`, `organizations`, `memberships(role)`, `invites`, `sessions`,
`api_keys`, `plans`, `organization_usage` (runs per month). Password hashing with
bcrypt; session and API-key tokens stored as SHA-256 hashes; first platform admin
bootstrapped from `BOOTSTRAP_ADMIN_EMAIL` and `BOOTSTRAP_ADMIN_PASSWORD` when no
users exist. Invite emails are sent via platform SMTP when configured; otherwise
the UI shows a copyable link.

**Acceptance**
- [x] Invite → accept → login → switch organization works end to end.
- [x] Role checks on every mutating endpoint; viewers cannot publish or run.
- [x] Free plan: the second workflow, a sixth step, and the 21st run in a
      month are rejected with `402` and a clear message.

**Built (2026-10-11).** Migration `0010_identity.sql` (users, organizations,
memberships, invites, sessions, api_keys, plans, organization_usage) and
`internal/services/identity`, whose `Authenticator` replaces the development
header outside production (`auth.Chain`). `identity.Plans` implements both
`workflows.Plans` and `runs.Allowance`; the run count is an atomic
`insert ... on conflict ... where runs < limit`. Login does not reveal which emails
exist and locks an account after five failures. Invite emails wait for the email
integration (M14): the API returns the link. API: `04 §4b`.

### M12 — Vault (2 days)

`secrets(org, name, ciphertext, nonce, key_version)`; AES-256-GCM with
`VAULT_MASTER_KEY` (32 bytes, base64) and AAD of organization plus name.
Values are write-only via the API (list returns names only). The worker's
`SecretSource` decrypts per run; expressions use `${secrets.NAME}`.

**Acceptance**
- [x] A secret used in an http header reaches the upstream but the stored node
      input shows `***`.
- [x] Rotating `VAULT_MASTER_KEY` via key versions re-encrypts without downtime.

**Built (2026-10-11).** `pkg/vault` (AES-256-GCM keyring with versions and additional
authenticated data), migration `0011_secrets.sql`, `internal/services/vault` (put,
list, delete, per-run decryption as the engine's `SecretSource`, and online
rotation with compare-and-set row updates), `/be/v1/secrets` and the rotation admin
routes (`04 §4c`). Beyond the plan, the dispatcher also scrubs secret values from a
node's stored output and error and fails a missing secret by name. Rotation has an
operating rule: deploy the new key to every process before running it.

### M13 — LLM providers and agents (1–1.5 weeks)

- `provider_connections(org, provider, name, base_url, encrypted key)`, with
  `POST /be/v1/providers/test` that lists models before saving (4.5).
- `agents(org, name, provider_connection, model, system_prompt, input_schema,
  output_schema, tools, max_iterations, temperature, max_tokens)` (4.1–4.3).
- `agent` node type: `config: {agent: <name>, input: {...}}` (4.4); the tool loop
  runs inside the executor (02 §4.6); output is validated against
  `output_schema`; metrics record tokens, cost, iterations and model.
- Tools available to agents: `http_request`, connector tools (M14), database tools
  (M15) and MCP tools (M15).

**Acceptance**
- [x] Each of Anthropic, OpenAI, OpenAI-compatible and Gemini passes the connection
      test and runs a structured-output agent in a workflow.
- [x] `max_iterations` is enforced; schema-violating output is a permanent failure;
      429 is retryable and honours Retry-After.

**Built (2026-10-11).** `pkg/llm` gained OpenAI (also the client for any compatible
server) and Gemini, each with a `ListModels` for the connection test; migration
`0012_providers_agents.sql`; `internal/services/providers` (encrypted keys, test
before save, a client per named connection, every request through the egress guard)
and `internal/services/agents` (definitions, validation, test runs); `/be/v1/providers`
and `/be/v1/agents` (`04 §4d`). The `agent` node runs a stored agent
(`agent: <name>, input: {...}`) as well as the inline form, and a fail-closed
`http_request` tool joins `current_time`. Connector, database and MCP tools arrive
with M14 and M15. Acceptance is checked against local fake servers for all four
provider kinds; a manual check against real accounts has not been done. Prices for
non-Anthropic models are not built in: set `LLM_PRICING_JSON`, otherwise their
`cost_usd` is 0 and `cost_known` is false.

**A wiring bug found along the way.** `app.Build` was missing the agent, sub-workflow
and vault wiring from M7, M9 and M12: the integration tests assemble their own
harness, so they passed while the real binaries lacked those node types and routes.
It is fixed, and `TestAppWiringEndToEnd` now assembles the application through
`app.Build`, runs a real worker and drives a pipeline across the vault, a stored
agent on a tenant connection, a platform-provider agent and a sub-workflow.

### M14 — Email and app integrations (1.5 weeks)

- `connections(org, kind, name, config, encrypted credentials, oauth tokens)` with
  OAuth start/callback, token refresh, and a test action per kind.
- Node types: `email.send` (SMTP or Resend; credentials from a connection *or*
  inline in config via `${secrets.*}`, 5), `gmail.send`, `gdrive.upload`,
  `dropbox.upload`, `onedrive.upload`, `db.query` and `db.insert` for
  PostgreSQL and MySQL connections (6).
- Executors receive credentials through a `Connections` interface defined in
  `executor`, never by importing `store`.

**Acceptance**
- [ ] Each integration has a recorded-response test and a manual end-to-end check
      against a real account.
- [x] Database connectors respect egress rules and a per-query timeout and row cap.

**Progress (2026-10-11).** Slice 3 adds `pkg/dbx` (PostgreSQL through pgx, MySQL through
go-sql-driver; read-only checks, row and size caps, statement timeouts) and the
`db.query` and `db.insert` nodes (`02 §4.13`), tested through the real wiring against
PostgreSQL and MySQL 8 (CI runs a MySQL service; locally set `MYSQL_URL`). Slice 2 adds the OAuth sign-in (state with PKCE, a public
callback, lazy refresh with compare-and-set; `0014_oauth_states.sql`) and the
`gmail.send`, `gdrive.upload`, `dropbox.upload` and `onedrive.upload` nodes
(`internal/core/executor/connectors`, `02 §4.12`). They are tested against fake Google,
Dropbox and Microsoft servers; **a manual end-to-end check against real accounts has not
been done** and needs OAuth apps registered with each provider. Slice 1 is built: the connections framework
(`0013_connections.sql`, `internal/services/connections`, `/be/v1/connections` and
`/be/v1/integrations`, `04 §4e`), `executor.Connections` and `Request.Redact`, and
`email.send` over SMTP and Resend (`pkg/mailer` builds RFC 5322 messages with header
injection protection and speaks SMTP through the egress guard). Vault rotation now
re-seals provider keys and connection credentials as well as vault secrets.

### M15 — Data-exploration agents and MCP connectors (1 week)

- Data-exploration agent: an agent bound to a database connection with read-only
  tools `list_tables`, `describe_table` and `run_query` (read-only transaction,
  statement timeout, row cap) (7).
- MCP servers use the existing organization-scoped encrypted `connections` store
  with kind `mcp` (URL, transport, optional bearer token); the MCP client supports
  initialize, `tools/list` and `tools/call` over streamable HTTP or SSE;
  agents list MCP servers in `tools`, and an `mcp.call` node type calls one
  tool directly (8).

**Acceptance**
- [x] An exploration agent answers a question by querying a sample database and
      cannot write to it (real PostgreSQL and MySQL, scripted LLM).
- [x] An agent calls a tool on a public MCP test server (DeepWiki live smoke test,
      2026-10-11, scripted LLM; opt-in so external uptime does not gate CI).

Implemented: tenant-bound `database_connection`, the three read-only exploration
tools, encrypted MCP connection kind with test-before-save, the official Go MCP
SDK behind `pkg/mcpclient`, both HTTP transports, dynamic agent tool discovery,
and `mcp.call`. Integration coverage checks both databases, write refusal,
parameter injection and row caps, tenant isolation, both transports, credentials
and blocked egress. Client tests cover pagination, SSE responses, authentication,
redirect/origin protection, cancellation and response bounds. No real LLM account
was used. To repeat the live smoke test:

```sh
M15_PUBLIC_MCP_URL=https://mcp.deepwiki.com/mcp \
  go test -count=1 -tags=integration ./test/integration -run '^TestMCPPublicAgent$'
```

**Shared connector expansion (complete locally).** One action catalog owns
workflow executors, agent tools and frontend schemas. SQL connection conversion
and read/query execution were deduplicated; PostgreSQL, MySQL and MongoDB appear
in the same Connectors section as other services. Added Google Docs, Sheets,
Calendar, Gmail search/read, Slack and Notion actions. Migration
`0017_connector_actions.sql` extends the existing connection kind constraint;
credentials continue to use the existing vault, tenant lookup, testing and
rotation paths. Google kinds use the existing OAuth state/refresh implementation.
Slack/Notion use integration tokens with test-before-save. MongoDB uses the
maintained Go driver, certificate verification by default, guarded dialing,
bounded reads and no server-side JavaScript. Agent permissions select individual
read/write actions, so analytics agents can be granted read actions alone.
Acceptance covers real PostgreSQL/MySQL/MongoDB, scripted Google/Slack/Notion and
LLM APIs, and the browser's connector-to-agent visual workflow. **Real Google,
Slack, Notion, MongoDB Atlas and LLM account checks remain pending.** Set
`MONGO_URL` to include local MongoDB acceptance; CI provisions MongoDB 8.

### M16 — Webhook triggers (complete locally)

`triggers(org, workflow, kind, token_hash, secret, enabled)`. Calling
`POST /hooks/{token}` starts a run with the JSON body as input (or
`{body, headers, query}` when `input_mode: envelope`); an optional HMAC signature
check; the `Idempotency-Key` header is honoured (9). Plan run limits apply.

### M17 — Workflow assistant (implemented; real-provider acceptance pending)

`POST /be/v1/assistant/workflows`: requirements text in, YAML out. The prompt
includes the spec rules, `GET /be/v1/executors` descriptions, and the organization's
agents, connections and secret names (never values). The draft is compiled with
`?dry_run`, and the diagnostics are fed back to the model for up to three repair
rounds. It runs on a platform key (`ASSISTANT_PROVIDER`, `ASSISTANT_API_KEY`) or
the organization's own provider connection.

**Acceptance**
- [x] Ten reference prompts exercise compilation and bounded repair through a scripted provider.
- [ ] Repeat the reference prompts against a real model and assess semantic quality.

### M18 — Web UI (complete locally)

Pages: login and accept invite; workflows (list, visual builder with draggable steps, editable dependency edges,
saved connector/agent selection, action forms, YAML editor with live validation,
DAG graph, versions, run button with an input form); assistant
panel; runs (filters, detail with node timeline, inputs/outputs, live events
over SSE, cancel); agents (create, test, use, explicit saved connector action permissions); Connectors and integrations
(OAuth connect buttons, test); vault; webhooks; members and invites; API keys;
plan and usage.

**Acceptance**
- [x] Chromium acceptance: a new user can go from invite to a successful run with an agent node
      without touching YAML by hand.

### M19 — Production (packaging complete; hosting decision pending)

Multi-stage Dockerfile (UI build, Go build, distroless); `deploy/compose.yaml`
with Caddy (automatic HTTPS), `migrate` one-shot, `api`, `worker`
(scalable), PostgreSQL 16 with tuned settings, nightly `pg_dump` to S3-compatible
storage with retention, and Grafana Alloy shipping metrics and logs to Grafana
Cloud; dashboards and the two alerts from §6; a GitHub Actions workflow that
builds and pushes images to GHCR and deploys over SSH; runbook updates.

**Acceptance**
- [ ] Fresh host to a working HTTPS deployment with one documented command.
- [x] Local backup/restore tooling tested against PostgreSQL, including checksum and decryption.
- [ ] Restore last night's production backup after a host/bucket have been selected.
- [x] Local API/worker replacement during a running node causes no failed runs.
- [ ] Verify rolling deployment on the chosen production host.

Local implementation and verification are documented in [the runbook](06-runbook.md).
No provider/host has been selected and no remote deployment has been performed.
Real OAuth, SMTP, LLM, S3, and Grafana Cloud account validation remains pending.

### M20 — Custom code nodes (scoped, not started)

A `code` node that runs Python, TypeScript or Go inside a sandbox outside the
worker process. The full design, threat model, limits and sub-milestones C1–C5 are in
[08 — Custom code in workflows](08-code-nodes.md). The sandbox backend decision
(hosted microVM API, own runner, or another option) must be made before C3.

**Acceptance:** see C1–C5 in the design document; the headline gate is that a
second tenant's job cannot observe the first one's files, environment or network,
and that timeouts, memory bombs, fork bombs and output floods are contained.

### Later

Stripe billing and self sign-up; scheduled (cron) triggers; `human_task` (M8);
`foreach` and sub-workflows (M9); OpenTelemetry tracing; per-tenant egress
allowlists; SSO.

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

**Manual interventions**, as platform-admin API calls (`04 §5.1`) so nobody
hand-writes UPDATEs against production at 3am:

```
POST /be/v1/runs/{id}/cancel                         -- tenant API, editor role
POST /be/v1/admin/node-executions/{id}/retry         -- insert attempt+1 as ready
POST /be/v1/admin/node-executions/{id}/skip          -- force-skip a wedged node, then advance
POST /be/v1/signals                                  -- tenant API, editor role
POST /be/v1/admin/workers/{worker_id}/release-leases -- after a hard crash
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
5. Wire `POST /be/v1/workflows` with `?dry_run`. You can now lint YAML in CI.
6. `executor` interface, registry, `noop`, `transform`.
7. The claim loop and dispatcher, single worker, no retries. Run a linear
   workflow end to end. **This is the first real milestone — everything after it
   is filling in.**
8. The resolver: successors, joins, run completion. Diamond test with 8 workers.
9. Lease, heartbeat, fencing, sweeper pass 1. Crash test.
10. `http` executor, retries, idempotency keys.
11. `branch` + skip cascade.
12. `sleep`, `wait_event`, signals, sweeper passes 2–4.
13. Events, SSE, cancellation, metrics, admin API.
14. Ship. Then `agent`.

**Product order (from 2026-10-10):** finish M2–M5 verification → M10 (entry
points and API) → M11 (identity and plans) → M12 (vault) → M13 (agents) → M16
(webhooks) → M18 (UI shell, built alongside each later milestone) → M14
(integrations) → M15 (exploration agents and MCP) → M17 (assistant) → M19
(production) → M20 (custom code, after the backend decision). Webhooks come early because they are small and make the engine
usable from other systems before the UI is complete.

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

- [x] All three example workflows in `02 §5` run end to end against real HTTP
      upstreams, including the timeout and compensation paths (delivery, agentic and healthcare
      each have acceptance tests, and the lint compiles all three).
- [x] Invariants I1–I7 asserted by `checkInvariants` in every engine test.
- [x] 8-worker diamond test, 200 iterations, zero duplicates, with `-race`.
- [x] Kill -9 chaos test: every run terminal, no duplicate `succeeded` rows.
- [x] Rolling restart mid-run costs <1s, not a lease TTL.
- [x] A 30-day `sleep` holds one row, zero goroutines (asserted, not assumed).
- [x] `?dry_run=true` wired into CI as a workflow lint.
- [x] Runbook §7 written, and every admin endpoint in it actually exists.

---

## 11. Hosting options (2026 prices; verify before committing)

| Option | Shape | ~Monthly | Notes |
|---|---|---|---|
| A. Existing droplet | Add Caddy, API, worker and Postgres containers | $0 extra | Fine for a private beta; competes with the other service for CPU and memory |
| **B. New small droplet (recommended start)** | $12 droplet (2 GB), Compose stack, self-hosted Postgres, nightly `pg_dump` to R2 or Spaces | ~$12 | Isolated and cheap; add worker replicas or a worker-only droplet to scale |
| C. Droplet + DO Managed Postgres | $6–12 droplet + $15 managed PG | ~$21–27 | Managed backups and failover; move here when there are paying customers |
| D. Droplet + Neon | Serverless Postgres | ~$19+ | Polling workers keep compute awake 24/7; the free tier runs out mid-month |
| E. Fly.io + any Postgres | API ~$2, worker ~$3 | ~$5 + PG | Good app tier; Postgres is billed separately |
| F. Hetzner CX23 | 2 vCPU / 4 GB, same Compose stack | ~€4–6 | Best value; adds a second provider |

Observability for every option: Grafana Cloud free tier (10k metric series,
50 GB logs and traces per month, 14-day retention).
