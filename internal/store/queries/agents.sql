-- name: CreateProviderConnection :one
insert into provider_connections (id, org_id, name, provider, base_url, key_ciphertext, key_nonce, key_version, key_hint, created_by)
values (@id, @org_id, @name, @provider, sqlc.narg(base_url), sqlc.narg(key_ciphertext), sqlc.narg(key_nonce),
        sqlc.narg(key_version), sqlc.narg(key_hint), sqlc.narg(created_by))
on conflict (org_id, name) do nothing
returning *;

-- name: GetProviderConnection :one
select * from provider_connections where id = @id and org_id = @org_id;

-- name: GetProviderConnectionByName :one
select * from provider_connections where org_id = @org_id and name = @name;

-- name: ListProviderConnections :many
select id, org_id, name, provider, base_url, key_hint, created_at, updated_at, created_by,
       (select count(*) from agents a where a.connection_id = provider_connections.id) as agent_count
  from provider_connections where provider_connections.org_id = @org_id order by provider_connections.name;

-- name: DeleteProviderConnection :execrows
delete from provider_connections where id = @id and org_id = @org_id;

-- name: CreateAgent :one
insert into agents (id, org_id, name, description, connection_id, model, system_prompt, input_schema, output_schema,
                    tools, max_iterations, max_tokens, temperature, updated_by)
values (@id, @org_id, @name, @description, sqlc.narg(connection_id), @model, @system_prompt, sqlc.narg(input_schema),
        sqlc.narg(output_schema), @tools::text[], @max_iterations, sqlc.narg(max_tokens),
        sqlc.narg(temperature), sqlc.narg(updated_by))
on conflict (org_id, name) do nothing
returning *;

-- name: UpdateAgent :one
update agents
   set description = @description, connection_id = sqlc.narg(connection_id), model = @model, system_prompt = @system_prompt,
       input_schema = sqlc.narg(input_schema), output_schema = sqlc.narg(output_schema),
       tools = @tools::text[], max_iterations = @max_iterations,
       max_tokens = sqlc.narg(max_tokens), temperature = sqlc.narg(temperature),
       revision = revision + 1, updated_at = now(), updated_by = sqlc.narg(updated_by)
 where org_id = @org_id and name = @name
returning *;

-- name: GetAgentByName :one
-- The connection's name rides along so the executor can ask for its provider.
select sqlc.embed(a), coalesce(c.name, '')::text as connection_name
  from agents a left join provider_connections c on c.id = a.connection_id
 where a.org_id = @org_id and a.name = @name;

-- name: ListAgents :many
select sqlc.embed(a), coalesce(c.name, '')::text as connection_name
  from agents a left join provider_connections c on c.id = a.connection_id
 where a.org_id = @org_id order by a.name;

-- name: ListAssistantAgents :many
-- Authoring context excludes instructions, descriptions and provider credentials.
select name, input_schema, output_schema from agents where org_id = @org_id order by name;

-- name: ListAssistantProviders :many
select name, provider from provider_connections where org_id = @org_id order by name;

-- name: DeleteAgent :execrows
delete from agents where org_id = @org_id and name = @name;
