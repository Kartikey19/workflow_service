-- +goose Up
-- Connections to external services (05 §3.1, M14): SMTP and Resend for email,
-- Gmail, Google Drive, Dropbox and OneDrive through OAuth, and PostgreSQL or MySQL
-- databases. config holds the non-secret settings (host, from address); every
-- credential (password, API key, OAuth tokens) is one JSON object sealed with the
-- vault master key and bound to the organization and connection name. Credentials
-- are write-only: the API shows only a hint and, for OAuth, the connected account.
create table connections (
  id                 uuid        primary key,
  org_id             uuid        not null,
  name               text        not null,
  kind               text        not null,
  config             jsonb       not null default '{}',
  secret_ciphertext  bytea,
  secret_nonce       bytea,
  secret_key_version integer,
  secret_hint        text,
  account            text,
  token_expires_at   timestamptz,
  created_at         timestamptz not null default now(),
  updated_at         timestamptz not null default now(),
  created_by         text,
  constraint connections_kind_ck check (kind in
    ('smtp', 'resend', 'gmail', 'gdrive', 'dropbox', 'onedrive', 'postgres', 'mysql'))
);
create unique index connections_org_name_uq on connections (org_id, name);

-- +goose Down
drop table connections;
