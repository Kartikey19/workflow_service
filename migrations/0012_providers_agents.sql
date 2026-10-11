-- +goose Up
-- LLM provider connections and reusable agent definitions (05 §3.1, M13).
--
-- A connection holds an organization's API key for one provider, encrypted with
-- the vault keyring and bound to its organization and name. The key is
-- write-only: only a hint (its last four characters) is ever shown back.
create table provider_connections (
  id             uuid        primary key,
  org_id         uuid        not null,
  name           text        not null,
  provider       text        not null,
  base_url       text,
  key_ciphertext bytea,
  key_nonce      bytea,
  key_version    integer,
  key_hint       text,
  created_at     timestamptz not null default now(),
  updated_at     timestamptz not null default now(),
  created_by     text,
  constraint pc_provider_ck check (provider in ('anthropic', 'openai', 'openai_compatible', 'gemini'))
);
create unique index pc_org_name_uq on provider_connections (org_id, name);

-- An agent definition is a named, reusable configuration: which provider
-- connection and model, the system prompt, the shape of its input and output,
-- and which tools it may use. Workflows run it with `type: agent, config:
-- {agent: <name>, input: {...}}`. It is mutable; revision counts the edits and is
-- recorded in each run's node metrics.
create table agents (
  id             uuid             primary key,
  org_id         uuid             not null,
  name           text             not null,
  description    text             not null default '',
  connection_id  uuid             references provider_connections(id) on delete restrict,
  model          text             not null,
  system_prompt  text             not null default '',
  input_schema   jsonb,
  output_schema  jsonb,
  tools          text[]           not null default '{}',
  allowed_hosts  text[]           not null default '{}',
  max_iterations integer          not null,
  max_tokens     integer,
  temperature    double precision,
  revision       integer          not null default 1,
  created_at     timestamptz      not null default now(),
  updated_at     timestamptz      not null default now(),
  updated_by     text
);
create unique index agents_org_name_uq on agents (org_id, name);
create index agents_connection_ix on agents (connection_id);

-- +goose Down
drop table agents;
drop table provider_connections;
