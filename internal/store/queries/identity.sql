-- name: CreateUser :one
insert into users (id, email, name, password_hash, platform_admin)
values (@id, @email, @name, @password_hash, @platform_admin)
returning *;

-- name: GetUserByEmail :one
select * from users where email = @email;

-- name: GetUserByID :one
select * from users where id = @id;

-- name: CountUsers :one
select count(*) from users;

-- name: RecordLoginFailure :exec
-- After max_failures consecutive failures the account locks for lock_for.
update users
   set failed_logins = failed_logins + 1,
       locked_until = case when failed_logins + 1 >= @max_failures::int
                           then now() + @lock_for::interval else locked_until end
 where id = @id;

-- name: ResetLoginFailures :exec
update users set failed_logins = 0, locked_until = null where id = @id;

-- name: CreateOrganization :one
insert into organizations (id, name, slug, plan_id)
values (@id, @name, @slug, @plan_id)
on conflict (slug) do nothing
returning *;

-- name: GetOrganization :one
select * from organizations where id = @id;

-- name: ListOrganizations :many
select o.*, p.name as plan_name,
       (select count(*) from memberships m where m.org_id = o.id) as member_count,
       coalesce((select u.runs from organization_usage u
                  where u.org_id = o.id and u.month = date_trunc('month', now() at time zone 'utc')::date), 0)::bigint as runs_this_month
  from organizations o join plans p on p.id = o.plan_id
 order by o.created_at desc, o.id desc
 limit @lim;

-- name: SetOrganizationPlan :execrows
update organizations set plan_id = @plan_id where id = @id;

-- name: UpsertMembership :exec
insert into memberships (org_id, user_id, role) values (@org_id, @user_id, @role)
on conflict (org_id, user_id) do update set role = excluded.role;

-- name: AddMembership :exec
-- Joining never changes the role of someone who is already a member.
insert into memberships (org_id, user_id, role) values (@org_id, @user_id, @role)
on conflict (org_id, user_id) do nothing;

-- name: GetMembership :one
select * from memberships where org_id = @org_id and user_id = @user_id;

-- name: ListMembershipsForUser :many
select m.role, o.id as org_id, o.name as org_name, o.slug as org_slug
  from memberships m join organizations o on o.id = m.org_id
 where m.user_id = @user_id
 order by o.name, o.id;

-- name: ListMembers :many
select m.role, m.created_at as joined_at, u.id as user_id, u.email, u.name
  from memberships m join users u on u.id = m.user_id
 where m.org_id = @org_id
 order by u.email;

-- name: UpdateMemberRole :execrows
update memberships set role = @role where org_id = @org_id and user_id = @user_id;

-- name: DeleteMembership :execrows
delete from memberships where org_id = @org_id and user_id = @user_id;

-- name: LockOrgMemberships :many
-- Serialises role changes in one organization so the last-owner rule holds.
select user_id, role from memberships where org_id = @org_id for update;

-- name: CreateInvite :one
insert into invites (id, org_id, org_name, email, role, token_hash, invited_by, expires_at)
values (@id, sqlc.narg(org_id), sqlc.narg(org_name), @email, @role, @token_hash, sqlc.narg(invited_by), @expires_at)
returning *;

-- name: LockInviteByTokenHash :one
select * from invites where token_hash = @token_hash for update;

-- name: GetInviteByTokenHash :one
select i.*, o.name as existing_org_name
  from invites i left join organizations o on o.id = i.org_id
 where i.token_hash = @token_hash;

-- name: ListPendingInvites :many
select * from invites
 where org_id = @org_id and accepted_at is null and expires_at > now()
 order by created_at desc;

-- name: DeleteInvite :execrows
delete from invites where id = @id and org_id = @org_id and accepted_at is null;

-- name: MarkInviteAccepted :exec
update invites set accepted_at = now() where id = @id;

-- name: CreateSession :exec
insert into sessions (id, user_id, org_id, token_hash, expires_at)
values (@id, @user_id, @org_id, @token_hash, @expires_at);

-- name: GetSessionAuth :one
-- One lookup per request: the session, its user and the user's role in the
-- session's organization (null when the membership was removed).
select s.id as session_id, s.org_id, s.expires_at, s.last_seen_at,
       u.id as user_id, u.email, u.name, u.platform_admin, u.disabled_at,
       m.role as role
  from sessions s
  join users u on u.id = s.user_id
  left join memberships m on m.org_id = s.org_id and m.user_id = s.user_id
 where s.token_hash = @token_hash and s.expires_at > now();

-- name: TouchSession :exec
update sessions set last_seen_at = now(), expires_at = @expires_at where id = @id;

-- name: SetSessionOrg :execrows
update sessions set org_id = @org_id where id = @id and user_id = @user_id;

-- name: DeleteSession :exec
delete from sessions where id = @id;

-- name: DeleteUserSessions :exec
delete from sessions where user_id = @user_id;

-- name: DeleteExpiredSessions :execrows
delete from sessions where expires_at <= now();

-- name: CreateAPIKey :one
insert into api_keys (id, org_id, name, prefix, token_hash, role, created_by)
values (@id, @org_id, @name, @prefix, @token_hash, @role, sqlc.narg(created_by))
returning *;

-- name: GetAPIKeyAuth :one
select k.id, k.org_id, k.name, k.role, k.last_used_at
  from api_keys k join organizations o on o.id = k.org_id
 where k.token_hash = @token_hash and k.revoked_at is null;

-- name: TouchAPIKey :exec
-- At most once a minute per key, so reads do not turn into writes.
update api_keys set last_used_at = now()
 where id = @id and (last_used_at is null or last_used_at < now() - interval '1 minute');

-- name: ListAPIKeys :many
select id, name, prefix, role, created_at, last_used_at, revoked_at
  from api_keys where org_id = @org_id and revoked_at is null
 order by created_at desc;

-- name: RevokeAPIKey :execrows
update api_keys set revoked_at = now() where id = @id and org_id = @org_id and revoked_at is null;

-- name: GetOrgPlan :one
select p.* from plans p join organizations o on o.plan_id = p.id where o.id = @org_id;

-- name: ListPlans :many
select * from plans order by coalesce(max_runs_per_month, 9223372036854775807), id;

-- name: ReserveRun :one
-- Counts one run against the current month. With a limit, the update only
-- applies while runs < limit, so concurrent starts cannot overshoot it: no row
-- comes back once the allowance is used up.
insert into organization_usage (org_id, month, runs)
values (@org_id, date_trunc('month', now() at time zone 'utc')::date, 1)
on conflict (org_id, month) do update set runs = organization_usage.runs + 1
 where @max_runs::bigint <= 0 or organization_usage.runs < @max_runs::bigint
returning runs;

-- name: RunsThisMonth :one
select coalesce((select runs from organization_usage
                  where org_id = @org_id and month = date_trunc('month', now() at time zone 'utc')::date), 0)::bigint;
