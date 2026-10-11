-- +goose Up
-- An agent's tools are now the connectors it may search (glob, grep, read) and
-- mcp:<server> references. Rewrite older definitions: a per-action reference
-- connector:<name>:<action> becomes <name>, the bound database_connection joins
-- the list, and the retired built-in tools (http_request, current_time and the
-- database tools) are dropped. allowed_hosts and database_connection are no
-- longer read; they stay as columns so this migration is reversible in data.
update agents set tools = coalesce((
  select array_agg(distinct name order by name) from (
    select case when ref like 'connector:%' then split_part(ref, ':', 2) else ref end as name
      from unnest(agents.tools) as ref
     where ref not in ('http_request', 'current_time', 'list_tables', 'describe_table', 'run_query')
    union
    select agents.database_connection where agents.database_connection <> ''
  ) as refs
), '{}');

-- +goose Down
-- The rewrite is lossy (which actions a connector was limited to); nothing to undo.
select 1;
