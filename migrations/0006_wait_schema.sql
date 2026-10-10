-- +goose Up
-- wait_event `schema`: a signal payload that fails it is rejected with 422.
alter table run_waits add column schema jsonb;

-- +goose Down
alter table run_waits drop column schema;
