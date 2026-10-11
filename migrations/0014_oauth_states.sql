-- +goose Up
-- One-time state for an OAuth authorization in flight. The state token is random
-- and stored only as a hash; the row binds the redirect back to the organization
-- and user who started it, and carries the PKCE verifier. It is consumed by the
-- callback and expires after ten minutes.
create table oauth_states (
  state_hash    text        primary key,
  org_id        uuid        not null,
  user_id       uuid,
  kind          text        not null,
  name          text        not null,
  code_verifier text        not null,
  created_at    timestamptz not null default now(),
  expires_at    timestamptz not null
);
create index oauth_states_expiry_ix on oauth_states (expires_at);

-- +goose Down
drop table oauth_states;
