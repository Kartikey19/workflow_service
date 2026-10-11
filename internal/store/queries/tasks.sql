-- name: InsertTask :exec
insert into tasks (id, tenant_id, run_id, node_execution_id, wait_id, node_id, queue, form, assignee, payload, due_at)
values (@id, @tenant_id, @run_id, @node_execution_id, @wait_id, @node_id, @queue,
        sqlc.narg(form), sqlc.narg(assignee), sqlc.narg(payload), sqlc.narg(due_at));

-- name: GetTaskDetail :one
select * from task_details where id = @id and tenant_id = @tenant_id;

-- name: ListTaskDetails :many
-- Keyset paginated, newest first. The status filter uses the derived status.
select * from task_details
 where tenant_id = @tenant_id
   and (sqlc.narg(queue)::text is null or queue = sqlc.narg(queue))
   and (sqlc.narg(status)::text is null or status = sqlc.narg(status))
   and (sqlc.narg(assignee)::text is null or assignee = sqlc.narg(assignee))
   and (sqlc.narg(claimed_by)::text is null or claimed_by = sqlc.narg(claimed_by))
   and (sqlc.narg(before_created)::timestamptz is null
        or (created_at, id) < (sqlc.narg(before_created)::timestamptz, sqlc.narg(before_id)::uuid))
 order by created_at desc, id desc
 limit @lim;

-- name: ClaimTask :execrows
-- Only a live, unclaimed task can be claimed.
update tasks t
   set claimed_by = @actor, claimed_at = now()
 where t.id = @id and t.tenant_id = @tenant_id and t.claimed_by is null
   and exists (select 1 from run_waits w where w.id = t.wait_id and w.status = 'waiting');

-- name: ReleaseTask :execrows
-- The claimer, or an administrator (force), gives the task back.
update tasks t
   set claimed_by = null, claimed_at = null
 where t.id = @id and t.tenant_id = @tenant_id and t.claimed_by is not null
   and (t.claimed_by = @actor or @force::bool)
   and exists (select 1 from run_waits w where w.id = t.wait_id and w.status = 'waiting');

-- name: SetTaskCompletedBy :exec
update tasks set completed_by = @actor where id = @id and tenant_id = @tenant_id;
