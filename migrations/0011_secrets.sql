-- +goose Up
-- The vault (05 §0.1, M12). Values are AES-256-GCM ciphertexts; additional
-- authenticated data binds each one to its organization and name. key_version
-- says which master key sealed it, so keys rotate without downtime: a new key
-- becomes current while the old one stays loaded, and rows are re-sealed in the
-- background until none carries the old version.
create table secrets (
  id          uuid        primary key,
  org_id      uuid        not null,
  name        text        not null,
  ciphertext  bytea       not null,
  nonce       bytea       not null,
  key_version integer     not null,
  created_at  timestamptz not null default now(),
  updated_at  timestamptz not null default now(),
  updated_by  text
);
create unique index secrets_org_name_uq on secrets (org_id, name);
create index secrets_key_version_ix on secrets (key_version, id);

-- +goose Down
drop table secrets;
