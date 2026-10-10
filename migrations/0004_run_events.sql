-- +goose Up
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

-- +goose Down
drop table run_events;
