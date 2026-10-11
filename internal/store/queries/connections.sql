-- name: CreateConnection :one
insert into connections (id, org_id, name, kind, config, secret_ciphertext, secret_nonce, secret_key_version,
                         secret_hint, account, token_expires_at, created_by)
values (@id, @org_id, @name, @kind, @config, sqlc.narg(secret_ciphertext), sqlc.narg(secret_nonce),
        sqlc.narg(secret_key_version), sqlc.narg(secret_hint), sqlc.narg(account), sqlc.narg(token_expires_at),
        sqlc.narg(created_by))
on conflict (org_id, name) do nothing
returning *;

-- name: GetConnection :one
select * from connections where org_id = @org_id and name = @name;

-- name: ListConnections :many
select id, org_id, name, kind, config, secret_hint, account, token_expires_at, created_at, updated_at, created_by
  from connections where org_id = @org_id order by name;

-- name: UpdateConnection :one
-- Credentials are replaced only when new ones are given (the narg values are not null).
update connections
   set config = @config,
       secret_ciphertext  = coalesce(sqlc.narg(secret_ciphertext), secret_ciphertext),
       secret_nonce       = coalesce(sqlc.narg(secret_nonce), secret_nonce),
       secret_key_version = coalesce(sqlc.narg(secret_key_version), secret_key_version),
       secret_hint        = coalesce(sqlc.narg(secret_hint), secret_hint),
       account            = coalesce(sqlc.narg(account), account),
       token_expires_at   = coalesce(sqlc.narg(token_expires_at), token_expires_at),
       updated_at = now()
 where org_id = @org_id and name = @name
returning *;

-- name: StoreConnectionSecret :execrows
-- Used when an OAuth token is refreshed. Matching on the old key version and
-- ciphertext makes a concurrent refresh lose cleanly instead of overwriting.
update connections
   set secret_ciphertext = @ciphertext, secret_nonce = @nonce, secret_key_version = @key_version,
       token_expires_at = sqlc.narg(token_expires_at), updated_at = now()
 where id = @id and secret_nonce = @old_nonce;

-- name: DeleteConnection :execrows
delete from connections where org_id = @org_id and name = @name;

-- name: ListConnectionsToRotate :many
select id, org_id, name, secret_ciphertext, secret_nonce, secret_key_version
  from connections
 where secret_key_version is not null and secret_key_version <> @current_version and id > @after_id
 order by id limit @lim;

-- name: RotateConnectionSecret :execrows
update connections
   set secret_ciphertext = @ciphertext, secret_nonce = @nonce, secret_key_version = @new_version
 where id = @id and secret_key_version = @old_version;

-- name: CreateOAuthState :exec
insert into oauth_states (state_hash, org_id, user_id, kind, name, code_verifier, expires_at)
values (@state_hash, @org_id, sqlc.narg(user_id), @kind, @name, @code_verifier, @expires_at);

-- name: ConsumeOAuthState :one
-- Deleting returns the row once: a replayed callback finds nothing.
delete from oauth_states where state_hash = @state_hash and expires_at > now() returning *;

-- name: DeleteExpiredOAuthStates :execrows
delete from oauth_states where expires_at <= now();
