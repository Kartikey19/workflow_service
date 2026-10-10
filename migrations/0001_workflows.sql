-- +goose Up
create table workflows (
  id          uuid        primary key,
  tenant_id   uuid        not null,
  name        text        not null,
  description text,
  created_at  timestamptz not null default now(),
  updated_at  timestamptz not null default now(),
  archived_at timestamptz,

  constraint workflows_name_fmt
    check (name ~ '^[a-z0-9][a-z0-9-]{0,62}[a-z0-9]$')
);

create unique index workflows_tenant_name_uq on workflows (tenant_id, name);

create table workflow_versions (
  id          uuid        primary key,
  workflow_id uuid        not null references workflows(id),
  version     int         not null,
  yaml_source text        not null,
  spec        jsonb       not null,
  spec_hash   text        not null,
  created_by  text,
  created_at  timestamptz not null default now(),

  constraint wfv_version_positive check (version > 0)
);

create unique index wfv_workflow_version_uq
  on workflow_versions (workflow_id, version);
create index wfv_workflow_hash_ix on workflow_versions (workflow_id, spec_hash);

-- +goose Down
drop table workflow_versions;
drop table workflows;
