-- +goose Up
create table triggers (
 id uuid primary key,
 org_id uuid not null,
 name text not null,
 workflow text not null,
 kind text not null default 'webhook' check (kind = 'webhook'),
 token_hash bytea not null unique,
 input_mode text not null default 'body' check (input_mode in ('body','envelope')),
 enabled boolean not null default true,
 secret_ciphertext bytea,
 secret_nonce bytea,
 secret_key_version integer,
 created_at timestamptz not null default now(),
 updated_at timestamptz not null default now(),
 unique(org_id,name)
);

-- +goose Down
drop table triggers;
