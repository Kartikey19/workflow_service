-- name: CreateSchedule :one
insert into schedules (id, org_id, name, workflow, cron, timezone, input, overlap, catch_up, enabled, next_run_at, created_by)
values (@id, @org_id, @name, @workflow, @cron, @timezone, @input, @overlap, @catch_up, @enabled, sqlc.narg(next_run_at), sqlc.narg(created_by))
on conflict (org_id, name) do nothing
returning *;

-- name: ListSchedules :many
select * from schedules
 where org_id = @org_id
   and (sqlc.narg(workflow)::text is null or workflow = sqlc.narg(workflow)::text)
 order by name;

-- name: GetSchedule :one
select * from schedules where org_id = @org_id and id = @id;

-- name: UpdateSchedule :one
update schedules
   set cron = @cron, timezone = @timezone, input = @input, overlap = @overlap,
       catch_up = @catch_up, enabled = @enabled, next_run_at = sqlc.narg(next_run_at),
       updated_at = now()
 where org_id = @org_id and id = @id
returning *;

-- name: DeleteSchedule :execrows
delete from schedules where org_id = @org_id and id = @id;

-- name: ClaimDueSchedules :many
-- Each worker locks a disjoint set of due schedules; a slow start on one worker
-- never blocks another (03 §8). The lock is held until the slot is recorded.
select * from schedules
 where enabled and next_run_at <= now()
 order by next_run_at
 for update skip locked
 limit @lim;

-- name: RecordScheduleSlot :exec
update schedules
   set next_run_at = @next_run_at, last_slot_at = @last_slot_at,
       last_run_id = coalesce(sqlc.narg(last_run_id), last_run_id),
       last_outcome = @last_outcome::text, last_error = sqlc.narg(last_error), updated_at = now()
 where id = @id;

-- name: RecordScheduleRun :exec
-- A manual "run now" is reported like a slot without moving the schedule.
update schedules
   set last_run_id = @last_run_id, last_outcome = 'started', last_error = null, updated_at = now()
 where org_id = @org_id and id = @id;
