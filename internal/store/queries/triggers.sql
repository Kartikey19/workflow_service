-- name: CreateTrigger :one
insert into triggers (id,org_id,name,workflow,token_hash,input_mode,enabled,secret_ciphertext,secret_nonce,secret_key_version)
values (@id,@org_id,@name,@workflow,@token_hash,@input_mode,@enabled,sqlc.narg(secret_ciphertext),sqlc.narg(secret_nonce),sqlc.narg(secret_key_version))
on conflict (org_id,name) do nothing returning *;

-- name: ListTriggers :many
select * from triggers where org_id= @org_id order by name;

-- name: GetTrigger :one
select * from triggers where org_id= @org_id and id= @id;

-- name: GetTriggerByToken :one
select * from triggers where token_hash= @token_hash and enabled;

-- name: UpdateTrigger :one
update triggers set enabled= @enabled, input_mode= @input_mode, updated_at=now()
where org_id= @org_id and id= @id returning *;

-- name: DeleteTrigger :execrows
delete from triggers where org_id= @org_id and id= @id;

-- name: RotateTriggerToken :execrows
update triggers set token_hash= @token_hash,updated_at=now() where org_id= @org_id and id= @id;

-- name: ListTriggerSecretsToRotate :many
select id,org_id,secret_ciphertext,secret_nonce,secret_key_version from triggers
where secret_key_version is not null and secret_key_version <> @current_version and id > @after_id
order by id limit @lim;

-- name: RotateTriggerSecret :execrows
update triggers set secret_ciphertext= @ciphertext,secret_nonce= @nonce,secret_key_version= @new_version
where id= @id and secret_key_version= @old_version;
