-- +goose Up

-- Draft test runs execute an unpublished definition. Each one is stored as an
-- immutable test version numbered below zero, so published version numbers
-- stay gap-free and every query that means "published" filters on is_test.
alter table workflow_versions add column is_test boolean not null default false;
alter table workflow_versions add constraint wfv_test_version_ck
  check ((version > 0) = (not is_test));
alter table workflow_versions drop constraint wfv_version_positive;
create index wfv_test_hash_ix on workflow_versions (workflow_id, spec_hash) where is_test;

-- Recurring schedules. A due schedule is a row whose next_run_at has passed;
-- workers claim it with FOR UPDATE SKIP LOCKED and start the run with an
-- idempotency key derived from the slot, so a slot starts at most one run.
create table schedules (
  id           uuid        primary key,
  org_id       uuid        not null,
  name         text        not null,
  workflow     text        not null,
  cron         text        not null,
  timezone     text        not null default 'UTC',
  input        jsonb       not null default '{}'::jsonb,
  overlap      text        not null default 'skip' check (overlap in ('skip', 'allow')),
  catch_up     text        not null default 'latest' check (catch_up in ('latest', 'skip')),
  enabled      boolean     not null default true,
  next_run_at  timestamptz,
  last_slot_at timestamptz,
  last_run_id  uuid,
  last_outcome text,
  last_error   text,
  created_by   text,
  created_at   timestamptz not null default now(),
  updated_at   timestamptz not null default now(),
  unique (org_id, name),
  constraint schedules_enabled_next_ck check (not enabled or next_run_at is not null)
);
create index schedules_due_ix on schedules (next_run_at) where enabled;

-- Alert channels receive workflow health notifications. Secrets (a Slack
-- incoming-webhook URL, a webhook URL and its signing key) are sealed with the
-- vault keyring like webhook trigger secrets.
create table alert_channels (
  id                 uuid        primary key,
  org_id             uuid        not null,
  name               text        not null,
  kind               text        not null check (kind in ('slack', 'webhook', 'email')),
  config             jsonb       not null default '{}'::jsonb,
  events             text[]      not null,
  workflows          text[]      not null default '{}',
  enabled            boolean     not null default true,
  secret_ciphertext  bytea,
  secret_nonce       bytea,
  secret_key_version integer,
  created_at         timestamptz not null default now(),
  updated_at         timestamptz not null default now(),
  unique (org_id, name)
);

-- One open incident per failing workflow: the first failure alerts, later
-- failures only count, and the next success sends a recovery.
create table workflow_health (
  workflow_id  uuid        primary key references workflows(id),
  org_id       uuid        not null,
  failing      boolean     not null default false,
  failing_since timestamptz,
  failures     integer     not null default 0,
  last_run_id  uuid,
  updated_at   timestamptz not null default now()
);

-- Outbox of notifications, written in the transaction that observed the event
-- and delivered by workers. dedupe_key makes enqueueing idempotent per channel.
create table alert_deliveries (
  id              uuid        primary key,
  org_id          uuid        not null,
  channel_id      uuid        not null references alert_channels(id) on delete cascade,
  event           text        not null,
  dedupe_key      text        not null,
  payload         jsonb       not null,
  status          text        not null default 'pending' check (status in ('pending', 'sent', 'failed')),
  attempts        integer     not null default 0,
  next_attempt_at timestamptz not null default now(),
  last_error      text,
  created_at      timestamptz not null default now(),
  sent_at         timestamptz,
  unique (channel_id, dedupe_key)
);
create index alert_deliveries_due_ix on alert_deliveries (next_attempt_at) where status = 'pending';
create index alert_deliveries_org_ix on alert_deliveries (org_id, created_at desc);

-- +goose Down
drop table alert_deliveries;
drop table workflow_health;
drop table alert_channels;
drop table schedules;
drop index wfv_test_hash_ix;
delete from workflow_versions where is_test
  and not exists (select 1 from runs r where r.workflow_version_id = workflow_versions.id);
alter table workflow_versions add constraint wfv_version_positive check (version > 0) not valid;
alter table workflow_versions drop constraint wfv_test_version_ck;
alter table workflow_versions drop column is_test;
