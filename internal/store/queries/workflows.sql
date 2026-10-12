-- name: GetWorkflowVersion :one
select wv.*
from workflow_versions wv
join workflows w on w.id = wv.workflow_id
where wv.id = @id
  and w.tenant_id = @tenant_id;

-- name: EnsureWorkflow :exec
insert into workflows (id, tenant_id, name, description)
values (@id, @tenant_id, @name, sqlc.narg(description))
on conflict (tenant_id, name) do nothing;

-- name: LockWorkflowByName :one
-- Serialises concurrent publishes of one workflow (02 §8).
select * from workflows where tenant_id = @tenant_id and name = @name for update;

-- name: TouchWorkflow :exec
update workflows
   set description = sqlc.narg(description), updated_at = now()
 where id = @id;

-- name: LatestWorkflowVersion :one
-- The latest published version; draft test versions are never "latest".
select * from workflow_versions
 where workflow_id = @workflow_id
   and not is_test
 order by version desc
 limit 1;

-- name: InsertWorkflowVersion :one
insert into workflow_versions (id, workflow_id, version, yaml_source, spec, spec_hash, created_by)
select @id, @workflow_id, coalesce(max(version), 0) + 1, @yaml_source, @spec, @spec_hash, sqlc.narg(created_by)
  from workflow_versions
 where workflow_id = @workflow_id
   and not is_test
returning *;

-- name: FindTestVersion :one
-- Reuses the test version of an identical draft instead of adding another.
select * from workflow_versions
 where workflow_id = @workflow_id and is_test and spec_hash = @spec_hash
 order by version desc
 limit 1;

-- name: InsertTestVersion :one
-- Test versions count down from -1; the caller holds the workflow row lock.
insert into workflow_versions (id, workflow_id, version, yaml_source, spec, spec_hash, created_by, is_test)
select @id, @workflow_id, coalesce(min(version), 0) - 1, @yaml_source, @spec, @spec_hash, sqlc.narg(created_by), true
  from workflow_versions
 where workflow_id = @workflow_id
   and is_test
returning *;

-- name: GetWorkflowByName :one
select * from workflows where tenant_id = @tenant_id and name = @name;

-- name: ListWorkflows :many
select sqlc.embed(w),
       coalesce(v.version, 0)::int as latest_version,
       v.created_at as latest_published_at,
       (select count(*) from runs r where r.workflow_id = w.id)::bigint as run_count,
       (select max(r.created_at) from runs r where r.workflow_id = w.id)::timestamptz as last_run_at
  from workflows w
  -- A workflow that has only been test-run has no published version yet.
  join lateral (
       select version, created_at from workflow_versions
        where workflow_id = w.id and not is_test order by version desc limit 1
  ) v on true
 where w.tenant_id = @tenant_id
   and (@include_archived::bool or w.archived_at is null)
 order by w.name;

-- name: ListWorkflowVersions :many
select id, workflow_id, version, spec_hash, created_by, created_at
  from workflow_versions
 where workflow_id = @workflow_id
   and not is_test
 order by version desc;

-- name: GetWorkflowVersionByNumber :one
select * from workflow_versions where workflow_id = @workflow_id and version = @version;

-- name: SetWorkflowArchived :execrows
update workflows
   set archived_at = case when @archived::bool then now() else null end,
       updated_at = now()
 where tenant_id = @tenant_id and name = @name;

-- name: CountActiveWorkflows :one
select count(*) from workflows w
 where w.tenant_id = @tenant_id and w.archived_at is null
   and exists (select 1 from workflow_versions v where v.workflow_id = w.id and not v.is_test);

-- name: GetWorkflowName :one
select name from workflows where id = @id;
