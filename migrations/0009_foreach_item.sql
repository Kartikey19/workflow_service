-- +goose Up
-- A foreach branch is a node_executions row of the foreach node with
-- branch_key = its list index. The element it works on is stored on the row, not
-- recomputed from the controller, so a retry of the branch (a new row, attempt+1)
-- sees exactly the element the first attempt did.
alter table node_executions add column item jsonb;

-- +goose Down
alter table node_executions drop column item;
