-- name: UpsertSecret :exec
insert into secrets (id, org_id, name, ciphertext, nonce, key_version, updated_by)
values (@id, @org_id, @name, @ciphertext, @nonce, @key_version, sqlc.narg(updated_by))
on conflict (org_id, name) do update
   set ciphertext = excluded.ciphertext, nonce = excluded.nonce, key_version = excluded.key_version,
       updated_at = now(), updated_by = excluded.updated_by;

-- name: ListSecretNames :many
-- Names and metadata only: values are write-only through the API.
select name, key_version, created_at, updated_at, updated_by
  from secrets where org_id = @org_id order by name;

-- name: DeleteSecret :execrows
delete from secrets where org_id = @org_id and name = @name;

-- name: ListSecretsForOrg :many
select id, name, ciphertext, nonce, key_version from secrets where org_id = @org_id;

-- name: ListSecretsToRotate :many
-- Rows not sealed under the current key, in id order for keyset paging.
select id, org_id, name, ciphertext, nonce, key_version
  from secrets
 where key_version <> @current_version and id > @after_id
 order by id
 limit @lim;

-- name: RotateSecret :execrows
-- Compare-and-set on the old version, so a concurrent overwrite is never undone.
update secrets
   set ciphertext = @ciphertext, nonce = @nonce, key_version = @new_version
 where id = @id and key_version = @old_version;

-- name: CountSecretsByKeyVersion :many
select key_version, count(*) as total from secrets group by key_version order by key_version;
