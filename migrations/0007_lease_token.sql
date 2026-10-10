-- +goose Up
-- A fencing token per claim. The worker ID alone cannot fence: a worker that
-- re-claims its own expired row would otherwise let the stale attempt's write
-- land. Every fenced write matches the token handed out by that claim.
alter table node_executions add column lease_token uuid;

-- +goose Down
alter table node_executions drop column lease_token;
