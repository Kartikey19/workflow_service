-- name: CreateAlertChannel :one
insert into alert_channels (id, org_id, name, kind, config, events, workflows, enabled, secret_ciphertext, secret_nonce, secret_key_version)
values (@id, @org_id, @name, @kind, @config, @events, @workflows, @enabled,
        sqlc.narg(secret_ciphertext), sqlc.narg(secret_nonce), sqlc.narg(secret_key_version))
on conflict (org_id, name) do nothing
returning *;

-- name: ListAlertChannels :many
select * from alert_channels where org_id = @org_id order by name;

-- name: GetAlertChannel :one
select * from alert_channels where org_id = @org_id and id = @id;

-- name: UpdateAlertChannel :one
update alert_channels
   set config = @config, events = @events, workflows = @workflows, enabled = @enabled, updated_at = now()
 where org_id = @org_id and id = @id
returning *;

-- name: DeleteAlertChannel :execrows
delete from alert_channels where org_id = @org_id and id = @id;

-- name: ListAlertSecretsToRotate :many
select id, org_id, secret_ciphertext, secret_nonce, secret_key_version from alert_channels
 where secret_key_version is not null and secret_key_version <> @current_version and id > @after_id
 order by id limit @lim;

-- name: RotateAlertSecret :execrows
update alert_channels set secret_ciphertext = @ciphertext, secret_nonce = @nonce, secret_key_version = @new_version
 where id = @id and secret_key_version = @old_version;

-- name: EnqueueAlert :execrows
-- Fans one event out to every enabled channel of the organization that
-- subscribes to it and to the workflow (an empty list means every workflow).
-- Runs inside the transaction that observed the event.
insert into alert_deliveries (id, org_id, channel_id, event, dedupe_key, payload)
select gen_random_uuid(), c.org_id, c.id, @event::text, @dedupe_key::text, @payload::jsonb
  from alert_channels c
 where c.org_id = @org_id
   and c.enabled
   and @event::text = any(c.events)
   and (cardinality(c.workflows) = 0 or @workflow::text = any(c.workflows))
on conflict (channel_id, dedupe_key) do nothing;

-- name: MarkWorkflowFailing :one
-- Opens or extends the workflow's incident and returns whether this failure
-- opened it (failures = 1), which is when an alert goes out.
insert into workflow_health (workflow_id, org_id, failing, failing_since, failures, last_run_id, updated_at)
values (@workflow_id, @org_id, true, now(), 1, @run_id, now())
on conflict (workflow_id) do update
   set failing = true,
       failing_since = case when workflow_health.failing then workflow_health.failing_since else now() end,
       failures = case when workflow_health.failing then workflow_health.failures + 1 else 1 end,
       last_run_id = excluded.last_run_id,
       updated_at = now()
returning failures;

-- name: MarkWorkflowHealthy :one
-- Closes an open incident. Returns the incident's failure count and start, or
-- no row when the workflow was not failing.
update workflow_health h
   set failing = false, failures = 0, last_run_id = @run_id, updated_at = now()
  from (select failures, failing_since from workflow_health where workflow_id = @workflow_id and failing for update) old
 where h.workflow_id = @workflow_id and h.failing
returning old.failures, old.failing_since;

-- name: ClaimAlertDeliveries :many
-- Locks due deliveries for this worker; the lock is held while sending, so a
-- delivery is attempted by one worker at a time.
select d.*, c.kind as channel_kind, c.name as channel_name, c.config as channel_config,
       c.secret_ciphertext, c.secret_nonce, c.secret_key_version
  from alert_deliveries d
  join alert_channels c on c.id = d.channel_id
 where d.status = 'pending' and d.next_attempt_at <= now()
 order by d.next_attempt_at
 for update of d skip locked
 limit @lim;

-- name: FinishAlertDelivery :exec
update alert_deliveries
   set status = @status, attempts = attempts + 1, last_error = sqlc.narg(last_error),
       next_attempt_at = @next_attempt_at,
       sent_at = case when @status::text = 'sent' then now() else sent_at end
 where id = @id;

-- name: ListAlertDeliveries :many
select d.id, d.channel_id, c.name as channel_name, c.kind as channel_kind, d.event, d.payload,
       d.status, d.attempts, d.last_error, d.created_at, d.sent_at, d.next_attempt_at
  from alert_deliveries d
  join alert_channels c on c.id = d.channel_id
 where d.org_id = @org_id
 order by d.created_at desc
 limit @lim;
