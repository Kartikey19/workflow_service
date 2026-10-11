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

Filters: `workflow`, `status`, `correlation_id`, `parent_run_id` (the child runs
of a sub-workflow node), `created_after`, `created_before`. Keyset paginated on `(created_at, id)` descending.

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
- **400** if `key` starts with `task:` or `subworkflow:`. Those wait keys belong to
  the engine; complete a human task through `/v1/tasks` (§4.2), which applies the
  claim and assignee rules, and sub-workflows complete themselves.

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

## 4b. Identity, organizations and plans (M11)

An **organization** is the tenant. Callers authenticate with a **session cookie**
(`wf_session`: HttpOnly, SameSite=Lax, Secure in production; a state-changing
request whose `Origin` names another site is rejected) or an **API key**
(`Authorization: Bearer wfk_...`). Roles, least to most privileged: `viewer`,
`editor` (publish, run), `admin` (members, invites, keys), `owner`. Nobody can
grant a role above their own, an admin cannot touch an owner, and an organization
always keeps at least one owner. Sign-up is invite-only.

| Endpoint | Auth | Purpose |
|---|---|---|
| `POST /v1/auth/login` | none | `{email, password}` → sets the session cookie; wrong credentials are indistinguishable from unknown emails, and 5 failures in a row lock the account for 15 minutes (`429 account_locked`) |
| `GET /v1/auth/invite?token=` | none | preview an invite: email, role, organization, whether the email already has an account |
| `POST /v1/auth/accept-invite` | none | `{token, password, name}`; creates the account (or, if the email has one, requires its password), joins the organization — created now for a platform invite — and signs in. `410` if the invite is unknown, used or expired |
| `POST /v1/auth/logout` | session | ends the session |
| `GET /v1/auth/me` | session | the user, the current organization and every organization they can switch to |
| `POST /v1/auth/switch-org` | session | `{organization_id}` |
| `GET /v1/members` | viewer | members of the current organization |
| `PATCH /v1/members/{user_id}` | admin | `{role}` |
| `DELETE /v1/members/{user_id}` | viewer | remove a member (admin) or leave (yourself); their sessions stop working at once |
| `POST /v1/invites` | admin | `{email, role}` → the response carries the invite `token` and `accept_url` **once**; `GET /v1/invites` lists pending ones, `DELETE /v1/invites/{id}` revokes |
| `POST /v1/api-keys` | admin | `{name, role}` (viewer, editor or admin; at most your own role) → the `token` is shown once; `GET` lists metadata, `DELETE /v1/api-keys/{id}` revokes at once |
| `GET /v1/usage` | viewer | the plan and current usage |
| `GET /v1/plans` | viewer | every plan and its limits |
| `POST /v1/admin/invites` | platform admin | `{email, organization_name}` — invites someone to a new organization they will own |
| `GET /v1/admin/organizations` | platform admin | organizations with plan, members and runs this month |
| `PUT /v1/admin/organizations/{id}/plan` | platform admin | `{plan}`; billing is manual until self sign-up opens |

**Plan limits** are rows in `plans` (a null limit is unlimited) and are enforced at
publish time (workflows, steps per workflow) and at run creation (runs per calendar
month, UTC). Breaching one is `402 plan_limit` with a message naming the limit.
The monthly count is atomic, so concurrent starts cannot overshoot it, and an
idempotent replay or a sub-workflow child run is not charged.

| Plan | Workflows | Steps per workflow | Runs per month |
|---|---|---|---|
| Free | 1 | 5 | 20 |
| Pro | 50 | 50 | 10 000 |
| Team | unlimited | 200 | 100 000 |

The first platform administrator is created from `BOOTSTRAP_ADMIN_EMAIL` and
`BOOTSTRAP_ADMIN_PASSWORD` when no user exists, with an organization of their own.
Invite links are returned in the response; sending them by email arrives with the
email integration (M14). Passwords are bcrypt hashes of 10–72 characters; session,
invite and API-key tokens are random and stored only as SHA-256 hashes.

---

## 4c. Secrets (M12)

A vault of per-organization credentials that workflows reference as
`${secrets.NAME}`, for example `Authorization: "Bearer ${secrets.API_KEY}"`.
Values are encrypted with AES-256-GCM before they are stored, bound to their
organization and name, and are **write-only**: nothing returns a value, and the
list shows names only.

| Endpoint | Role | Purpose |
|---|---|---|
| `GET /v1/secrets` | editor | names and metadata, plus the `${secrets.NAME}` reference to paste |
| `PUT /v1/secrets/{name}` | admin | `{"value": "..."}` creates or replaces; takes effect on the next node |
| `DELETE /v1/secrets/{name}` | admin | workflows that still use it fail with `E_SECRET_MISSING` |
| `GET /v1/admin/vault` | platform admin | how many secrets use each master-key version |
| `POST /v1/admin/vault/rotate` | platform admin | re-seal every secret under the current master key |

Names are `[A-Z][A-Z0-9_]{0,63}`; values are 8 bytes to 64 KiB (short values could
not be scrubbed reliably). Without `VAULT_MASTER_KEY` the vault is off and these
routes answer `501 vault_not_configured`.

**What a workflow run does with a secret.** The worker decrypts the organization's
secrets only when a node's config mentions `secrets`, hands the real value to the
executor, and stores the node input with the value replaced by `***`. The value is
also scrubbed from the node's stored output and error, so an upstream that echoes a
header back, or a transport error that quotes a URL, cannot leak it. Secrets never
reach logs or run events. Referring to a secret that does not exist fails the node
once, permanently, with `E_SECRET_MISSING` and the secret's name.

**Rotating the master key** needs no downtime: (1) generate a new 32-byte key;
(2) deploy **every** process (API and workers) with it as `VAULT_MASTER_KEY`, a
higher `VAULT_KEY_VERSION`, and the previous key in `VAULT_OLD_KEYS=<version>:<key>`;
(3) only then `POST /v1/admin/vault/rotate` (repeat until `failed` is 0; it is
safe while serving, and each row is replaced compare-and-set on its old version);
(4) when `GET /v1/admin/vault` shows nothing under the old version, remove the old
key. Step 2 must finish first: a process that does not yet hold the new key cannot
read secrets that were already re-sealed.

---

## 4d. Providers and agents (M13)

**Provider connections** hold an organization's API key for one LLM provider:
`anthropic`, `openai`, `gemini`, or `openai_compatible` (Groq, Together, OpenRouter,
Ollama, vLLM and others, through a `base_url` that includes the version path, e.g.
`https://api.groq.com/openai/v1`). Keys are sealed with the vault master key and are
**write-only**: only a hint (the last four characters) is ever shown. Every request
goes through the egress guard, since a base URL is chosen by the tenant.

| Endpoint | Role | Purpose |
|---|---|---|
| `POST /v1/providers/test` | admin | `{provider, base_url?, api_key}` → `{ok, models}` by listing the models the key can use; saves nothing. A bad key is `422 provider_test_failed` and the message never echoes the key |
| `POST /v1/providers` | admin | `{name, provider, base_url?, api_key, skip_test?}`; **tests first and saves only if the test passes**; returns the connection and the models found |
| `GET /v1/providers` | editor | connections, with how many agents use each |
| `POST /v1/providers/{id}/test` | admin | re-test a saved connection |
| `DELETE /v1/providers/{id}` | admin | `409 connection_in_use` while an agent uses it |

**Agents** are named, reusable definitions: which connection and model, the system
prompt, an `input_schema` and `output_schema` (the JSON Schema subset used by
`wait_event`), `tools`, `allowed_hosts`, `max_iterations` (required, 1 to 50),
`max_tokens` and `temperature`. Leave `connection` empty to use the platform's own
provider (`ANTHROPIC_API_KEY`). Saving validates everything; editing bumps `revision`,
which each run's node metrics record.

| Endpoint | Role | Purpose |
|---|---|---|
| `GET /v1/agents`, `GET /v1/agents/{name}` | viewer | read |
| `POST /v1/agents`, `PUT /v1/agents/{name}`, `DELETE /v1/agents/{name}` | editor | manage (the name in the path is the identity) |
| `POST /v1/agents/{name}/test` | editor | run once with `{input?, prompt?}`; spends real tokens, creates no run; a failing agent is a 200 with `ok: false` and the reason |
| `GET /v1/agent-tools` | viewer | tools an agent can be given |

Use an agent in a workflow with `type: agent, config: {agent: <name>, input: {...}}`
(`02 §4.6`). The input is checked against `input_schema` and passed to the model as
JSON; the reply is validated against `output_schema`, and a violation is a permanent
failure. A missing agent or connection fails the node by name (`E_AGENT_NOT_FOUND`,
`E_PROVIDER_NOT_FOUND`).

**Tools.** `current_time`, and `http_request`, which is fail-closed: it works only
for hosts in the agent's own `allowed_hosts` (exact names or `*.example.com`), goes
through the egress guard, never follows redirects, and returns a truncated body.
Without `allowed_hosts` every request is refused, and the model is told why.

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
