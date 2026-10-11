-- +goose Up
-- The human_task read model (02 §4.7). The authoritative state is the run_waits
-- row the task waits on: completing a task is delivering that wait's signal, and
-- an SLA timeout is the wait's normal timeout path. This table only adds what a
-- worklist needs (queue, form, assignee, claim) and is written in the same
-- transaction that registers the wait.
create table tasks (
  id                uuid        primary key,
  tenant_id         uuid        not null,
  run_id            uuid        not null references runs(id) on delete cascade,
  node_execution_id uuid        not null references node_executions(id) on delete cascade,
  wait_id           uuid        not null references run_waits(id) on delete cascade,
  node_id           text        not null,
  queue             text        not null,
  form              text,
  assignee          text,
  payload           jsonb,
  due_at            timestamptz,
  claimed_by        text,
  claimed_at        timestamptz,
  completed_by      text,
  created_at        timestamptz not null default now()
);

create unique index tasks_wait_uq on tasks (wait_id);
create index tasks_queue_ix on tasks (tenant_id, queue, created_at desc, id desc);
create index tasks_run_ix on tasks (run_id);

-- Status is derived, never stored, so a timeout or a cancelled run cannot leave
-- a task looking open: open | claimed | completed | expired | cancelled.
create view task_details as
select t.*,
       case w.status
         when 'waiting'   then case when t.claimed_by is null then 'open' else 'claimed' end
         when 'fulfilled' then 'completed'
         when 'expired'   then 'expired'
         else 'cancelled'
       end as status,
       w.wait_key as wait_key,
       wf.name    as workflow_name
  from tasks t
  join run_waits w on w.id = t.wait_id
  join runs r      on r.id = t.run_id
  join workflows wf on wf.id = r.workflow_id;

-- +goose Down
drop view task_details;
drop table tasks;
