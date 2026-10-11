# 00 — Overview, Scope & Architecture

**Status:** design, pre-implementation
**Target runtime:** Go 1.27, PostgreSQL 16+
**Audience:** the person implementing this (you), six months from now

---

## 1. What this service is

A **durable DAG executor**. A user submits a YAML document describing a graph of
nodes. The service compiles it, stores it as an immutable version, and then
executes instances of it — surviving process crashes, machine restarts, and waits
that last for months.

The engine itself knows nothing about LLMs, deliveries, or patients. It knows:

1. how to turn YAML into a validated graph,
2. how to decide which node is runnable next,
3. how to hand that node to a registered executor,
4. how to persist every state transition so the run can resume from any point.

Everything domain-specific — calling an LLM, hitting a courier API, asking a nurse
to sign a form — lives behind a single Go interface (`Executor`, see `03-engine.md`).

## 2. Why this generalises across domains

The naive assumption is that "agentic workflows" and "delivery workflows" are
different products. They are not. They differ in **node types**, which are plugins,
and in **how long a run sits idle**, which is the only thing that constrains the
core architecture.

| Domain | Typical idle wait | Consequence for the core |
|---|---|---|
| Agentic (triage, research) | seconds–minutes | loops with dynamic exit; large payloads |
| Delivery / logistics | minutes–hours | event-driven wakeups, timeouts, compensation |
| Healthcare / compliance | days–months | sleeping runs must hold zero resources; hard audit trail |

That yields the single load-bearing rule of this system:

> **A waiting run is a row in Postgres. Never a goroutine, never a `time.Sleep`,
> never an in-memory timer.**

Get that right and a 30-day healthcare follow-up costs the same as a 30-second
retry backoff: one `timestamptz` in one row. Get it wrong and you have built
something that only works for agents.

## 3. Design principles

These are the decisions that are expensive to reverse. Everything else is detail.

**P1 — Compile once, at publish time.**
YAML is parsed, normalised, and fully validated when a version is published. The
runtime reads only the compiled `spec` JSON. A malformed workflow can never reach
the executor. Corollary: adding a validation rule is a *publish-time* concern and
never breaks running workflows.

**P2 — Runs pin to an immutable version.**
A run references `workflow_versions.id`, not `workflows.id`. Publishing v2 while
400 runs are mid-flight on v1 is a non-event. Versions are never mutated or deleted.

**P3 — No shared mutable run state.**
There is no `context` blob that nodes write into. Expressions read the *outputs of
other nodes* (`nodes.fetch_order.output.address`). Node outputs are immutable rows.
This kills an entire class of lost-update races between parallel branches, and makes
a run's history replayable and explainable.

**P4 — The database is the queue.**
`SELECT ... FOR UPDATE SKIP LOCKED` over a partial index. No Kafka, no Redis, no
separate scheduler process. This is good to several hundred nodes/second on modest
hardware — far past where this service will be when it matters. When you outgrow it,
you will know, and the interface (`store.ClaimReady`) is the seam to replace.

**P5 — At-least-once, with fencing.**
A leased node whose worker freezes will be re-leased by another worker. Executors
must therefore be idempotent, and the engine gives every node a **stable idempotency
key across retries** to make that achievable. Result writes are fenced by lease
ownership so a zombie worker cannot clobber a newer attempt.

**P6 — Control flow is a return value, not a special case.**
The engine does not know what `branch` or `sleep` mean. An executor returns an
optional `Control` directive (`Goto`, `Sleep`, `Wait`) and the engine acts on it.
New control-flow primitives are therefore new executors, not core changes.

**P7 — Every transition is an event.**
`run_events` is append-only and ordered per run. It is the audit trail, the debug
tool, the UI timeline, and the integration point for downstream consumers. Healthcare
and finance will require it; build it in v1 when it is free.

## 4. Non-goals (explicitly out of scope for v1)

| Not building | Why | Do instead |
|---|---|---|
| A bespoke expression language | 3-month tarpit disguised as a 3-day task | CEL (`google/cel-go`) |
| Automatic sagas / compensation | Implicit rollback is unexplainable in prod | Explicit `on_failure:` edge in YAML |
| A custom queue / broker | P4 | Postgres |
| Dynamic `foreach` fan-out, sub-workflows | Real, but they can wait | v2; reserve `branch_key` column now |
| A web UI | You don't yet know what to show | Ship `run_events` + timeline API first |
| Cross-run distributed transactions | Never a good idea | Idempotency keys + explicit compensation |
| Streaming node output to clients | Only agents need it | v2; the `Emit` hook is reserved for it |

## 5. Architecture

```
                 ┌──────────────────────────────────────────┐
   YAML ────────▶│  API server  (cmd/api)                   │
   HTTP clients  │                                          │
   signals ─────▶│  ├─ POST /be/v1/workflows   → compile+store  │
                 │  ├─ POST /be/v1/runs        → create run     │
                 │  ├─ POST /be/v1/signals     → fulfil wait    │
                 │  └─ GET  /be/v1/runs/:id    → read model     │
                 └────────────────────┬─────────────────────┘
                                      │
                          ┌───────────▼────────────┐
                          │      PostgreSQL        │
                          │  workflows             │
                          │  workflow_versions     │  ← immutable specs
                          │  runs                  │
                          │  node_executions       │  ← the queue
                          │  run_waits             │  ← external wakeups
                          │  run_events            │  ← append-only log
                          └───────────▲────────────┘
                                      │
                 ┌────────────────────┴─────────────────────┐
                 │  Worker  (cmd/worker)                │
                 │                                          │
                 │  claim loop ──▶ dispatcher ──▶ Executor   │
                 │       ▲                          registry │
                 │       │                          ├ http   │
                 │  sweeper (leases, wait timeouts) ├ transform
                 │                                  ├ branch │
                 │  resolver (advance the DAG)      ├ sleep  │
                 │                                  ├ wait_event
                 │                                  ├ agent  (v2)
                 │                                  └ human_task (v2)
                 └──────────────────────────────────────────┘
```

One binary, two modes. Run both in the same process in dev (`-mode=all`).
Workers are stateless and horizontally scalable; there is no leader, no coordinator,
and no partition assignment.

### Component responsibilities

| Component | Owns | Never does |
|---|---|---|
| `spec` | YAML → validated graph, versioning | touch runtime tables |
| `expr` | CEL evaluation, `${...}` template walking | I/O, network |
| `engine/claim` | leasing ready rows | interpret node config |
| `engine/dispatch` | resolve config templates, call executor, fence + write result | know node semantics |
| `engine/resolve` | advance the DAG, skip cascade, run completion | execute anything |
| `engine/sweep` | expired leases, wait timeouts, stuck runs | write outputs |
| `executors/*` | one node type each | touch the DB directly |
| `store` | all SQL | business logic |

## 6. Glossary

- **Workflow** — a named, tenant-scoped logical thing (`order-fulfilment`).
- **Version** — one immutable compiled spec of a workflow. Runs pin to this.
- **Spec** — the compiled, normalised graph (JSON) produced from YAML.
- **Run** — one execution instance of a version.
- **Node** — a vertex in the spec (static, defined in YAML).
- **Node execution** — one attempt at one node in one run (a row; dynamic).
- **Executor** — Go code that implements one node type.
- **Lease** — a time-bounded claim by a worker on a node execution.
- **Signal** — an external event that fulfils a `run_waits` row and wakes a run.
- **Terminal status** — `succeeded` | `failed` | `skipped`. Nothing follows.
- **Resolver** — the function that, given a node that just went terminal, decides
  which successors become `ready` or `skipped`.

## 7. Milestones

Each milestone is independently demoable. Detail and acceptance criteria in
`05-implementation-plan.md`.

| M | Scope | Rough size |
|---|---|---|
| **M0** | repo skeleton, migrations, `store` layer, CI | 2–3 days |
| **M1** | `spec` compiler: YAML → validated JSON, publish API | 4–5 days |
| **M2** | engine core: claim loop, lease, dispatch, resolver, run completion. Node types: `noop`, `transform` | 1 week |
| **M3** | `http` executor, retries + backoff, idempotency keys, error classification | 3–4 days |
| **M4** | `branch` + skip cascade, `on_failure` edges | 3 days |
| **M5** | `sleep`, `wait_event`, signals API, sweeper, timeouts | 4–5 days |
| **M6** | `run_events`, timeline API, cancellation, observability | 3–4 days |
| — | **v1 ships here** | ~5 weeks |
| **M7** | `agent` executor (Claude), token/cost metrics, `max_iterations` | 1 week |
| **M8** | `human_task`, task queues, SLA escalation | 1 week |
| **M9** | `foreach` fan-out (`branch_key`), sub-workflows | 1–2 weeks |

## 8. A note on build-vs-buy

A durable DAG executor with leases, signals, and versioned definitions is, in
outline, what Temporal already is. The justification for building this is that
**the YAML is the product surface**: you want tenants and non-Go engineers authoring
workflows, and you want the workflow definition to be data you can store, diff,
lint, and generate — not compiled Go.

Keep that as the north star. Every time you are tempted to add a feature because
"a real workflow engine has it", ask whether it makes the YAML surface better. If
it doesn't, it is probably scope creep.

---

**Next:** `01-data-model.md` — the schema, its invariants, and worked row traces.
