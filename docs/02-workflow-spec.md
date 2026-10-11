# 02 — Workflow Spec: YAML Surface & Compiler

The YAML is the product. It is the thing users see, diff, review, and get wrong.
Treat it with the care you would give a public API: version it, validate it
brutally, and never break it.

---

## 1. Top-level document

```yaml
apiVersion: workflow/v1          # required. gates all future breaking changes
name: order-fulfilment           # required. [a-z0-9-], unique per tenant
description: Reserve stock, assign a rider, wait for pickup, notify.

inputs:                          # optional. JSON-Schema subset, validated at run creation
  order_id:   {type: string, required: true}
  priority:   {type: string, enum: [standard, express], default: standard}

correlation: ${input.order_id}   # optional. populates runs.correlation_id
timeout: 720h                    # optional. whole-run deadline. default 720h (30d)

defaults:                        # optional. merged into every node
  retry: {max_attempts: 3, backoff: exponential, initial: 1s, max: 60s}
  timeout: 30s

nodes:                           # required. >= 1
  - id: reserve_stock
    type: http
    # ...

outputs:                         # optional. shape of runs.output
  reservation: ${nodes.reserve_stock.output.reservation_id}
  rider:       ${nodes.await_pickup.output.rider}
```

**`apiVersion` is not decoration.** The day you need to change `needs` semantics
or the expression syntax, `workflow/v2` lets you do it without touching the
thousands of `workflow/v1` documents already in the database. The compiler
dispatches on it. Reject unknown values.

---

## 2. Node

```yaml
- id: assign_rider          # required. unique within the workflow. [a-z0-9_]
  type: http                # required. must be a registered executor type
  needs: [reserve_stock]    # optional. list of node ids. default [] => root node
  join: all                 # optional. all | any. default all
  if: ${input.priority == "express"}   # optional. skip this node when false
  config: {}                # required (may be empty). executor-specific, templated
  retry:                    # optional. overrides defaults.retry
    max_attempts: 5
    backoff: exponential    # exponential | linear | fixed
    initial: 2s
    max: 60s
    jitter: 0.2             # ±20%, full-jitter recommended
    retry_on: [timeout, upstream_5xx, rate_limited]  # optional whitelist
  timeout: 30s              # optional. per-attempt deadline
  on_failure: refund_stock  # optional. node id to run instead of failing the run
  on_timeout: reassign      # optional. wait_event / sleep only
```

### Field semantics

**`needs` + `join`** define readiness. With `join: all` (the default) the node
becomes `ready` when every dependency's latest attempt is `succeeded`; if any
dependency ends `failed` or `skipped`, this node is `skipped`. With `join: any`
it becomes `ready` as soon as *one* dependency succeeds, and is `skipped` only if
*all* dependencies end non-succeeded. Use `join: any` for merge points after a
`branch`.

**`if`** is evaluated at readiness time, after templates resolve. False ⇒ the
node is written as `skipped` (a real row, so downstream skip-cascade works) and
its successors are evaluated normally.

**`on_failure`** converts a terminal failure into a control transfer. The failing
node still ends `failed` — you do not lose the record — but instead of failing the
run, the resolver enqueues the named node. This is the explicit alternative to
implicit saga/compensation machinery: rollback is a node you can read in the YAML.

**`retry.retry_on`** whitelists error codes. Omit it and the engine's default
classification applies (`03-engine.md §6.2`).

---

## 3. Expressions

### 3.1 Syntax

Any string in `config`, `if`, `correlation`, or `outputs` may contain `${ ... }`.
The contents are **CEL** (`github.com/google/cel-go`).

Two substitution modes, and the difference matters:

```yaml
config:
  url: "https://api.internal/orders/${input.order_id}/reserve"   # interpolation → string
  body: ${nodes.fetch.output}                                    # whole-value → object
  count: ${nodes.fetch.output.items.size()}                      # whole-value → int
```

> If a string consists of **exactly one** `${expr}` and nothing else, the
> expression's **typed value** is substituted (object, array, number, bool, null).
> Otherwise every `${expr}` is stringified and concatenated into a string.

Without the whole-value rule you cannot pass a JSON object to an HTTP body without
serialising it by hand. With it, both cases read naturally. Implement it in the
template walker (§6.3).

**Escaping:** `$${not_an_expression}` yields the literal `${not_an_expression}`.

*(Note: earlier sketches used `{{ }}`. The spec is `${ }` — it avoids colliding
with Go template tooling, Helm, and GitHub Actions, all of which people will
paste around this YAML.)*

### 3.2 Evaluation context

| Binding | Type | Available |
|---|---|---|
| `input` | object | always — the run input, post-validation with defaults applied |
| `nodes` | map[string]NodeView | nodes that have completed in this run |
| `run` | object | `{id, correlation_id, started_at, attempt}` |
| `secrets` | map[string]string | resolved secret refs; **never logged, never persisted** |
| `now` | timestamp | evaluation time |
| `item`, `index` | any, int | inside a `foreach` body only (M9) |

`NodeView` is `{output: any, status: string, attempt: int, metrics: object}`.
Referencing a node that has not completed is a **compile-time error** if it is not
a transitive dependency, and a runtime error otherwise (which should be impossible
given the compile check — treat it as a bug, not a user error).

### 3.3 Why CEL and not Go templates

- Typed, with compile-time checking against a declared environment. You can
  validate every expression at publish time and reject `nodes.typo.output` before
  a single run exists.
- Non-Turing-complete and resource-bounded. A tenant cannot write a workflow
  expression that spins a CPU.
- Sandboxed by construction — no filesystem, no network, no reflection.
- Sane error messages with source positions, which you can surface in the publish
  response.

Go's `text/template` gives you none of that and encourages people to embed logic.
Register a small, closed function library (`string`, `int`, `has`, `size`,
`timestamp`, `duration`, `base64`, `uuid`, `env` — deliberately no `http`, no
`exec`, no `file`) and resist every request to extend it. Each function you add is
permanent.

### 3.4 Compile-time expression checking

At publish time, for each node, build a CEL environment whose `nodes` binding
declares **only that node's transitive dependencies**, then `env.Compile()` every
expression in its config. This catches:

- typos in node ids (`nodes.reserv_stock`)
- references to nodes that are not upstream (a real ordering bug, always)
- type errors (`input.count + "x"`)
- syntax errors

Cache the compiled `cel.Program`s in the spec by index so the runtime never
re-parses. This alone eliminates most of the failure modes users will otherwise
hit at 2am on run 500.

---

## 4. Node type reference

Each type is an `Executor` (see `03-engine.md §3`). `ValidateConfig` runs at
publish time, so a malformed `config` is a 400 on publish, never a runtime failure.

### 4.1 `http` (M3)

```yaml
- id: reserve_stock
  type: http
  config:
    method: POST                       # GET|POST|PUT|PATCH|DELETE
    url: "https://inv.internal/reserve"
    headers:
      Authorization: "Bearer ${secrets.INVENTORY_TOKEN}"
      Content-Type: application/json
    body: {order_id: '${input.order_id}', items: '${nodes.fetch.output.items}'}
    timeout: 10s
    expect_status: [200, 201]          # anything else => error
    send_idempotency_key: true         # adds Idempotency-Key: <node key>. default true
```

Output: `{status, headers, body}` where `body` is parsed JSON if the response
content-type is JSON, else a string.

Notes for the implementation: enforce an **egress allowlist per tenant** (this is
an SSRF vector by construction — block link-local `169.254.169.254`, RFC1918, and
loopback unless explicitly allowed), cap response bodies at the 256 KB payload
limit, and always set a client timeout.

### 4.2 `transform` (M2)

```yaml
- id: shape
  type: transform
  needs: [fetch]
  config:
    value:
      customer: ${nodes.fetch.output.customer.name}
      total:    ${nodes.fetch.output.items.map(i, i.price).sum()}
```

Pure CEL, no I/O. Output is the resolved `value`. This is your glue node and it
will be the most-used type in the system. Because it has no side effects, it is
trivially retryable and needs no idempotency key.

### 4.3 `branch` (M4)

```yaml
- id: route
  type: branch
  needs: [classify]
  config:
    cases:
      - when: ${nodes.classify.output.severity == "P0"}
        goto: page_oncall
      - when: ${nodes.classify.output.severity == "P1"}
        goto: create_incident
    default: auto_reply                # optional; omit => no successor, arm ends
```

First matching case wins. Returns `Control.Goto{NodeID}`; the resolver enqueues
only that node and **skip-cascades the other arms**, so a `join: any` merge node
downstream still resolves correctly.

Validation rule: every `goto` target and the `default` must exist, and must
declare this branch node in its `needs` (so the graph stays a real DAG and the
resolver has an edge to follow).

### 4.4 `sleep` (M5)

```yaml
- id: wait_30d
  type: sleep
  needs: [nurse_review]
  config: {duration: 720h}       # or: until: ${nodes.x.output.scheduled_at}
```

Returns `Control.Sleep{Until}`. The engine sets `status='waiting'`,
`run_after=until`. No `run_waits` row — a sleep has no key. Costs one row.

### 4.5 `wait_event` (M5)

```yaml
- id: await_pickup
  type: wait_event
  needs: [assign_rider]
  config:
    key: "order.picked_up:${input.order_id}"
    timeout: 45m
    schema: {type: object, required: [rider]}   # optional: validate signal payload
  on_timeout: reassign_rider
```

Returns `Control.Wait{Key, Until, Schema}`. The engine inserts a `run_waits` row
and parks the node. The signal payload becomes the node's `output`.

**Key design guidance for authors:** the key must be globally unique per live
wait within a tenant (enforced by `rw_key_live_uq`). Always namespace it with a
business identifier: `domain.event:entity_id`. A bare `"pickup"` key will collide
across concurrent orders and the second run will fail to register its wait — which
is the right failure, loudly at wait time, rather than a signal going to the wrong
run.

### 4.6 `agent` (M7)

```yaml
- id: classify
  type: agent
  needs: [fetch_ticket]
  config:
    model: claude-sonnet-5-5
    system: "You classify support tickets. Reply with JSON only."
    prompt: ${nodes.fetch_ticket.output.body}
    tools: [search_kb, get_customer]     # names registered in the tool registry
    max_iterations: 6                    # REQUIRED. hard cap on the tool loop
    max_tokens: 4096
    temperature: 0
    output_schema:                       # optional; enables structured output + validation
      type: object
      required: [severity, category]
      properties:
        severity: {type: string, enum: [P0, P1, P2]}
        category: {type: string}
```

Output: with `output_schema`, the model's JSON reply, validated against it (a
violation, a reply that is not JSON, or one cut off at `max_tokens` is a permanent
failure). Without it, `{"text": "<reply>"}`.
Metrics: `{iterations, tokens_in, tokens_out, cost_usd, model, stop_reason}`.

The tool-calling loop lives **inside** this executor and is a single durable step.
Do not model it as DAG nodes; a partially-completed model turn is not resumable in
any meaningful sense, and trying to make it so is how this project turns into a
two-year project.

`max_iterations` is mandatory at the schema level — reject a spec without it. The
run-level `timeout` is far too coarse to bound agent spend.

### 4.7 `human_task` (M8)

```yaml
- id: nurse_review
  type: human_task
  needs: [create_care_plan]
  config:
    queue: rn-review
    form: care_plan_v2
    assignee: ${nodes.lookup.output.nurse_id}   # optional
    sla: 24h
    payload: {patient: '${input.patient_id}', plan: '${nodes.create_care_plan.output}'}
  on_timeout: escalate_to_physician
```

Implemented as `wait_event` with key `task:${run.id}:${node.id}` plus a row in a
`tasks` read-model table for the worklist UI. `POST /v1/tasks/{id}/complete` is
sugar over the signals endpoint. Same wait table, same timeout path.

### 4.8 `noop` (M2)

No config. Succeeds immediately. Exists for fan-in points and for testing the
engine without any executor doing real work — build this one first.

---

## 5. Complete examples

### 5.1 Delivery

```yaml
apiVersion: workflow/v1
name: order-fulfilment
inputs:
  order_id: {type: string, required: true}
correlation: ${input.order_id}
timeout: 24h
defaults:
  retry: {max_attempts: 3, backoff: exponential, initial: 1s, max: 30s}
  timeout: 15s

nodes:
  - id: reserve_stock
    type: http
    retry: {max_attempts: 5, backoff: exponential, initial: 2s, max: 60s}
    config:
      method: POST
      url: "https://inv.internal/v1/reserve"
      headers: {Authorization: "Bearer ${secrets.INV_TOKEN}"}
      body: {order_id: '${input.order_id}'}

  - id: assign_rider
    type: http
    needs: [reserve_stock]
    on_failure: release_stock
    config:
      method: POST
      url: "https://dispatch.internal/v1/assign"
      body: {reservation: '${nodes.reserve_stock.output.body.reservation_id}'}

  - id: await_pickup
    type: wait_event
    needs: [assign_rider]
    on_timeout: reassign_rider
    config:
      key: "order.picked_up:${input.order_id}"
      timeout: 45m

  - id: reassign_rider
    type: http
    needs: [await_pickup]
    join: any
    config:
      method: POST
      url: "https://dispatch.internal/v1/reassign"
      body: {order_id: '${input.order_id}'}

  - id: notify_customer
    type: http
    needs: [await_pickup]
    config:
      method: POST
      url: "https://notify.internal/v1/sms"
      body:
        to: ${input.order_id}
        text: "Your order is on the way with ${nodes.await_pickup.output.rider}."

  - id: release_stock
    type: http
    needs: [assign_rider]
    join: any
    config:
      method: POST
      url: "https://inv.internal/v1/release"
      body: {reservation: '${nodes.reserve_stock.output.body.reservation_id}'}

outputs:
  reservation: ${nodes.reserve_stock.output.body.reservation_id}
```

### 5.2 Agentic

```yaml
apiVersion: workflow/v1
name: support-triage
inputs:
  ticket_id: {type: string, required: true}
correlation: ${input.ticket_id}

nodes:
  - id: fetch_ticket
    type: http
    config:
      method: GET
      url: "https://desk.internal/v1/tickets/${input.ticket_id}"

  - id: classify
    type: agent
    needs: [fetch_ticket]
    config:
      model: claude-sonnet-5-5
      system: "Classify the ticket. Respond with JSON only."
      prompt: ${nodes.fetch_ticket.output.body.description}
      tools: [search_kb]
      max_iterations: 6
      output_schema:
        type: object
        required: [severity, category]
        properties:
          severity: {type: string, enum: [P0, P1, P2]}
          category: {type: string}

  - id: route
    type: branch
    needs: [classify]
    config:
      cases:
        - when: ${nodes.classify.output.severity == "P0"}
          goto: page_oncall
      default: auto_reply

  - id: page_oncall
    type: http
    needs: [route]
    config: {method: POST, url: "https://pager.internal/v1/page", body: {ticket: '${input.ticket_id}'}}

  - id: auto_reply
    type: agent
    needs: [route]
    config:
      model: claude-sonnet-5-5
      system: "Draft a helpful reply."
      prompt: ${nodes.fetch_ticket.output.body.description}
      max_iterations: 3

  - id: record
    type: http
    needs: [page_oncall, auto_reply]
    join: any                      # exactly one arm ran; the other is skipped
    config:
      method: POST
      url: "https://desk.internal/v1/tickets/${input.ticket_id}/triage"
      body: {severity: '${nodes.classify.output.severity}'}
```

Note `join: any` on `record`. This is the merge-after-branch pattern, and it is
the one thing about the DAG semantics that users will get wrong. The validator
should **warn** when a node's `needs` span multiple arms of the same branch and
`join` is `all` — that workflow will hang until timeout, every time.

### 5.3 Healthcare

```yaml
apiVersion: workflow/v1
name: post-discharge-followup
inputs:
  patient_id: {type: string, required: true}
  discharge_at: {type: string, required: true}
correlation: "patient:${input.patient_id}"
timeout: 2160h   # 90 days

nodes:
  - id: create_care_plan
    type: http
    config:
      method: POST
      url: "https://ehr.internal/v1/care-plans"
      body: {patient: '${input.patient_id}'}

  - id: nurse_review
    type: human_task
    needs: [create_care_plan]
    on_timeout: escalate
    config:
      queue: rn-review
      form: care_plan_v2
      sla: 24h
      payload: {plan: '${nodes.create_care_plan.output.body}'}

  - id: escalate
    type: http
    needs: [nurse_review]
    join: any
    config: {method: POST, url: "https://ehr.internal/v1/escalate"}

  - id: wait_30d
    type: sleep
    needs: [nurse_review]
    config: {duration: 720h}

  - id: outreach_call
    type: http
    needs: [wait_30d]
    config:
      method: POST
      url: "https://crm.internal/v1/tasks"
      body: {patient: '${input.patient_id}', kind: followup_call}
```

Three domains. Same seven tables, same engine, same resolver. The only thing
that differs is which executors are registered.

---

## 6. The compiler

`internal/spec`. This is a pure function: `[]byte → (Spec, []Diagnostic)`. No
database, no network, no clock. That makes it exhaustively table-testable, which
matters because it is your entire defence against bad workflows.

### 6.1 Stages

```
raw YAML
  │  1. decode      sigs.k8s.io/yaml (YAML→JSON→struct), strict: unknown fields error
  ▼
Document
  │  2. normalise   apply `defaults` to each node; fill in every optional;
  │                 canonicalise durations to nanoseconds; sort node list by id
  ▼
Normalised
  │  3. validate    structural rules (§6.2). collect ALL diagnostics, don't fail fast
  ▼
  │  4. graph       build adjacency + reverse adjacency; topo-sort; detect cycles;
  │                 compute transitive deps per node
  ▼
  │  5. expressions compile every ${...} against a per-node CEL env limited to
  │                 that node's transitive deps
  ▼
  │  6. executors   for each node, registry.Get(type).ValidateConfig(config)
  ▼
Spec  +  spec_hash = sha256(canonical JSON)
```

**Collect all diagnostics.** A user with six typos should see six errors on one
publish, not discover them one deploy at a time. Diagnostic shape:

```go
type Diagnostic struct {
    Severity string  // error | warning
    Code     string  // E_UNKNOWN_NODE, E_CYCLE, W_UNREACHABLE, ...
    Message  string
    NodeID   string
    Path     string  // "nodes[2].config.url"
    Line     int     // from the YAML decoder, if obtainable
}
```

### 6.2 Validation rules

Errors (publish is rejected):

| Code | Rule |
|---|---|
| `E_API_VERSION` | `apiVersion` missing or unknown |
| `E_NAME_FORMAT` | name fails `^[a-z0-9][a-z0-9-]{0,62}[a-z0-9]$` |
| `E_NO_NODES` | `nodes` empty |
| `E_DUP_NODE_ID` | duplicate `id` |
| `E_NODE_ID_FORMAT` | id fails `^[a-z][a-z0-9_]{0,62}$` |
| `E_UNKNOWN_NODE` | `needs` / `goto` / `on_failure` / `on_timeout` references a nonexistent id |
| `E_CYCLE` | graph has a cycle (report the cycle path) |
| `E_NO_ROOT` | no node with empty `needs` |
| `E_UNKNOWN_TYPE` | `type` is not a registered executor |
| `E_CONFIG` | executor's `ValidateConfig` rejected it |
| `E_EXPR_SYNTAX` | CEL parse failure |
| `E_EXPR_SCOPE` | expression references a node that is not a transitive dependency |
| `E_EXPR_TYPE` | CEL type check failure |
| `E_SELF_REF` | node references its own output |
| `E_BRANCH_TARGET` | a `goto` target does not list the branch node in `needs` |
| `E_RETRY_RANGE` | `max_attempts` outside 1..20, or `initial` > `max` |
| `E_TIMEOUT_RANGE` | node timeout > run timeout |

Warnings (published, but surfaced in the response):

| Code | Rule |
|---|---|
| `W_UNREACHABLE` | node not reachable from any root |
| `W_JOIN_ALL_ACROSS_BRANCH` | `join: all` over nodes on different arms of one branch — this workflow will hang |
| `W_NO_TIMEOUT` | an `http` node with no timeout and no default |
| `W_UNUSED_INPUT` | a declared input is never referenced |
| `W_LONG_SLEEP` | a `sleep` longer than the run `timeout` |

`W_JOIN_ALL_ACROSS_BRANCH` is worth the graph analysis it costs. It is the single
most common way to author a workflow that silently hangs for 30 days.

### 6.3 The template walker

Used by the compiler (to find and compile expressions) and by the dispatcher (to
resolve them at execution time). Same code, two modes.

```go
// Walk every string in an arbitrary decoded-JSON value; replace ${...} segments.
func Resolve(v any, env *Env) (any, error) {
    switch t := v.(type) {
    case map[string]any:
        out := make(map[string]any, len(t))
        for k, vv := range t {
            r, err := Resolve(vv, env); if err != nil { return nil, err }
            out[k] = r
        }
        return out, nil
    case []any:
        out := make([]any, len(t))
        for i, vv := range t {
            r, err := Resolve(vv, env); if err != nil { return nil, err }
            out[i] = r
        }
        return out, nil
    case string:
        return resolveString(t, env)   // implements the whole-value rule, §3.1
    default:
        return v, nil
    }
}
```

`resolveString` must:
1. scan for `${` … `}` respecting `$$` escaping and nested braces inside string
   literals within the expression,
2. if the whole string is exactly one expression, return the typed CEL value,
3. otherwise evaluate each and concatenate string forms.

Write a table test for this function with at least 30 cases before you trust it —
including `"$${literal}"`, `"a${x}b${y}c"`, `"${obj}"`, `"${ \"}\" }"`. It is 60
lines of code that everything else depends on.

### 6.4 Compiled spec shape

What actually goes in `workflow_versions.spec`:

```json
{
  "api_version": "workflow/v1",
  "name": "order-fulfilment",
  "timeout_ns": 86400000000000,
  "correlation_expr": 0,
  "inputs": { "order_id": {"type":"string","required":true} },
  "nodes": [
    {
      "id": "assign_rider",
      "type": "http",
      "needs": ["reserve_stock"],
      "join": "all",
      "config": { "method":"POST", "url":"https://...", "body":{"reservation":"${...}"} },
      "retry": {"max_attempts":3,"backoff":"exponential","initial_ns":1e9,"max_ns":3e10,"jitter":0.2},
      "timeout_ns": 15000000000,
      "on_failure": "release_stock"
    }
  ],
  "edges": { "reserve_stock": ["assign_rider"], "assign_rider": ["await_pickup","release_stock"] },
  "reverse_edges": { "assign_rider": ["reserve_stock"] },
  "roots": ["reserve_stock"],
  "topo_order": ["reserve_stock","assign_rider","await_pickup","notify_customer","release_stock"],
  "outputs": { "reservation": "${...}" }
}
```

`edges` is precomputed forward adjacency — the resolver's hot path is "given node
X just went terminal, who are X's successors", and it should be a map lookup, not
a scan over all nodes. `topo_order` is for UI rendering and for the unreachability
check.

Compiled CEL programs are **not** serialised into the spec (they aren't
serialisable). Keep an in-process LRU cache keyed by `workflow_version_id` that
lazily compiles on first use; versions are immutable, so the cache never needs
invalidation. That immutability is what makes this cache trivially correct — one
more dividend from P2.

---

## 7. Input validation

`inputs` is a deliberately small JSON-Schema subset: `type` (string, number,
integer, boolean, object, array), `required`, `default`, `enum`, `format`
(`uuid`, `date-time`, `email`), `minimum`/`maximum`, `pattern`.

Validate at **run creation**, in the API layer, before any row is written. Reject
with 400 and a per-field error list. Apply defaults, then persist the *resolved*
input into `runs.input` — so `input.priority` is always present in expressions and
you never write `has(input.priority) ? ... : "standard"` in a workflow.

Resist growing this into full JSON Schema. If a tenant needs conditional
subschemas in their workflow inputs, the complexity belongs in their caller.

---

## 8. Publish semantics

`POST /v1/workflows` with a YAML body:

1. Compile. On any `error` diagnostic → `400` with the full diagnostic list.
2. `select ... from workflows where tenant_id=$1 and name=$2 for update`
   (insert the workflow row if absent).
3. Compute `spec_hash`. If it equals the latest version's hash → return that
   version, `200`, `{"unchanged": true}`.
4. Else allocate `version = max+1`, insert, commit, return `201` with the version
   and any `warning` diagnostics.

Step 3 is what keeps a deploy-on-every-commit CI pipeline from generating 4 000
identical versions. Step 2's row lock is what keeps two concurrent deploys from
colliding on `(workflow_id, version)`.

There is no unpublish and no edit. Versions are immutable (P2, I7). To stop a bad
version being used, archive the workflow or publish a fixed version — running
instances of the bad version continue on their pinned spec, which is correct: a
half-executed workflow must finish under the rules it started with.

---

**Next:** `03-engine.md` — the runtime that executes all of this.
