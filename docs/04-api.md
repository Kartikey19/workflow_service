# 04 — HTTP API

Small surface: workflows, runs, signals, tasks and the executor catalogue, plus a
small admin surface (§5.1). Everything is JSON except workflow
publishing, which takes YAML.

Base path `/v1`. Auth is out of scope for this document — assume middleware
resolves a `tenant_id` and puts it on the request context. Every query in the
store layer takes `tenant_id` explicitly; there is no ambient default.

---

## 1. Conventions

**Errors** are RFC 7807 `application/problem+json`:

```json
{
  "type":   "https://errors.workflow.internal/spec_invalid",
  "title":  "Workflow specification is invalid",
  "status": 400,
  "detail": "3 errors found while compiling the workflow",
  "instance": "/v1/workflows",
  "errors": [
    {"code":"E_UNKNOWN_NODE","message":"needs references unknown node \"reserv_stock\"",
     "node_id":"assign_rider","path":"nodes[1].needs[0]","line":21},
    {"code":"E_EXPR_SCOPE","message":"node \"notify\" is not a dependency of \"route\"",
     "node_id":"route","path":"nodes[3].config.cases[0].when","line":34}
  ]
}
```

Always return the **full** list. A user with three typos should see three errors.

**Time** is RFC 3339 UTC with millisecond precision. **IDs** are UUIDv7 strings.
**Durations** in responses are integer milliseconds (`duration_ms`), never
strings — durations in *requests* (YAML) are Go duration strings (`45m`, `720h`).

**Pagination** is keyset, never offset:
`?limit=50&after=<opaque_cursor>` → `{"items":[...], "next":"<cursor>"}`.
Offset pagination over `runs` will be your first slow query in production.

---

## 2. Workflows

### 2.1 `POST /v1/workflows` — publish a version

```
Content-Type: application/x-yaml
```

Body is the YAML document (`02-workflow-spec.md §1`). Compiles, validates, and
creates version `max+1`. See `02 §8` for the exact semantics.

**201 Created** — new version:

```json
{
  "workflow": {"id":"018f...","name":"order-fulfilment"},
  "version": 7,
  "version_id": "018f...",
  "spec_hash": "sha256:9c1a...",
  "unchanged": false,
  "warnings": [
    {"code":"W_NO_TIMEOUT","message":"http node \"notify_customer\" has no timeout",
     "node_id":"notify_customer"}
  ]
}
```

**200 OK** — the YAML compiles to a spec identical to the current latest;
`"unchanged": true` and the existing version is returned. This is what stops a
deploy-on-every-commit pipeline from minting thousands of identical versions.

**400** — one or more `error` diagnostics. Nothing is written.

Add `?dry_run=true` to compile and validate without persisting. Wire this into
CI as a lint step — it is the highest-value 20 lines in the API, because it moves
workflow bugs from runtime to pull-request time.

### 2.2 `GET /v1/workflows` / `GET /v1/workflows/{name}`

List (keyset paginated) and fetch. The detail response includes the latest
version's compiled spec and a version summary list — enough to render a graph.

### 2.3 `GET /v1/workflows/{name}/versions/{version}`

Returns `yaml_source`, `spec`, `spec_hash`, `created_at`, `created_by`.
`?format=yaml` returns the raw source with `Content-Type: application/x-yaml`,
so `curl ... > workflow.yaml` round-trips exactly what was deployed.

### 2.4 `DELETE /v1/workflows/{name}` — archive

Sets `archived_at`. New runs are rejected with **409**; existing runs continue.
Versions are never deleted (I7).

---

## 3. Runs

### 3.1 `POST /v1/runs` — start a run

```json
{
  "workflow": "order-fulfilment",
  "version": 7,
  "input": {"order_id": "ORD-88213"},
  "correlation_id": "ORD-88213"
}
```

- `version` optional; omit to pin to the latest at creation time. **The resolved
  version is stored on the run and never changes** (P2).
- `correlation_id` optional; if the spec declares `correlation:`, that expression
  wins and this field is ignored.
- `input` validated against `spec.inputs` before anything is written
  (`02 §7`). Defaults are applied and the *resolved* input is persisted.

**Header `Idempotency-Key: <caller-supplied>`** — strongly recommended, and
required for any webhook-triggered workflow. Maps to `runs.idempotency_key`.

**201 Created:**

```json
{
  "id": "018f...", "status": "running", "workflow": "order-fulfilment",
  "version": 7, "correlation_id": "ORD-88213",
  "created_at": "2026-08-25T09:14:02.113Z"
}
```

**200 OK** if the idempotency key already exists — returns the *existing* run
unchanged. This is the correct behaviour and it is worth being pedantic about:
a courier webhook that fires twice must observe one run, not two, and must not
receive an error it will interpret as "retry harder".

**409** if the two requests share an idempotency key but differ in body — that is
a caller bug and must be loud.

**400** on input validation failure, with per-field detail.

Implementation: insert the run row, insert the root nodes as `ready`, emit
`run.created` + `run.started`, `pg_notify('wf_ready')` — one transaction, and the
`ON CONFLICT (tenant_id, idempotency_key)` path returns the existing row.

### 3.2 `GET /v1/runs/{id}`

```json
{
  "id": "018f...", "status": "waiting",
  "workflow": "order-fulfilment", "version": 7,
  "correlation_id": "ORD-88213",
  "input": {"order_id":"ORD-88213"},
  "output": null,
  "error": null,
  "created_at": "...", "started_at": "...", "ended_at": null,
  "deadline_at": "2026-08-26T09:14:02.113Z",
  "progress": {"total": 5, "succeeded": 3, "failed": 0, "skipped": 0,
               "running": 0, "waiting": 1, "ready": 0}
}
```

`progress.total` is `len(spec.nodes)` — an upper bound, since branches skip nodes.
Good enough for a progress bar and free to compute.

### 3.3 `GET /v1/runs` — list

Filters: `workflow`, `status`, `correlation_id`, `created_after`,
`created_before`. Keyset paginated on `(created_at, id)` descending.

`?correlation_id=ORD-88213` is the query that gets used most. It is backed by
`runs_correlation_ix`.

### 3.4 `GET /v1/runs/{id}/nodes`

Every node execution, ordered by `created_at`. This is the debugging endpoint:

```json
{"items": [
  {"id":"018f...","node_id":"reserve_stock","node_type":"http","attempt":1,
   "status":"failed","error":{"code":"upstream_5xx","retryable":true,"http_status":503},
   "started_at":"...","ended_at":"...","metrics":{"duration_ms":2100}},
  {"id":"018f...","node_id":"reserve_stock","node_type":"http","attempt":2,
   "status":"succeeded","output":{"status":200,"body":{"reservation_id":"RSV-771"}},
   "metrics":{"duration_ms":290}}
]}
```

Both attempts are returned. Do not collapse them — "it failed twice with 503 then
succeeded" is exactly the information someone is looking for when they call this.

`?include=input` adds the resolved config that was sent (large; opt-in).

### 3.5 `GET /v1/runs/{id}/events`

```
GET /v1/runs/{id}/events?after=12&limit=100
```

Returns `run_events` in `seq` order. Because `seq` is gap-free (I6), a consumer
polling `?after=<last_seen>` provably misses nothing.

With `Accept: text/event-stream`, stream via SSE: replay everything after `after`,
then follow. Implement follow by `LISTEN wf_events_<run_id>` with a slow poll
fallback — same rule as the claim loop: **NOTIFY is an optimisation, the poll is
the correctness guarantee.**

### 3.6 `POST /v1/runs/{id}/cancel`

```json
{"reason": "customer cancelled the order"}
```

**202 Accepted.** Semantics in `03-engine.md §10`: ready and waiting nodes go to
`cancelled` immediately; already-running nodes are cancelled cooperatively via
the heartbeat, so the run can take up to one heartbeat interval to fully settle.
Say so in the response.

**409** if the run is already terminal.

### 3.7 `POST /v1/runs/{id}/retry` (M6, optional)

Creates a **new run** from the same version and input, with
`parent_run_id` set. Deliberately not a resume: resuming a failed run means
reasoning about which side effects already happened, and the honest answer is
"only the workflow author knows". A new run with the same idempotency-key
derivation gives you the same protection with none of the ambiguity.

---

## 4. Signals

### 4.1 `POST /v1/signals`

```json
{"key": "order.picked_up:ORD-88213", "payload": {"rider":"R-42","at":"2026-08-25T09:26:00Z"}}
```

Looks up the single live waiter for `(tenant_id, key)`, fulfils it, completes the
waiting node with `payload` as its output, and advances the DAG — one transaction
(`01 §6.7`).

- **200** `{"run_id":"018f...","node_id":"await_pickup"}`
- **404** `no_waiter` — no live wait for that key. See the early-signal discussion
  in `03-engine.md §7.3`; **v1 rejects, and callers must retry.** Document this
  prominently, because it is the thing integrators will trip on.
- **422** if `payload` fails the wait's declared `schema`.

An `Idempotency-Key` header is honoured: a duplicate signal for an
already-fulfilled wait returns **200** with the original result rather than 404.
Without this, a courier's at-least-once webhook produces spurious 404s and
someone will eventually "fix" it by removing the retry.

### 4.2 Tasks (M8)

The worklist for `human_task` nodes. A task's state is derived from the wait it
parks on, so completing one is delivering that wait's signal and an SLA timeout
is the wait's ordinary timeout path.

| Endpoint | Role | Purpose |
|---|---|---|
| `GET /v1/tasks` | viewer | the worklist, newest first, keyset paginated (`limit`, `after`); filters `queue`, `status`, `assignee`, and `mine=true` for tasks the caller claimed |
| `GET /v1/tasks/{id}` | viewer | one task: queue, form, assignee, payload, `due_at`, claim, status |
| `POST /v1/tasks/{id}/claim` | editor | reserve an open task; claiming your own task again is a no-op |
| `POST /v1/tasks/{id}/release` | editor | give a claimed task back; only the claimer or an admin |
| `POST /v1/tasks/{id}/complete` | editor | body `{"payload": ...}`; the payload becomes the node's output and the run advances |

`status` is `open`, `claimed`, `completed`, `expired` (the SLA passed) or
`cancelled` (the run ended). A task with an `assignee` can only be claimed or
completed by that person or an organization admin; an admin may also complete a
task someone else claimed. Errors: `409 task_claimed`, `409 task_closed`
(completed, expired or cancelled), `403` for a non-assignee, `404` for another
organization's task.

---

## 5. Operations

| Endpoint | Purpose |
|---|---|
| `GET /healthz` | process liveness; no DB call |
| `GET /readyz` | DB ping + migration version check |
| `GET /metrics` | Prometheus |
| `GET /v1/executors` | registered node types + their config JSON Schema |

`GET /v1/executors` is what lets an editor or docs page stay in sync with the
binary. It costs nothing — you already have the registry.

### 5.1 Admin API (operators only)

Manual interventions for stuck work, so nobody hand-writes UPDATEs in production.
Every route requires a principal with the platform-admin flag
(`auth.Principal.PlatformAdmin`); tenant roles, including `owner`, are not enough.
Responses use the same problem+json errors as the rest of the API. Node
interventions write an `admin.node_retried` or `admin.node_skipped` row to the
run's `run_events` recording the actor; releasing leases is logged with the
actor and counted in `wf_sweeper_actions_total{action="lease_released"}`. With
the development authenticator, `X-Platform-Admin: true` grants the flag.

| Endpoint | Effect |
|---|---|
| `POST /v1/admin/node-executions/{id}/retry` | cancel a live attempt (fencing its worker) and insert attempt+1 as `ready`; a failed attempt in a live run is retried as is. `409` for a finished run or a node that already has a later attempt |
| `POST /v1/admin/node-executions/{id}/skip` | force-skip a ready, running or waiting node, then advance the run with normal skip semantics. `409` otherwise |
| `POST /v1/admin/workers/{worker_id}/release-leases` | release a crashed worker's running nodes so rows are reclaimed now rather than at `LeaseTTL`; returns `{released: n}` |

These go through the same fenced SQL as the engine: a zero-row write means the
node moved on and the call returns `409`, never a forced update. Run
cancellation and signals stay on the tenant API (§3.6, §4.1).

---

## 6. Handler skeleton

```go
func (h *Handlers) CreateRun(w http.ResponseWriter, r *http.Request) {
    tenant := auth.TenantFrom(r.Context())

    var req CreateRunRequest
    if err := decodeJSON(r, &req); err != nil { exception.Write(w, exception.BadRequest(err)); return }

    wf, ver, err := h.specs.Resolve(r.Context(), tenant, req.Workflow, req.Version)
    if err != nil { exception.Write(w, exception.From(err)); return }

    input, verrs := spec.ValidateInput(ver.Spec, req.Input)
    if len(verrs) > 0 { exception.Write(w, exception.InputInvalid(verrs)); return }

    run, created, err := h.runs.Create(r.Context(), runs.CreateParams{
        TenantID: tenant, Workflow: wf, Version: ver,
        Input: input, CorrelationID: req.CorrelationID,
        IdempotencyKey: r.Header.Get("Idempotency-Key"),
    })
    if err != nil { exception.Write(w, exception.From(err)); return }

    writeJSON(w, statusFor(created), runView(run))   // 201 or 200
}
```

Handlers stay this thin. All logic lives in `runs`/`spec` services so it is
testable without an HTTP layer, and so the same code is reachable from a future
gRPC entry point.

---

## 7. What is deliberately absent

- **No `PUT /workflows/{name}`.** Versions are immutable (P2). Publishing is the
  only write.
- **No "pause run".** It sounds cheap and is not: pausing mid-node means either
  killing in-flight work or holding a lease indefinitely. If you need it, model it
  as a `wait_event` node in the workflow, where the author decides where a safe
  pause point is.
- **No bulk run creation.** Callers loop. If throughput becomes a problem, add a
  batch endpoint then — not before you have the numbers.
- **No CLI.** Everything, including operator interventions, is reachable over
  HTTP; use `curl` or any HTTP client.
- **No GraphQL.**

---

**Next:** `05-implementation-plan.md` — repo layout, milestones, testing.
