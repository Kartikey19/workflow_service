# 01 — Data Model

PostgreSQL 16+. Seven tables. Five of them are needed to ship v1
(`workflows`, `workflow_versions`, `runs`, `node_executions`, `run_events`);
`run_waits` arrives in M5 and `blobs` only when you actually hit payload limits.

Read `§7 Worked traces` first if you learn better from examples — the DDL will
make more sense afterwards.

---

## 1. Entity relationships

```
workflows (1) ──< workflow_versions (1) ──< runs (1) ──< node_executions
                                             │              │
                                             ├──< run_events│
                                             └──< run_waits >┘
```

- A **workflow** is a name. It has no behaviour.
- A **version** is an immutable compiled spec. All behaviour lives here.
- A **run** is one execution of one version.
- A **node_execution** is one attempt at one node of one run. This is where
  virtually all the rows and all the reads are.

---

## 2. DDL

### 2.1 Conventions

- Primary keys are UUIDv7 (`uuid` column type). v7 is time-ordered, which keeps
  B-tree inserts append-mostly and makes `order by id` a usable proxy for
  `order by created_at`. Generate in Go (`github.com/google/uuid` v1.6+
  `uuid.NewV7()`), not in the DB — you often need the ID before the insert.
- All timestamps are `timestamptz`, always stored UTC.
- Status columns are `text` with a `check` constraint, not Postgres enums.
  Enums require a lock-taking `ALTER TYPE` to extend; text + check is a
  validated-not-valid migration away from painless.
- Every user-facing table carries `tenant_id`. Even if you have one tenant today.
  Retrofitting tenancy is a six-week project; carrying the column is free.

### 2.2 workflows

```sql
create table workflows (
  id          uuid        primary key,
  tenant_id   uuid        not null,
  name        text        not null,
  description text,
  created_at  timestamptz not null default now(),
  updated_at  timestamptz not null default now(),
  archived_at timestamptz,

  constraint workflows_name_fmt check (name ~ '^[a-z0-9][a-z0-9-]{0,62}[a-z0-9]$')
);

create unique index workflows_tenant_name_uq on workflows (tenant_id, name);
```

`name` is the stable public identifier used by API callers
(`POST /be/v1/runs {"workflow": "order-fulfilment"}`). Constrain the format at the
DB level — you will otherwise get names with spaces and emoji in them, and they
will end up in URLs.

Archive rather than delete: `archived_at` blocks new runs but leaves history
readable.

### 2.3 workflow_versions

```sql
create table workflow_versions (
  id          uuid        primary key,
  workflow_id uuid        not null references workflows(id),
  version     int         not null,
  yaml_source text        not null,
  spec        jsonb       not null,
  spec_hash   text        not null,      -- sha256 of canonicalised spec
  created_by  text,
  created_at  timestamptz not null default now(),

  constraint wfv_version_positive check (version > 0)
);

create unique index wfv_workflow_version_uq on workflow_versions (workflow_id, version);
create index wfv_workflow_hash_ix on workflow_versions (workflow_id, spec_hash);
```

**Why both `yaml_source` and `spec`.** `yaml_source` is what the human wrote —
you need it verbatim for diffs, for "show me what I deployed", and for re-compiling
under a newer compiler. `spec` is the normalised graph the engine reads: defaults
filled in, `needs` edges resolved to indices, retry policies materialised,
expressions pre-parsed and cached. **The runtime never parses YAML.**

**`spec_hash`** lets `POST /be/v1/workflows` be idempotent: if the incoming YAML
compiles to a spec identical to the current latest version, return that version
instead of creating v(n+1). Without this, a CI pipeline that deploys on every
commit generates a new version per commit forever.

**Version allocation.** Do not use a sequence. Allocate inside the publish
transaction:

```sql
-- inside tx, after taking a row lock on the workflow
select coalesce(max(version), 0) + 1 from workflow_versions where workflow_id = $1;
```

with `select ... from workflows where id = $1 for update` first, so two concurrent
publishes serialise instead of colliding on the unique index.

There is deliberately **no `is_active` / `latest` column.** Latest is
`max(version)`. A mutable pointer column invites the bug where the pointer and the
rows disagree. If you later need blue/green or pinned aliases, add a separate
`workflow_aliases (workflow_id, alias, version)` table.

### 2.4 runs

```sql
create table runs (
  id                  uuid        primary key,
  tenant_id           uuid        not null,
  workflow_id         uuid        not null references workflows(id),
  workflow_version_id uuid        not null references workflow_versions(id),

  status              text        not null,
  input               jsonb       not null default '{}'::jsonb,
  output              jsonb,
  error               jsonb,

  idempotency_key     text,
  correlation_id      text,
  trigger             text        not null default 'api',  -- api|schedule|event|subworkflow
  parent_run_id       uuid        references runs(id),     -- reserved for M9

  created_at          timestamptz not null default now(),
  started_at          timestamptz,
  ended_at            timestamptz,
  deadline_at         timestamptz,   -- whole-run timeout, from spec.timeout

  constraint runs_status_ck check (status in
    ('pending','running','waiting','succeeded','failed','cancelled')),
  constraint runs_terminal_ended_ck check
    ((status in ('succeeded','failed','cancelled')) = (ended_at is not null))
);

create unique index runs_tenant_idem_uq
  on runs (tenant_id, idempotency_key) where idempotency_key is not null;
create index runs_correlation_ix on runs (tenant_id, correlation_id)
  where correlation_id is not null;
create index runs_active_ix on runs (tenant_id, created_at desc)
  where status in ('pending','running','waiting');
create index runs_deadline_ix on runs (deadline_at)
  where status in ('running','waiting') and deadline_at is not null;
```

**`correlation_id` is the column that makes this service operable.** It is the
business entity the run is about: `ORD-88213`, `patient:4471`, `ticket:9902`.
Within a week of launch someone will ask "what happened to order 88213" and this
index is the answer. Populate it from the API request, and also allow the spec to
declare `correlation: ${input.order_id}` so callers can't forget.

**`idempotency_key`** dedupes *run creation*. A courier webhook that fires twice
must not start two fulfilment runs. Partial unique index so NULLs don't collide.
On conflict, return the existing run with `200` rather than erroring — see
`04-api.md §3.2`.

**`runs.status` vs node statuses.** The run status is a derived summary, updated
by the resolver. `waiting` means every non-terminal node is in `waiting` (nothing
is runnable and nothing is running) — useful for dashboards and for the
"stuck run" sweeper.

**`deadline_at`** is the whole-run timeout. Without it, a run with a bad
`wait_event` key lives forever. Default it from `spec.timeout` (which itself
should default to something like 30 days).

### 2.5 node_executions — the workhorse

```sql
create table node_executions (
  id           uuid        primary key,
  run_id       uuid        not null references runs(id) on delete cascade,
  tenant_id    uuid        not null,

  node_id      text        not null,          -- the `id:` from the YAML
  node_type    text        not null,          -- http | transform | branch | ...
  branch_key   text        not null default '',  -- '' for v1; foreach index in M9
  attempt      int         not null default 1,

  status       text        not null,
  input        jsonb,          -- template-resolved config actually sent to executor
  output       jsonb,
  error        jsonb,          -- {code, message, retryable, executor_detail}
  metrics      jsonb,          -- {duration_ms, tokens_in, tokens_out, cost_usd, http_status}

  idempotency_key text       not null,   -- STABLE across attempts, see §4
  run_after    timestamptz not null default now(),
  lease_owner  text,
  lease_expires_at timestamptz,

  created_at   timestamptz not null default now(),
  started_at   timestamptz,
  ended_at     timestamptz,

  constraint ne_status_ck check (status in
    ('ready','running','waiting','succeeded','failed','skipped','cancelled')),
  constraint ne_attempt_ck check (attempt >= 1)
);

create unique index ne_identity_uq
  on node_executions (run_id, node_id, branch_key, attempt);

-- the claim query's index. partial => tiny, regardless of table size
create index ne_claim_ix on node_executions (run_after, id)
  where status = 'ready';

-- the sweeper's indexes
create index ne_lease_ix on node_executions (lease_expires_at)
  where status = 'running';
create index ne_waiting_ix on node_executions (run_after)
  where status = 'waiting';

-- run detail page: all nodes of one run
create index ne_run_ix on node_executions (run_id, created_at);
```

Column-by-column rationale for the non-obvious ones:

**`branch_key`** — empty string for ordinary nodes. A `foreach` node (M9) has one
controller row with `branch_key = ''` and one row per list element,
`branch_key = '0','1','2'…`, each holding its element in `item` (copied to retry
attempts).
**Add this column now.** Widening a unique index on your largest, hottest table
later is a migration you will keep postponing.

**`attempt`** — retries create a *new row*, they do not mutate the old one. This
gives you a free forensic record ("it failed twice with 503 then succeeded") at
the cost of a few rows. The unique index makes double-retry impossible.

**`input`** — store the *resolved* config, after `${...}` substitution, not the
raw spec config. When a node misbehaves, the first question is always "what did
it actually receive", and you want that answer without re-evaluating expressions
against state that has since changed.

**`run_after`** — does triple duty: retry backoff, `sleep` node wakeup, and
`wait_event` timeout deadline. One column, three features, one index.

**`lease_owner` / `lease_expires_at`** — this pair *is* the crash-recovery
mechanism. A worker claims by setting them; a sweeper resets rows whose lease
expired. There is no separate "worker liveness" concept, no heartbeat table, no
registry of workers.

**`metrics`** — a jsonb grab-bag, deliberately unstructured. `duration_ms` on
every node, plus whatever the executor wants to add: HTTP status, retry count,
LLM tokens and cost. Agent nodes make this valuable immediately, but the delivery
team will use it for p99 latency per node within a month.

### 2.6 run_waits (M5)

```sql
create table run_waits (
  id                uuid        primary key,
  tenant_id         uuid        not null,
  run_id            uuid        not null references runs(id) on delete cascade,
  node_execution_id uuid        not null references node_executions(id) on delete cascade,

  wait_key          text        not null,
  status            text        not null,      -- waiting|fulfilled|expired|cancelled
  payload           jsonb,                     -- what the signal delivered
  created_at        timestamptz not null default now(),
  expires_at        timestamptz,
  fulfilled_at      timestamptz,

  constraint rw_status_ck check (status in ('waiting','fulfilled','expired','cancelled'))
);

-- at most one live waiter per key, per tenant. this is the concurrency control
create unique index rw_key_live_uq on run_waits (tenant_id, wait_key)
  where status = 'waiting';
create index rw_node_ix on run_waits (node_execution_id);
```

`wait_key` is a string the workflow author composes, typically
`"order.picked_up:${input.order_id}"`. The signalling API looks up exactly one
live waiter by `(tenant_id, wait_key)`. The partial unique index enforces the
invariant that makes signal delivery unambiguous — if two runs try to wait on the
same key, the second one fails loudly at wait-registration time rather than
producing a race at delivery time.

Note `expires_at` is duplicated as `node_executions.run_after` on the waiting
node. That denormalisation is intentional: it lets the sweeper find timed-out
waits using the same index it already scans, without joining.

### 2.7 run_events

```sql
create table run_events (
  id         bigint      generated always as identity primary key,
  run_id     uuid        not null,
  tenant_id  uuid        not null,
  seq        int         not null,
  type       text        not null,
  node_id    text,
  branch_key text,
  attempt    int,
  payload    jsonb       not null default '{}'::jsonb,
  created_at timestamptz not null default now()
);

create unique index re_run_seq_uq on run_events (run_id, seq);
create index re_created_ix on run_events (created_at);
```

Append-only. Never updated, never deleted (until retention, §8).

Event types (keep this list closed and versioned):

```
run.created      run.started      run.succeeded   run.failed    run.cancelled
node.scheduled   node.started     node.succeeded  node.failed   node.skipped
node.retrying    node.waiting     node.signalled  node.timeout
```

`seq` is per-run and gap-free. Allocate it inside the same transaction as the
state change: `select coalesce(max(seq),0)+1 from run_events where run_id=$1`
under the run row lock you are already holding (see `03-engine.md §5`). Gap-free
sequencing is what lets a consumer poll `?after=seq` and know it missed nothing.

**Payload discipline:** events carry *references and summaries*, not full
payloads. `node.succeeded` carries `{output_bytes: 2048, duration_ms: 130}`, not
the output itself — that is already in `node_executions.output`. Otherwise your
event log is 10× the size of your data.

### 2.8 blobs (defer until needed)

```sql
create table blobs (
  id         uuid primary key,
  tenant_id  uuid not null,
  run_id     uuid,
  uri        text not null,        -- s3://bucket/key
  size_bytes bigint not null,
  sha256     text not null,
  created_at timestamptz not null default now()
);
```

Policy: any `input`/`output` jsonb exceeding **256 KB** is written to object
storage and replaced in the row with `{"$blob": "<uuid>", "size": 1048576}`.
The `expr` layer transparently dereferences `$blob` on read. Agent nodes with
long conversation histories are what will force this; do not build it before then,
but *do* enforce the 256 KB cap from day one so the failure mode is a clean error
rather than a 40 MB row that wedges replication.

---

## 3. State machines

### 3.1 node_executions

```
                  ┌──────────────────────────────► skipped   (resolver: dep failed/pruned)
                  │
   (insert) ──► ready ──claim──► running ──┬──► succeeded
                  ▲                        │
                  │                        ├──► failed        (permanent, or attempts exhausted)
                  │                        │
                  │                        ├──► waiting ──signal──► succeeded
                  │                        │          └──timeout──► failed | (on_timeout goto)
                  │                        │
                  └────new row, attempt+1──┘   (retryable error; run_after = now+backoff)
                  ▲
                  │
      lease expiry (sweeper resets running → ready, same attempt)
```

Legal transitions, exhaustively:

| From | To | Trigger |
|---|---|---|
| — | `ready` | resolver enqueues (deps satisfied) |
| — | `skipped` | resolver prunes (dep failed or branch not taken) |
| `ready` | `running` | worker claims (sets lease) |
| `running` | `succeeded` | executor returns Result, fence valid |
| `running` | `failed` | permanent error, or retryable with attempts exhausted |
| `running` | `waiting` | executor returned `Control.Wait` or `Control.Sleep` |
| `running` | `ready` | **sweeper**, lease expired (same row, same attempt) |
| `waiting` | `succeeded` | signal delivered |
| `waiting` | `failed` | wait timed out and no `on_timeout` |
| `waiting` | `ready` | `sleep` elapsed (`run_after` reached) |
| any non-terminal | `cancelled` | run cancelled |

Note `running → ready` on lease expiry keeps the same `attempt`. Lease expiry is
not a retry — it is a re-delivery. The `attempt` counter is for *executor*
failures, and conflating them makes your retry budget lie to you.

### 3.2 runs

```
pending ──► running ──┬──► succeeded
              ▲  │    ├──► failed
              │  ▼    └──► cancelled
            waiting
```

`running ↔ waiting` flips as the resolver observes the node population.
Computed after every terminal node transition:

```
if any node in (ready, running)          → running
else if any node in (waiting)            → waiting
else if any node failed and unhandled    → failed
else                                     → succeeded
```

---

## 4. Invariants

Write these as assertions in tests. They are the contract.

1. **I1** — At most one row per `(run_id, node_id, branch_key, attempt)`.
   *Enforced by:* `ne_identity_uq`. This is what makes concurrent resolvers safe.

2. **I2** — A node is `ready` only if every node in its `needs` has a terminal row
   in the same run (and, for `join: all`, all succeeded).
   *Enforced by:* resolver logic + I1.

3. **I3** — `lease_owner is not null` ⟺ `status = 'running'`.
   *Enforced by:* only the claim query sets it; every terminal write clears it.

4. **I4** — A run is terminal ⟺ no node execution is in `ready|running|waiting`.
   *Enforced by:* resolver's completion check under `select ... for update` on
   the run row.

5. **I5** — `idempotency_key` is identical across all attempts of the same
   `(run_id, node_id, branch_key)`.
   *Enforced by:* derivation, §5.

6. **I6** — `run_events.seq` is gap-free and strictly increasing per run.

7. **I7** — A `workflow_versions` row is never updated after insert.
   *Enforced by:* a `BEFORE UPDATE` trigger that raises, if you want belt and braces.

---

## 5. Idempotency key derivation

```go
// Stable across attempts and across lease re-deliveries.
// NOT stable across runs — two runs of the same workflow are two real operations.
func idempotencyKey(runID uuid.UUID, nodeID, branchKey string) string {
    h := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%s", runID, nodeID, branchKey))
    return "wf_" + base32.StdEncoding.WithPadding(base32.NoPadding).
        EncodeToString(h[:15]) // 24 chars, plenty
}
```

This value is passed to the executor and forwarded to downstream systems (as an
`Idempotency-Key` HTTP header, a payment-provider reference, etc.).

The critical property: **attempt 2 presents the same key as attempt 1.** If
attempt 1 charged the card and then lost the response, attempt 2 hits the
provider's dedupe and gets the original result back instead of double-charging.
Deriving the key from `node_executions.id` — which is per-attempt — would silently
break exactly this case, which is also the case you will only discover in
production. This is the single most commonly botched detail in workflow engines.

---

## 6. Query cookbook

The queries you will actually write. Put them in `internal/store/queries/*.sql`
and generate with sqlc.

### 6.1 Claim ready work — the entire scheduler

```sql
-- name: ClaimReady :many
update node_executions ne
   set status           = 'running',
       lease_owner      = @worker_id,
       lease_expires_at = now() + @lease_ttl::interval,
       started_at       = coalesce(ne.started_at, now())
 where ne.id in (
       select id
         from node_executions
        where status = 'ready'
          and run_after <= now()
        order by run_after
        for update skip locked
        limit @batch_size
 )
returning ne.*;
```

`FOR UPDATE SKIP LOCKED` is what makes N workers safe with zero coordination:
each transaction skips rows another transaction has locked. `order by run_after`
gives rough FIFO fairness. Batch of 10–50; larger batches increase the blast
radius of a worker dying mid-batch.

### 6.2 Fenced result write

```sql
-- name: CompleteNode :execrows
update node_executions
   set status  = @status,
       output  = @output,
       error   = @error,
       metrics = @metrics,
       ended_at = now(),
       lease_owner = null,
       lease_expires_at = null
 where id = @id
   and status = 'running'
   and lease_owner = @worker_id
   and lease_expires_at > now();
```

If this returns **0 rows**, the worker lost its lease while executing — another
worker has already picked the node up. **Discard the result and log it.** Do not
retry, do not force the write. This is the fencing token pattern and it is the
only thing standing between you and two workers writing conflicting outputs for
one node.

### 6.3 Enqueue a successor (concurrency-safe)

```sql
-- name: EnqueueNode :one
insert into node_executions (
  id, run_id, tenant_id, node_id, node_type, branch_key, attempt,
  status, input, idempotency_key, run_after
) values (
  @id, @run_id, @tenant_id, @node_id, @node_type, @branch_key, 1,
  @status, @input, @idempotency_key, coalesce(@run_after, now())
)
on conflict (run_id, node_id, branch_key, attempt) do nothing
returning *;
```

`ON CONFLICT DO NOTHING` handles the diamond race: nodes A and B both complete
simultaneously on different workers, both resolve successor C, both try to insert
it. One wins, one no-ops, C runs exactly once. Returning zero rows here is normal
and must not be treated as an error.

### 6.4 Are all dependencies of node C satisfied?

```sql
-- name: DepStates :many
select node_id, status
  from node_executions
 where run_id = @run_id
   and branch_key = @branch_key
   and node_id = any(@dep_node_ids::text[])
   and attempt = (
       select max(attempt) from node_executions ne2
        where ne2.run_id = node_executions.run_id
          and ne2.node_id = node_executions.node_id
          and ne2.branch_key = node_executions.branch_key
   );
```

The `max(attempt)` correlated subquery matters: a node with a failed attempt 1
and a succeeded attempt 2 is *succeeded*. Reading all attempts and taking the
latest in Go is equally fine and easier to test — do whichever you'll understand
at 3am.

### 6.5 Sweeper: reclaim expired leases

```sql
-- name: ReclaimExpiredLeases :many
update node_executions
   set status = 'ready', lease_owner = null, lease_expires_at = null
 where id in (
   select id from node_executions
    where status = 'running' and lease_expires_at < now()
    order by lease_expires_at
    for update skip locked
    limit 100
 )
returning id, run_id, node_id, attempt;
```

### 6.6 Sweeper: expire timed-out waits

```sql
-- name: ExpireWaits :many
select ne.id, ne.run_id, ne.node_id, ne.branch_key
  from node_executions ne
 where ne.status = 'waiting'
   and ne.run_after <= now()
 order by ne.run_after
 for update skip locked
 limit 100;
```

Uses `ne_waiting_ix`. Handles both `sleep` expiry (→ `ready`) and `wait_event`
timeout (→ `on_timeout` branch or `failed`); the node type tells you which.

### 6.7 Deliver a signal (one transaction)

```sql
begin;

  select id, run_id, node_execution_id
    from run_waits
   where tenant_id = @tenant_id and wait_key = @key and status = 'waiting'
     for update;
  -- 0 rows => 404 or park as an early signal, see 03-engine §7.3

  update run_waits
     set status = 'fulfilled', payload = @payload, fulfilled_at = now()
   where id = @wait_id;

  update node_executions
     set status = 'succeeded', output = @payload, ended_at = now(), run_after = now()
   where id = @node_execution_id and status = 'waiting';

  -- then: resolver advances the DAG, inside this same tx
commit;
```

### 6.8 Operator query: "what happened to order 88213?"

```sql
select r.id, r.status, w.name, r.created_at, r.ended_at,
       ne.node_id, ne.attempt, ne.status, ne.error, ne.metrics->>'duration_ms'
  from runs r
  join workflows w on w.id = r.workflow_id
  left join node_executions ne on ne.run_id = r.id
 where r.tenant_id = $1 and r.correlation_id = 'ORD-88213'
 order by r.created_at desc, ne.created_at;
```

---

## 7. Worked traces

### 7.1 Delivery: `order-fulfilment`

Spec (see `02-workflow-spec.md` for full syntax):

```yaml
name: order-fulfilment
nodes:
  - id: reserve_stock
    type: http
    retry: {max_attempts: 5, backoff: exponential, initial: 2s}
  - id: assign_rider
    type: http
    needs: [reserve_stock]
  - id: await_pickup
    type: wait_event
    needs: [assign_rider]
    config: {key: "order.picked_up:${input.order_id}", timeout: 45m}
  - id: notify_customer
    type: http
    needs: [await_pickup]
```

**t0 — `POST /be/v1/runs`.** One transaction inserts the run and the *root nodes
only*. Nodes are created lazily as their dependencies resolve; pre-creating the
whole graph is impossible once M9 adds dynamic fan-out, so never start doing it.

```
runs
 id=r1  status=running  correlation_id=ORD-88213  input={"order_id":"ORD-88213"}
        workflow_version_id=v1  started_at=t0  deadline_at=t0+30d

node_executions
 id=n1  run_id=r1  node_id=reserve_stock  type=http  attempt=1
        status=ready  run_after=t0  idempotency_key=wf_K3F9...

run_events
 seq=1  run.created      seq=2  run.started      seq=3  node.scheduled{reserve_stock}
```

**t0+80ms — worker claims n1.**

```
 n1: status=running  lease_owner=worker-7  lease_expires_at=t0+60s  started_at=t0+80ms
 event seq=4  node.started{reserve_stock}
```

**t0+2.1s — inventory API returns 503.** Executor classifies it retryable. The
engine writes n1 as failed and inserts **a new row** for attempt 2 with backoff:

```
 n1: status=failed  error={"code":"upstream_5xx","retryable":true,"http_status":503}
     ended_at=t0+2.1s  lease cleared
 n2: node_id=reserve_stock  attempt=2  status=ready  run_after=t0+4.1s
     idempotency_key=wf_K3F9...     ← IDENTICAL to n1. this is the point.
 event seq=5  node.retrying{node:reserve_stock, attempt:2, delay_ms:2000}
```

**t0+4.4s — attempt 2 succeeds.** The resolver runs in the same transaction as
the completion: `assign_rider.needs = [reserve_stock]`, latest attempt succeeded,
so enqueue it.

```
 n2: status=succeeded  output={"reservation_id":"RSV-771"}  metrics={"duration_ms":290}
 n3: node_id=assign_rider  attempt=1  status=ready  run_after=now
     input={"method":"POST","url":".../riders/assign","body":{"reservation":"RSV-771"}}
                          ↑ resolved config, stored so you can see what was sent
 events seq=6 node.succeeded{reserve_stock}   seq=7 node.scheduled{assign_rider}
```

**t0+5.0s — assign_rider succeeds; `await_pickup` becomes ready, is claimed,
and the `wait_event` executor returns `Control.Wait`.** The node does not
complete — it parks:

```
 n4: node_id=await_pickup  status=waiting  run_after=t0+45m  ← the timeout deadline
     lease cleared (a waiting node holds no lease)

run_waits
 id=w1  run_id=r1  node_execution_id=n4  status=waiting
        wait_key='order.picked_up:ORD-88213'  expires_at=t0+45m

runs
 r1: status=waiting          ← no node is ready or running

 events seq=8 node.succeeded{assign_rider}  seq=9 node.waiting{await_pickup, key:...}
```

**This is the important frame.** The run now consumes: one row, zero goroutines,
zero connections, zero memory. It can sit here for 45 minutes or, in the
healthcare workflow, for 30 days. Nothing in the process needs to remember it
exists. Every worker can be restarted, redeployed, or replaced.

**t0+12m — courier webhook: `POST /be/v1/signals {key:"order.picked_up:ORD-88213"}`.**
Single transaction (§6.7):

```
 w1: status=fulfilled  payload={"rider":"R-42","at":"..."}  fulfilled_at=t0+12m
 n4: status=succeeded  output={"rider":"R-42","at":"..."}
 n5: node_id=notify_customer  status=ready
 r1: status=running

 events seq=10 node.signalled{await_pickup}  seq=11 node.succeeded{await_pickup}
        seq=12 node.scheduled{notify_customer}
```

**t0+12m2s — notify_customer succeeds. Resolver finds no successors and no
non-terminal nodes**, so under `select ... from runs where id=r1 for update`:

```
 r1: status=succeeded  ended_at=t0+12m2s  output={"notified":true}
 events seq=13 node.succeeded{notify_customer}   seq=14 run.succeeded
```

Total: 1 run row, 5 node_execution rows, 1 wait row, 14 events. That is the
complete, queryable, replayable history of a 12-minute distributed workflow.

### 7.2 The timeout path

Same run, but no signal arrives. At `t0+45m` the sweeper's `ExpireWaits` query
(§6.6) returns n4:

```
 w1: status=expired
 n4: status=failed  error={"code":"wait_timeout","key":"order.picked_up:ORD-88213"}
 event seq=10 node.timeout{await_pickup}
```

With `on_timeout: reassign_rider` in the spec, the resolver enqueues
`reassign_rider` instead of failing the run, and `notify_customer` — whose only
dep failed — is written as `skipped`, cascading to anything downstream of it.
Without `on_timeout`, the run goes to `failed` and every unstarted node is
`skipped`. Either way the sweeper needed no knowledge of couriers.

### 7.3 Healthcare: a 30-day sleep

```yaml
name: post-discharge-followup
nodes:
  - id: create_care_plan
    type: http
  - id: nurse_review
    type: human_task
    needs: [create_care_plan]
    config: {queue: rn-review, form: care_plan_v2, sla: 24h}
  - id: wait_30d
    type: sleep
    needs: [nurse_review]
    config: {duration: 720h}
  - id: outreach_call
    type: http
    needs: [wait_30d]
```

`nurse_review` is `wait_event` with a nicer name: the executor registers a wait
keyed `task:${run.id}:nurse_review`, and `POST /be/v1/tasks/{id}/complete` is sugar
over `POST /be/v1/signals`. Same table, same delivery path, same timeout mechanism.

`wait_30d` is the whole point of the design:

```
 n3: node_id=wait_30d  node_type=sleep  status=waiting  run_after=<t + 720h>
```

One row. No timer, no cron entry, no scheduled job, no `run_waits` row (a sleep
has no key — it wakes on `run_after` alone). Thirty days later the sweeper flips
it to `ready` and the run continues on whatever workers happen to be alive then,
running whatever version of your binary you have deployed by then, against the
spec version pinned at `t0`.

### 7.4 Agentic: where the columns pay off

```yaml
  - id: classify
    type: agent
    needs: [fetch_ticket]
    config:
      model: claude-sonnet-5
      system: "Classify this support ticket."
      prompt: "${nodes.fetch_ticket.output.body}"
      tools: [search_kb, get_customer]
      max_iterations: 6
```

The agent's internal tool-calling loop is **inside one executor call** — it is not
modelled as DAG nodes. The DAG is for durable, resumable steps; a model's
tool loop is a single logical step that either completes or is retried whole.
(If you later want each tool call durable and resumable, that is M9's
sub-workflow feature, not a change to this table.)

The columns that earn their keep here:

```
 n2: node_id=classify  node_type=agent  status=succeeded
     output={"severity":"P1","category":"billing","reasoning":"..."}
     metrics={"duration_ms":8400,"iterations":3,"tokens_in":12043,
              "tokens_out":802,"cost_usd":0.0391,"model":"claude-sonnet-5"}
```

`metrics.cost_usd` summed over `node_executions` grouped by `tenant_id` is your
per-tenant LLM billing, and it required no new tables. `max_iterations` is
enforced by the executor and is non-negotiable — an agent node without an
iteration budget is an unbounded spend, and the `deadline_at` run timeout is far
too coarse to catch it.

---

## 8. Growth, retention, partitioning

Rough sizing per run: 1 run row (~400 B) + N node rows (~1–3 KB each, dominated
by `input`/`output` jsonb) + ~2N event rows (~300 B). A 10-node workflow at
10 000 runs/day ≈ **200 MB/day**, ≈ 6 GB/month.

That is fine for a year on a single instance. Plan for it anyway:

1. **Partition `run_events` and `node_executions` by month** on `created_at`
   (declarative range partitioning) once you cross ~100 GB. Detaching an old
   partition is instant; `DELETE FROM ... WHERE created_at < ...` on a 200 GB
   table is a weekend.
2. **Retention policy per tenant**, driven by domain: agentic runs can be trimmed
   at 30 days; healthcare will contractually require 7 years. Store
   `retention_days` on the tenant, archive to Parquet in object storage, delete
   the partition.
3. **Never delete `workflow_versions`.** They are tiny and they are the only thing
   that makes an archived run interpretable.
4. **Enforce the 256 KB payload cap** (§2.8) from day one, as a check constraint
   or in the store layer. `pg_column_size(output) < 262144`.

---

## 9. Migration file order (M0)

```
migrations/
  0001_workflows.sql              -- workflows, workflow_versions
  0002_runs.sql                   -- runs
  0003_node_executions.sql        -- node_executions + all four indexes
  0004_run_events.sql             -- run_events
  0005_run_waits.sql              -- run_waits           (M5)
  0006_blobs.sql                  -- blobs               (when needed)
```

Use `pressly/goose` or plain numbered SQL applied by a tiny embedded runner
(`//go:embed migrations/*.sql`). Avoid ORM-driven auto-migration: you want the
DDL reviewable in a diff.

Every migration must be **forward-only and additive**. If you need to change a
column type, add the new column, backfill, dual-write, then drop in a later
release. There will be runs mid-flight during every deploy — that is the entire
point of the system.

---

**Next:** `02-workflow-spec.md` — the YAML the user writes and how it compiles.
