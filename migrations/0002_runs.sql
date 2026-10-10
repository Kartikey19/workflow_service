-- +goose Up
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
  trigger             text        not null default 'api',
  parent_run_id       uuid        references runs(id),

  created_at          timestamptz not null default now(),
  started_at          timestamptz,
  ended_at            timestamptz,
  deadline_at         timestamptz,

  constraint runs_status_ck check (status in
    ('pending', 'running', 'waiting', 'succeeded', 'failed', 'cancelled')),
  constraint runs_terminal_ended_ck check
    ((status in ('succeeded', 'failed', 'cancelled')) = (ended_at is not null))
);

create unique index runs_tenant_idem_uq
  on runs (tenant_id, idempotency_key) where idempotency_key is not null;
create index runs_correlation_ix on runs (tenant_id, correlation_id)
  where correlation_id is not null;
create index runs_active_ix on runs (tenant_id, created_at desc)
  where status in ('pending', 'running', 'waiting');
create index runs_deadline_ix on runs (deadline_at)
  where status in ('running', 'waiting') and deadline_at is not null;

-- +goose Down
drop table runs;
