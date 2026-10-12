# 04 — HTTP API

Small surface: workflows, runs, signals, tasks and the executor catalogue, plus a
small admin surface (§5.1). Everything is JSON except workflow
publishing, which takes YAML.

Base path `/be/v1`. Auth is out of scope for this document — assume middleware
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
  "instance": "/be/v1/workflows",
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

### 2.1 `POST /be/v1/workflows` — publish a version

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

### 2.2 `GET /be/v1/workflows` / `GET /be/v1/workflows/{name}`

List (keyset paginated) and fetch. The detail response includes the latest
version's compiled spec and a version summary list — enough to render a graph.

### 2.3 `GET /be/v1/workflows/{name}/versions/{version}`

Returns `yaml_source`, `spec`, `spec_hash`, `created_at`, `created_by`.
`?format=yaml` returns the raw source with `Content-Type: application/x-yaml`,
so `curl ... > workflow.yaml` round-trips exactly what was deployed.

### 2.4 `DELETE /be/v1/workflows/{name}` — archive

Sets `archived_at`. New runs are rejected with **409**; existing runs continue.
Versions are never deleted (I7).

---

## 3. Runs

### 3.1 `POST /be/v1/runs` — start a run

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

### 3.2 `GET /be/v1/runs/{id}`

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

### 3.3 `GET /be/v1/runs` — list

Filters: `workflow`, `status`, `correlation_id`, `parent_run_id` (the child runs
of a sub-workflow node), `created_after`, `created_before`. Keyset paginated on `(created_at, id)` descending.

`?correlation_id=ORD-88213` is the query that gets used most. It is backed by
`runs_correlation_ix`.

### 3.4 `GET /be/v1/runs/{id}/nodes`

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

### 3.5 `GET /be/v1/runs/{id}/events`

```
GET /be/v1/runs/{id}/events?after=12&limit=100
```

Returns `run_events` in `seq` order. Because `seq` is gap-free (I6), a consumer
polling `?after=<last_seen>` provably misses nothing.

With `Accept: text/event-stream`, stream via SSE: replay everything after `after`,
then follow. Implement follow by `LISTEN wf_events_<run_id>` with a slow poll
fallback — same rule as the claim loop: **NOTIFY is an optimisation, the poll is
the correctness guarantee.**

### 3.6 `POST /be/v1/runs/{id}/cancel`

```json
{"reason": "customer cancelled the order"}
```

**202 Accepted.** Semantics in `03-engine.md §10`: ready and waiting nodes go to
`cancelled` immediately; already-running nodes are cancelled cooperatively via
the heartbeat, so the run can take up to one heartbeat interval to fully settle.
Say so in the response.

**409** if the run is already terminal.

### 3.7 `POST /be/v1/runs/{id}/retry` (M6, optional)

Creates a **new run** from the same version and input, with
`parent_run_id` set. Deliberately not a resume: resuming a failed run means
reasoning about which side effects already happened, and the honest answer is
"only the workflow author knows". A new run with the same idempotency-key
derivation gives you the same protection with none of the ambiguity.

---

## 4. Signals

### 4.1 `POST /be/v1/signals`

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
  the engine; complete a human task through `/be/v1/tasks` (§4.2), which applies the
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
| `GET /be/v1/tasks` | viewer | the worklist, newest first, keyset paginated (`limit`, `after`); filters `queue`, `status`, `assignee`, and `mine=true` for tasks the caller claimed |
| `GET /be/v1/tasks/{id}` | viewer | one task: queue, form, assignee, payload, `due_at`, claim, status |
| `POST /be/v1/tasks/{id}/claim` | editor | reserve an open task; claiming your own task again is a no-op |
| `POST /be/v1/tasks/{id}/release` | editor | give a claimed task back; only the claimer or an admin |
| `POST /be/v1/tasks/{id}/complete` | editor | body `{"payload": ...}`; the payload becomes the node's output and the run advances |

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
| `POST /be/v1/auth/login` | none | `{email, password}` → sets the session cookie; wrong credentials are indistinguishable from unknown emails, and 5 failures in a row lock the account for 15 minutes (`429 account_locked`) |
| `GET /be/v1/auth/invite?token=` | none | preview an invite: email, role, organization, whether the email already has an account |
| `POST /be/v1/auth/accept-invite` | none | `{token, password, name}`; creates the account (or, if the email has one, requires its password), joins the organization — created now for a platform invite — and signs in. `410` if the invite is unknown, used or expired |
| `POST /be/v1/auth/logout` | session | ends the session |
| `GET /be/v1/auth/me` | session | the user, the current organization and every organization they can switch to |
| `POST /be/v1/auth/switch-org` | session | `{organization_id}` |
| `GET /be/v1/members` | viewer | members of the current organization |
| `PATCH /be/v1/members/{user_id}` | admin | `{role}` |
| `DELETE /be/v1/members/{user_id}` | viewer | remove a member (admin) or leave (yourself); their sessions stop working at once |
| `POST /be/v1/invites` | admin | `{email, role}` → the response carries the invite `token` and `accept_url` **once**; `GET /be/v1/invites` lists pending ones, `DELETE /be/v1/invites/{id}` revokes |
| `POST /be/v1/api-keys` | admin | `{name, role}` (viewer, editor or admin; at most your own role) → the `token` is shown once; `GET` lists metadata, `DELETE /be/v1/api-keys/{id}` revokes at once |
| `GET /be/v1/usage` | viewer | the plan and current usage |
| `GET /be/v1/plans` | viewer | every plan and its limits |
| `POST /be/v1/admin/invites` | platform admin | `{email, organization_name}` — invites someone to a new organization they will own |
| `GET /be/v1/admin/organizations` | platform admin | organizations with plan, members and runs this month |
| `PUT /be/v1/admin/organizations/{id}/plan` | platform admin | `{plan}`; billing is manual until self sign-up opens |

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
`${secrets.NAME}`, for example `Authorization: "Bearer ${secrets.API_KEY}"`. Related
keys can live in a **group** and are read as `${secrets.GROUP.key}`, for example
`${secrets.open_api_settings.api_key}`, `${secrets.open_api_settings.effort}` and
`${double(secrets.open_api_settings.temperature)}` (every value is a string).
Values are encrypted with AES-256-GCM before they are stored, bound to their
organization and name, and are **write-only**: nothing returns a value, and the
list shows names only.

| Endpoint | Role | Purpose |
|---|---|---|
| `GET /be/v1/secrets` | editor | names and metadata, plus the `${secrets.NAME}` reference to paste |
| `PUT /be/v1/secrets/{name}` | admin | `{"value": "..."}` creates or replaces one secret; `{"fields": {"api_key": "...", "effort": "high"}}` creates or replaces keys of the group `{name}`. Takes effect on the next node |
| `DELETE /be/v1/secrets/{name}` | admin | `{name}` removes a secret, or a whole group and its keys; `group.key` removes one key. Workflows that still use it fail with `E_SECRET_MISSING` |
| `GET /be/v1/admin/vault` | platform admin | how many secrets use each master-key version |
| `POST /be/v1/admin/vault/rotate` | platform admin | re-seal every secret under the current master key |

Names are a letter then letters, digits and underscores (at most 64), and a group
key is `group.key`; a name cannot be both a secret and a group. A plain secret value is
8 bytes to 64 KiB (short values could not be scrubbed reliably). A key inside a group
may be shorter, for an effort or a temperature: values under 8 characters are treated
as settings, so they are not masked in stored input, logs or events. Without `VAULT_MASTER_KEY` the vault is off and these
routes answer `501 vault_not_configured`.

**What a workflow run does with a secret.** The worker decrypts the organization's
secrets only when a node's config mentions `secrets`, hands the real value to the
executor, and stores the node input with the value replaced by `***`. The value is
also scrubbed from the node's stored output and error, so an upstream that echoes a
header back, or a transport error that quotes a URL, cannot leak it. Secrets never
reach logs or run events. Group keys under 8 characters are the exception above:
they are kept as written. Referring to a secret that does not exist, only inside
`${...}` (plain text such as `secrets.md` is not a reference), fails the node
once, permanently, with `E_SECRET_MISSING` and the secret's name, or
`group.key` for a missing key of a group.

**Rotating the master key** needs no downtime: (1) generate a new 32-byte key;
(2) deploy **every** process (API and workers) with it as `VAULT_MASTER_KEY`, a
higher `VAULT_KEY_VERSION`, and the previous key in `VAULT_OLD_KEYS=<version>:<key>`;
(3) only then `POST /be/v1/admin/vault/rotate` (repeat until `failed` is 0; it is
safe while serving, and each row is replaced compare-and-set on its old version);
(4) when `GET /be/v1/admin/vault` shows nothing under the old version, remove the old
key. Step 2 must finish first: a process that does not yet hold the new key cannot
read secrets that were already re-sealed.

---

## 4d. Providers and agents (M13)

**Provider connections** hold an organization's API key for one LLM provider. The
kinds follow the providers omp supports with a key (`GET /be/v1/provider-kinds`):
`anthropic`, `openai` and `gemini` use their own APIs; `ollama` (local, no key),
`ollama_cloud`, `openrouter`, `groq`, `deepseek`, `mistral`, `xai`, `together`,
`cerebras`, `moonshot`, `minimax`, `zai`, `qwen`, `huggingface`, `nvidia`, `venice`,
`nanogpt` and `zenmux` speak the OpenAI API at their own endpoint, which `base_url`
may override (a proxy, or Ollama on another host); `openai_compatible` is any other
server and needs a `base_url` that includes the version path. Keys are sealed with
the vault master key and are **write-only**: only a hint (the last four characters)
is ever shown. Every request goes through the egress guard, since a base URL is
chosen by the tenant. The workspace manages providers on their own page, apart from
connectors.

| Endpoint | Role | Purpose |
|---|---|---|
| `POST /be/v1/providers/test` | admin | `{provider, base_url?, api_key}` → `{ok, models}` by listing the models the key can use; saves nothing. A bad key is `422 provider_test_failed` and the message never echoes the key |
| `POST /be/v1/providers` | admin | `{name, provider, base_url?, api_key, skip_test?}`; **tests first and saves only if the test passes**; returns the connection and the models found |
| `GET /be/v1/providers` | editor | connections, with how many agents use each |
| `GET /be/v1/provider-kinds` | editor | every kind: `{id, title, base_url, key_optional, native}` |
| `GET /be/v1/providers/{id}/models` | editor | list available models using this organization’s saved account; returns `{ok, models}`, never the key |
| `POST /be/v1/providers/{id}/test` | admin | re-test a saved connection |
| `DELETE /be/v1/providers/{id}` | admin | `409 connection_in_use` while an agent uses it |

**Agents** are named, reusable definitions: which connection and model, the system
prompt, an `input_schema` and `output_schema` (the JSON Schema subset used by
`wait_event`), `tools` (the connectors it may search), `max_iterations` (required,
1 to 50), `max_tokens` and `temperature`. Leave `connection` empty to use the platform's own
provider (`ANTHROPIC_API_KEY`). Saving validates everything; editing bumps `revision`,
which each run's node metrics record.

| Endpoint | Role | Purpose |
|---|---|---|
| `GET /be/v1/agents`, `GET /be/v1/agents/{name}` | viewer | read |
| `POST /be/v1/agents`, `PUT /be/v1/agents/{name}`, `DELETE /be/v1/agents/{name}` | editor | manage (the name in the path is the identity) |
| `POST /be/v1/agents/{name}/test` | editor | run once with `{input?, prompt?}`; spends real tokens, creates no run; a failing agent is a 200 with `ok: false` and the reason |
| `GET /be/v1/agent-tools` | viewer | the three tools every agent gets, and `searchable_kinds` |

Use an agent in a workflow with `type: agent, config: {agent: <name>, input: {...}}`
(`02 §4.6`). The input is checked against `input_schema` and passed to the model as
JSON; the reply is validated against `output_schema`, and a violation is a permanent
failure. A missing agent or connection fails the node by name (`E_AGENT_NOT_FOUND`,
`E_PROVIDER_NOT_FOUND`).

**Tools.** `tools` names saved connectors in the same organization, which the agent
searches with `glob`, `grep` and `read` (`02 §4.14`). Saving checks that each one
exists and is a kind an agent can search (`searchable_kinds`): a database, MongoDB,
Gmail, Calendar, Docs, Sheets, Slack or Notion. The model chooses only among the
attached connectors and never sees credentials. Tools have a 30s execution budget and
a 32 KiB result budget, and only read: writes are workflow steps. There are at most
64 references in one agent definition.

MCP tool references use `"mcp:<connection-name>"` in `tools`. Saving checks that
each name belongs to this organization and has kind `mcp`; discovery happens
at execution time; remote catalogs come from the selected servers. Duplicate tool references
and more than eight MCP server references are rejected.

---

## 4e. Connectors (M14–M15)

A **connection** is an organization's named link to an external service that
workflow nodes can use by name: SMTP and Resend for email, OAuth services
(Gmail, Google Drive, Google Docs, Sheets, Calendar, Dropbox, OneDrive),
PostgreSQL, MySQL, MongoDB, Slack, Notion and MCP servers. Workflows and agents
reuse the same saved connectors. Settings
(host, port, default sender) are stored in clear; credentials are sealed with the
vault master key and are **write-only**: the API shows only a hint (the last four
characters) and, for OAuth connections, the connected account.

| Endpoint | Role | Purpose |
|---|---|---|
| `GET /be/v1/integrations` | editor | all supported kinds, `oauth`, `oauth_configured`, configuration fields, and `actions: [{name, title, kinds, read_only, input_schema}]` |
| `POST /be/v1/connections/test` | admin | `{kind, config, secret}`: prove settings and credentials work, save nothing |
| `POST /be/v1/connections` | admin | `{name, kind, config, secret, skip_test?}`; **tests first and saves only if the test passes** (`422 connection_test_failed`; the message never echoes a credential) |
| `GET /be/v1/connections`, `GET /be/v1/connections/{name}` | editor | settings and hint, never credentials |
| `PUT /be/v1/connections/{name}` | admin | replace the settings; send `secret` to replace the credentials, omit it to keep them; the kind cannot change |
| `POST /be/v1/connections/{name}/test` | admin | re-test with the stored credentials |
| `DELETE /be/v1/connections/{name}` | admin | nodes that still name it fail with `E_CONNECTION_NOT_FOUND` |

**MongoDB** uses `kind: mongo`, `config: {database, tls?}` and
`secret: {uri}`. The URI supports `mongodb://` and `mongodb+srv://`; keep its
credentials in `secret`. TLS defaults to `verify-full`; `disable` is an explicit
option for a trusted local server. URI TLS overrides are rejected. Saving tests
with a ping through the egress guard. Use a MongoDB user with only the permissions
required by its granted actions.

**Slack and Notion** use `kind: slack|notion`, empty `config`, and
`secret: {access_token}`. Save tests Slack `auth.test` or Notion `/v1/users/me`.
Slack needs `chat:write` for sending and the appropriate channel/group/DM history
scopes for reads; add the bot to its channels. Notion needs the read/update
capabilities required by the actions, with pages shared with the integration.
Notion requests pin API version `2025-09-03`.

**OAuth connections** (Gmail, Google Drive, Google Docs, Sheets, Calendar, Dropbox, OneDrive) are created by signing
in with the provider, not by pasting credentials, and appear in `GET /be/v1/integrations`
(with `oauth: true`). Sign-in is available only when `oauth_configured: true`, after the operator registers an OAuth app for the provider:

1. `POST /be/v1/connections/oauth/start` (admin) with `{kind, name}` returns an
   `authorize_url`. It carries a one-time `state` bound to your organization and a PKCE
   challenge; send the user's browser there. Naming an existing connection of the same
   kind reconnects it with new tokens (use this when access was revoked).
2. The provider redirects to `GET /be/v1/oauth/callback` (register
   `{PUBLIC_URL}/be/v1/oauth/callback` with the provider). It needs no session, consumes the
   state (so a replay fails), exchanges the code, looks up the account and stores the
   tokens sealed with the vault key, then redirects the browser to
   `{PUBLIC_URL}/ui/connectors?connected=<name>`, or `?error=<message>` if anything failed.
3. Access tokens are refreshed shortly before they expire, once however many nodes need
   it. If the provider no longer honours the refresh token the node fails permanently with
   `E_CONNECTION_REAUTH` and says to reconnect.

Scopes are the narrowest that work: Gmail `gmail.send`, Drive `drive.file` (files this
application created), Dropbox `files.content.write`, OneDrive `Files.ReadWrite`.

Kinds and their fields: **smtp** (`host`, `port` default 587, `tls` = `starttls` |
`tls` | `none`, `username`, `from`; secret `password`), **resend** (`from`; secret
`api_key`). A password is never sent over an unencrypted connection to a host other
than loopback, and `starttls` refuses to continue when the server does not offer it.
**postgres** and **mysql** (`host`, `port`, `database`, `username`, `tls` =
`verify-full` (default) | `require` | `disable`; secret `password`): the password is
never placed in a connection string, and TLS certificates are verified unless the
connection says otherwise — there is no silent fallback to plaintext.
**mcp** (`url`, `transport` = `streamable_http` (default) | `sse`; optional
secret `token` for bearer authentication). The URL must have no embedded
credentials, query or fragment. Test-before-save initializes the session and
lists tools; it never calls a tool. Authless public servers are supported.
MCP servers use this connection store and its encryption/rotation lifecycle,
so there is no separate server credential store.
Every connection goes through the egress guard.

Credentials a node fetches from a connection are added to the same scrubber as
`${secrets.*}`: they are removed from the node's stored output and error, so a server
that echoes a rejected password back cannot leak it into a run record.

---

## 5. Operations

| Endpoint | Purpose |
|---|---|
| `GET /healthz` | process liveness; no DB call |
| `GET /readyz` | DB ping + migration version check |
| `GET /metrics` | Prometheus |
| `GET /be/v1/executors` | registered node types + their config JSON Schema |

`GET /be/v1/executors` is what lets an editor or docs page stay in sync with the
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
| `POST /be/v1/admin/node-executions/{id}/retry` | cancel a live attempt (fencing its worker) and insert attempt+1 as `ready`; a failed attempt in a live run is retried as is. `409` for a finished run or a node that already has a later attempt |
| `POST /be/v1/admin/node-executions/{id}/skip` | force-skip a ready, running or waiting node, then advance the run with normal skip semantics. `409` otherwise |
| `POST /be/v1/admin/workers/{worker_id}/release-leases` | release a crashed worker's running nodes so rows are reclaimed now rather than at `LeaseTTL`; returns `{released: n}` |

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

## 4f. Webhook triggers

Editors can list `GET /be/v1/triggers`. Organization admins create
`POST /be/v1/triggers` with `{name, workflow, input_mode: "body"|"envelope", signed,
enabled?}`; the workflow must be active and belong to that organization.
The response includes a one-time token and, when signed, a signing_secret.
Only the token's SHA-256 is stored; the signing secret is encrypted in the
versioned vault keyring. Lists return metadata only.

Admins can `PATCH /be/v1/triggers/{id}` with enabled/input_mode, delete that route,
and `POST /be/v1/triggers/{id}/rotate-token`. Rotation returns the new token once
and immediately invalidates the old URL. Foreign-organization IDs return 404.

`POST /hooks/{token}` is public and authenticated by the unguessable token.
Disabled, deleted, or unknown tokens return 404. `body` requires a JSON object
and passes it as workflow input. `envelope` accepts any JSON body and passes
`{body, headers, query}`; header/query values are arrays. Authorization, Cookie,
signature, and Idempotency-Key headers are excluded from the envelope. Request
bodies share the API's 1 MiB limit.

Signed hooks require `X-Workflow-Signature: sha256=<hex digest>`, computed with
HMAC-SHA256 using the UTF-8 signing_secret exactly as returned (do not base64
decode it), over the **exact raw request bytes**. Missing/incorrect signatures
return 401. Use HTTPS and keep token URLs out of access logs.

Idempotency-Key (up to 200 bytes) is scoped to each trigger. A matching replay
returns the existing run with 200; new runs return 201. Reusing a key with
different resolved input returns 409. Monthly plan limits apply atomically to
new runs; valid replays do not consume another allowance. Runs identify their
trigger as webhook and the creating webhook name. They pin the workflow version
current at the time the first request starts the run.

## 4g. Workflow assistant

The UI calls `POST /be/v1/assistant/workflows` with
`{requirements, stepwise: true, approved_yaml?, step_instructions?}`.
The creator uses the platform OpenRouter key (`ASSISTANT_PROVIDER=openrouter`,
`ASSISTANT_API_KEY`) and the fixed model `z-ai/glm-5.3-flash`. It ignores tenant
account and model overrides. No dropdown is shown for the creator. Requirements
are capped at 16,000 UTF-8 bytes, step guidance at 4,000, and workflow YAML at 64 KiB.
`ASSISTANT_BASE_URL` optionally routes through an operator-managed proxy.

Each proposal contains exactly one new node. The server appends it to the
accepted YAML without changing existing nodes, inputs, defaults or outputs.
The UI requires explicit acceptance before requesting another step, and saves
accepted steps as an unpublished browser draft. Generating never publishes or
starts a run. The compiler checks each candidate, with at most three repair
rounds (four model calls total).

The model receives Weft's language reference, executor and connector action
schemas, organization resource names, and agent input/output structure. It
receives no stored credentials, key hints, agent instructions/descriptions,
schema examples/defaults/enums, or accepted step configuration values. Compiler
repair feedback omits messages that might quote existing values. Accepted YAML
stays on the Weft server. Free-form context and feedback are sent to OpenRouter:
users should supply secret/account references rather than paste credentials.
The creator has no tools, connector access, or execution capabilities.

The legacy full-draft API also accepts `{requirements, connection?, model?}`
without `stepwise`; this retains tenant provider selection and defaults to
`ASSISTANT_MODEL`. Account setup and running agents share the embedded provider
catalog in `internal/services/providers/catalog.json`, exposed at
`GET /be/v1/provider-kinds`. Model lists are not truncated to 200 entries.

A valid draft returns 200 with `{yaml, valid: true, step?, attempts, diagnostics, spec,
tokens_in, tokens_out, cost_usd, cost_known}`. An invalid final draft returns
422 with the same draft/diagnostics and valid:false, allowing manual repair.
Absent configuration returns 501, provider failure 502, and timeout 504. The
request is capped at two minutes and the YAML at 64 KiB. Unknown model pricing
is reported with cost_known:false. Token costs are estimates; calls use the
selected provider's billing account.

## 4h. Embedded web application

The API serves the React/Vite application under `/ui/`, including
`/ui/workflows` and `/ui/accept-invite?token=…`. `/` redirects to `/ui/`.
Frontend assets are served under `/ui/assets/`. Hashed assets are immutable;
HTML is not cached.
The backend API lives under `/be/v1`; unregistered backend routes and old
`/v1` routes return 404. Webhook ingress remains at `/hooks/{token}`.
Health and metrics endpoints remain at `/healthz`, `/readyz` and `/metrics`.
The browser uses same-origin session cookies and SSE; no credential is stored
in localStorage. Public authentication mutations also reject mismatched browser
origins. The worker's health/metrics server does not serve the UI.
