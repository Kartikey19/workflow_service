-- name: InsertWait :exec
insert into run_waits (id, tenant_id, run_id, node_execution_id, wait_key, status, expires_at)
values (@id, @tenant_id, @run_id, @node_execution_id, @wait_key, 'waiting', @expires_at);

-- name: LockLiveWait :one
select * from run_waits
 where tenant_id = @tenant_id and wait_key = @wait_key and status = 'waiting'
 for update;

-- name: LatestFulfilledWait :one
-- A duplicate signal returns the original result instead of 404 (04 §4.1).
select * from run_waits
 where tenant_id = @tenant_id and wait_key = @wait_key and status = 'fulfilled'
 order by fulfilled_at desc
 limit 1;

-- name: FulfillWait :exec
update run_waits
   set status = 'fulfilled', payload = @payload, fulfilled_at = now()
 where id = @id;

-- name: ExpireNodeWaits :exec
update run_waits set status = 'expired'
 where node_execution_id = @node_execution_id and status = 'waiting';

-- name: CancelRunWaits :exec
update run_waits set status = 'cancelled'
 where run_id = @run_id and status = 'waiting';
