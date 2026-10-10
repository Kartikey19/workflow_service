-- +goose Up
create table run_waits (
  id                uuid        primary key,
  tenant_id         uuid        not null,
  run_id            uuid        not null references runs(id) on delete cascade,
  node_execution_id uuid        not null references node_executions(id) on delete cascade,

  wait_key          text        not null,
  status            text        not null,
  payload           jsonb,
  created_at        timestamptz not null default now(),
  expires_at        timestamptz,
  fulfilled_at      timestamptz,

  constraint rw_status_ck check (status in ('waiting', 'fulfilled', 'expired', 'cancelled'))
);

-- At most one live waiter per key per tenant: signal delivery is unambiguous.
create unique index rw_key_live_uq on run_waits (tenant_id, wait_key)
  where status = 'waiting';
create index rw_node_ix on run_waits (node_execution_id);
create index rw_run_ix on run_waits (run_id);

-- Detects an idempotency-key reuse with a different request body (409).
alter table runs add column request_hash text;
alter table runs add column created_by text;

create index runs_tenant_created_ix on runs (tenant_id, created_at desc, id desc);
create index runs_workflow_created_ix on runs (workflow_id, created_at desc, id desc);
create index re_run_ix on run_events (run_id, seq);

-- I7: a published version is immutable.
create function workflow_versions_immutable() returns trigger
language plpgsql as $$
begin
  raise exception 'workflow_versions rows are immutable';
end;
$$;

create trigger wfv_immutable
  before update on workflow_versions
  for each row execute function workflow_versions_immutable();

-- +goose Down
drop trigger wfv_immutable on workflow_versions;
drop function workflow_versions_immutable();
drop index re_run_ix;
drop index runs_workflow_created_ix;
drop index runs_tenant_created_ix;
alter table runs drop column created_by;
alter table runs drop column request_hash;
drop table run_waits;
