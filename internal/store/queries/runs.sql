-- name: GetRun :one
select *
from runs
where id = @id
  and tenant_id = @tenant_id;

-- name: CreateRun :one
-- ON CONFLICT returns no row: the caller then loads the existing run (04 §3.1).
insert into runs (
  id, tenant_id, workflow_id, workflow_version_id, status, input,
  idempotency_key, correlation_id, trigger, parent_run_id,
  started_at, deadline_at, request_hash, created_by
) values (
  @id, @tenant_id, @workflow_id, @workflow_version_id, 'running', @input,
  sqlc.narg(idempotency_key), sqlc.narg(correlation_id), @trigger, sqlc.narg(parent_run_id),
  now(), @deadline_at, sqlc.narg(request_hash), sqlc.narg(created_by)
)
on conflict (tenant_id, idempotency_key) where idempotency_key is not null do nothing
returning *;

-- name: GetRunByIdempotencyKey :one
select * from runs where tenant_id = @tenant_id and idempotency_key = @idempotency_key;

-- name: LockRun :one
-- Serialises DAG advancement, run completion and event seq allocation (03 §5.4).
select * from runs where id = @id for update;

-- name: SetRunStatus :exec
update runs
   set status = @status
 where id = @id
   and status in ('pending', 'running', 'waiting');

-- name: FinishRun :execrows
update runs
   set status = @status, output = @output, error = @error, ended_at = now()
 where id = @id
   and status in ('pending', 'running', 'waiting');

-- name: AppendEvent :exec
-- Gap-free per-run seq (I6); callers hold the run row lock.
insert into run_events (run_id, tenant_id, seq, type, node_id, branch_key, attempt, payload)
select @run_id::uuid, @tenant_id::uuid, coalesce(max(seq), 0) + 1, @type::text,
       sqlc.narg(node_id)::text, sqlc.narg(branch_key)::text, sqlc.narg(attempt)::int,
       @payload::jsonb
  from run_events
 where run_id = @run_id::uuid;

-- name: ListRunEvents :many
select * from run_events
 where run_id = @run_id and tenant_id = @tenant_id and seq > @after_seq
 order by seq
 limit @lim;

-- name: GetRunDetail :one
select sqlc.embed(r), w.name as workflow_name, wv.version as workflow_version
  from runs r
  join workflows w on w.id = r.workflow_id
  join workflow_versions wv on wv.id = r.workflow_version_id
 where r.id = @id and r.tenant_id = @tenant_id;

-- name: ListRuns :many
-- Keyset pagination on (created_at, id) descending (04 §1).
select sqlc.embed(r), w.name as workflow_name, wv.version as workflow_version
  from runs r
  join workflows w on w.id = r.workflow_id
  join workflow_versions wv on wv.id = r.workflow_version_id
 where r.tenant_id = @tenant_id
   and (sqlc.narg(workflow_name)::text is null or w.name = sqlc.narg(workflow_name)::text)
   and (sqlc.narg(status)::text is null or r.status = sqlc.narg(status)::text)
   and (sqlc.narg(correlation_id)::text is null or r.correlation_id = sqlc.narg(correlation_id)::text)
   and (sqlc.narg(before_created)::timestamptz is null
        or (r.created_at, r.id) < (sqlc.narg(before_created)::timestamptz, sqlc.narg(before_id)::uuid))
 order by r.created_at desc, r.id desc
 limit @lim;

-- name: CountRunsSince :one
select count(*) from runs where tenant_id = @tenant_id and created_at >= @since;

-- name: RunStatusCounts :many
select status, count(*) as count
  from runs
 where tenant_id = @tenant_id and created_at >= @since
 group by status;

-- name: CancelRun :execrows
update runs
   set status = 'cancelled', ended_at = now(),
       error = jsonb_build_object('code', 'cancelled', 'message', @reason::text)
 where id = @id
   and tenant_id = @tenant_id
   and status in ('pending', 'running', 'waiting');

-- name: RunsPastDeadline :many
select id, tenant_id
  from runs
 where status in ('running', 'waiting')
   and deadline_at <= now()
 order by deadline_at
 limit 100;

-- name: PeekLiveWait :one
-- Unlocked lookup of the run to lock first; LockLiveWait re-checks under the lock.
select run_id from run_waits
 where tenant_id = @tenant_id and wait_key = @wait_key and status = 'waiting';

-- name: NotifyReady :exec
-- Wakes idle workers at commit; polling remains the correctness guarantee.
select pg_notify('wf_ready', @run_id::text);

-- name: NotifyRunEvents :exec
select pg_notify('wf_events', @run_id::text);
