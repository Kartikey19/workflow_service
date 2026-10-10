-- +goose Up
create table node_executions (
  id               uuid        primary key,
  run_id           uuid        not null references runs(id) on delete cascade,
  tenant_id        uuid        not null,

  node_id          text        not null,
  node_type        text        not null,
  branch_key       text        not null default '',
  attempt          int         not null default 1,

  status           text        not null,
  input            jsonb,
  output           jsonb,
  error            jsonb,
  metrics          jsonb,

  idempotency_key  text        not null,
  run_after        timestamptz not null default now(),
  lease_owner      text,
  lease_expires_at timestamptz,

  created_at       timestamptz not null default now(),
  started_at       timestamptz,
  ended_at         timestamptz,

  constraint ne_status_ck check (status in
    ('ready', 'running', 'waiting', 'succeeded', 'failed', 'skipped', 'cancelled')),
  constraint ne_attempt_ck check (attempt >= 1)
);

create unique index ne_identity_uq
  on node_executions (run_id, node_id, branch_key, attempt);
create index ne_claim_ix on node_executions (run_after, id)
  where status = 'ready';
create index ne_lease_ix on node_executions (lease_expires_at)
  where status = 'running';
create index ne_waiting_ix on node_executions (run_after)
  where status = 'waiting';
create index ne_run_ix on node_executions (run_id, created_at);

-- +goose Down
drop table node_executions;
