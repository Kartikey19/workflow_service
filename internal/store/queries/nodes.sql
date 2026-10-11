-- name: ClaimReady :many
update node_executions ne
   set status           = 'running',
       lease_owner      = @worker_id,
       lease_token      = gen_random_uuid(),
       lease_expires_at = now() + @lease_ttl::interval,
       started_at       = coalesce(ne.started_at, now())
 where ne.id in (
       select id
         from node_executions
        where status = 'ready'
          and run_after <= now()
        order by run_after
        for update skip locked
        limit @batch_size
 )
returning ne.*;

-- name: ExtendLease :execrows
-- A cancelled or finished run stops extending, which cancels the attempt (03 §10).
update node_executions ne
   set lease_expires_at = now() + @lease_ttl::interval
 where ne.id = @id
   and ne.lease_owner = @worker_id
   and ne.lease_token = @lease_token
   and ne.status = 'running'
   and ne.lease_expires_at > now()
   and exists (
       select 1 from runs r
        where r.id = ne.run_id
          and r.status in ('pending', 'running', 'waiting')
   );

-- name: CompleteNode :execrows
update node_executions
   set status           = @status,
       output           = @output,
       error            = @error,
       metrics          = @metrics,
       ended_at         = now(),
       lease_owner      = null,
       lease_token      = null,
       lease_expires_at = null
 where id = @id
   and status = 'running'
   and lease_owner = @worker_id
   and lease_token = @lease_token
   and lease_expires_at > now();

-- name: EnqueueNode :one
-- A skipped node is inserted already terminal, so it carries ended_at.
insert into node_executions (
  id, run_id, tenant_id, node_id, node_type, branch_key, attempt,
  status, input, idempotency_key, run_after, ended_at
) values (
  @id, @run_id, @tenant_id, @node_id, @node_type, @branch_key, 1,
  @status::text, @input, @idempotency_key, coalesce(sqlc.narg(run_after)::timestamptz, now()),
  case when @status::text = 'skipped' then now() end
)
on conflict (run_id, node_id, branch_key, attempt) do nothing
returning *;

-- name: DepStates :many
select node_id, status
  from node_executions ne
 where ne.run_id = @run_id
   and ne.branch_key = @branch_key
   and ne.node_id = any(@dep_node_ids::text[])
   and ne.attempt = (
       select max(attempt)
         from node_executions ne2
        where ne2.run_id = ne.run_id
          and ne2.node_id = ne.node_id
          and ne2.branch_key = ne.branch_key
   );

-- name: ReclaimExpiredLeases :many
update node_executions
   set status = 'ready', lease_owner = null, lease_token = null, lease_expires_at = null
 where id in (
   select id
     from node_executions
    where status = 'running' and lease_expires_at < now()
    order by lease_expires_at
    for update skip locked
    limit 100
 )
returning id, run_id, node_id, attempt;

-- name: SetNodeInput :execrows
update node_executions
   set input = @input
 where id = @id
   and status = 'running'
   and lease_owner = @worker_id
   and lease_token = @lease_token
   and lease_expires_at > now();

-- name: LatestAttemptsForRun :many
-- Latest attempt per node, kept only when that attempt is terminal; a node whose
-- newest row is a pending retry is not visible to expressions yet.
select node_id, status, attempt, output, metrics
  from (
       select distinct on (node_id) node_id, status, attempt, output, metrics
         from node_executions
        where run_id = @run_id
          and branch_key = @branch_key
        order by node_id, attempt desc
  ) latest
 where status in ('succeeded', 'failed', 'skipped');

-- name: EnqueueRetry :execrows
-- A retry is a new row with the same idempotency key (I5), never a mutation.
insert into node_executions (
  id, run_id, tenant_id, node_id, node_type, branch_key, attempt,
  status, idempotency_key, run_after
) values (
  @id, @run_id, @tenant_id, @node_id, @node_type, @branch_key, @attempt,
  'ready', @idempotency_key, @run_after
)
on conflict (run_id, node_id, branch_key, attempt) do nothing;

-- name: ParkNode :execrows
-- sleep/wait_event: release the lease and wake at run_after (03 §7.2). Fenced.
update node_executions
   set status           = 'waiting',
       run_after        = @run_after,
       output           = @output,
       metrics          = @metrics,
       lease_owner      = null,
       lease_token      = null,
       lease_expires_at = null
 where id = @id
   and status = 'running'
   and lease_owner = @worker_id
   and lease_token = @lease_token
   and lease_expires_at > now();

-- name: CompleteWaitingNode :execrows
-- The one unfenced terminal write: a waiting node holds no lease by construction.
update node_executions
   set status   = @status,
       output   = coalesce(sqlc.narg(output), output),
       error    = @error,
       ended_at = now()
 where id = @id
   and status = 'waiting';

-- name: DueWaitingNodes :many
-- Unlocked read: each node is then handled under its run lock (run → nodes order).
select *
  from node_executions
 where status = 'waiting'
   and run_after <= now()
 order by run_after
 limit 100;

-- name: ReleaseWorkerLeases :many
-- Graceful shutdown: hand running nodes back immediately instead of after a TTL.
update node_executions
   set status = 'ready', lease_owner = null, lease_token = null, lease_expires_at = null
 where lease_owner = @worker_id
   and status = 'running'
returning id;

-- name: LatestNodeStates :many
-- Latest attempt per node; "latest attempt wins" in one place (03 §5.4).
select distinct on (node_id, branch_key)
       node_id, branch_key, status, attempt, error
  from node_executions
 where run_id = @run_id
 order by node_id, branch_key, attempt desc;

-- name: CancelRunNodes :many
update node_executions
   set status = 'cancelled', ended_at = now(),
       lease_owner = null, lease_token = null, lease_expires_at = null
 where run_id = @run_id
   and status in ('ready', 'waiting')
returning id;

-- name: CancelRunningNode :execrows
-- The run ended while this attempt ran; record that the result was not used.
update node_executions
   set status = 'cancelled', ended_at = now(), metrics = @metrics,
       lease_owner = null, lease_token = null, lease_expires_at = null
 where id = @id
   and status = 'running'
   and lease_owner = @worker_id
   and lease_token = @lease_token;

-- name: CountRunNodeExecutions :one
select count(*) from node_executions where run_id = @run_id;

-- name: ListRunNodes :many
select *
  from node_executions
 where run_id = @run_id
   and tenant_id = @tenant_id
 order by created_at, attempt;

-- name: GetNodeExecution :one
select * from node_executions where id = @id;

-- name: AdminStopNode :one
-- Operator intervention: take a live node out of play as cancelled or skipped.
-- Only non-terminal rows change, and clearing the lease token makes the fenced
-- writes of any worker still holding the old lease affect zero rows.
update node_executions
   set status = @status::text, ended_at = now(),
       lease_owner = null, lease_token = null, lease_expires_at = null
 where id = @id
   and status in ('ready', 'running', 'waiting')
returning *;
