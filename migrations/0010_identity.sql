-- +goose Up
-- Identity, organizations and plans (05 §0.1, M11). An organization is the
-- tenant: tenant_id everywhere else is organizations.id.

create table plans (
  id                 text   primary key,
  name               text   not null,
  max_workflows      bigint,   -- null means unlimited
  max_steps          bigint,
  max_runs_per_month bigint
);

-- Starting values; plans are rows, so pricing decisions never need a deploy.
insert into plans (id, name, max_workflows, max_steps, max_runs_per_month) values
  ('free', 'Free',  1,    5,   20),
  ('pro',  'Pro',   50,   50,  10000),
  ('team', 'Team',  null, 200, 100000);

create table users (
  id              uuid        primary key,
  email           text        not null,
  name            text        not null default '',
  password_hash   text        not null,
  platform_admin  boolean     not null default false,
  failed_logins   integer     not null default 0,
  locked_until    timestamptz,
  created_at      timestamptz not null default now(),
  disabled_at     timestamptz,
  constraint users_email_lower_ck check (email = lower(email))
);
create unique index users_email_uq on users (email);

create table organizations (
  id         uuid        primary key,
  name       text        not null,
  slug       text        not null,
  plan_id    text        not null references plans(id) default 'free',
  created_at timestamptz not null default now()
);
create unique index organizations_slug_uq on organizations (slug);

create table memberships (
  org_id     uuid        not null references organizations(id) on delete cascade,
  user_id    uuid        not null references users(id) on delete cascade,
  role       text        not null,
  created_at timestamptz not null default now(),
  primary key (org_id, user_id),
  constraint memberships_role_ck check (role in ('viewer', 'editor', 'admin', 'owner'))
);
create index memberships_user_ix on memberships (user_id);

-- An invite either adds a member to an existing organization (org_id set) or,
-- when a platform admin sends it, creates a new organization on acceptance
-- (org_name set). Tokens are stored as SHA-256 hashes; the link carries the token.
create table invites (
  id          uuid        primary key,
  org_id      uuid        references organizations(id) on delete cascade,
  org_name    text,
  email       text        not null,
  role        text        not null,
  token_hash  text        not null,
  invited_by  uuid        references users(id) on delete set null,
  created_at  timestamptz not null default now(),
  expires_at  timestamptz not null,
  accepted_at timestamptz,
  constraint invites_role_ck check (role in ('viewer', 'editor', 'admin', 'owner')),
  constraint invites_target_ck check ((org_id is null) <> (org_name is null)),
  constraint invites_email_lower_ck check (email = lower(email))
);
create unique index invites_token_uq on invites (token_hash);
create index invites_org_ix on invites (org_id) where accepted_at is null;

create table sessions (
  id           uuid        primary key,
  user_id      uuid        not null references users(id) on delete cascade,
  org_id       uuid        not null references organizations(id) on delete cascade,
  token_hash   text        not null,
  created_at   timestamptz not null default now(),
  expires_at   timestamptz not null,
  last_seen_at timestamptz not null default now()
);
create unique index sessions_token_uq on sessions (token_hash);
create index sessions_user_ix on sessions (user_id);

create table api_keys (
  id           uuid        primary key,
  org_id       uuid        not null references organizations(id) on delete cascade,
  name         text        not null,
  prefix       text        not null,
  token_hash   text        not null,
  role         text        not null,
  created_by   uuid        references users(id) on delete set null,
  created_at   timestamptz not null default now(),
  last_used_at timestamptz,
  revoked_at   timestamptz,
  constraint api_keys_role_ck check (role in ('viewer', 'editor', 'admin'))
);
create unique index api_keys_token_uq on api_keys (token_hash);
create index api_keys_org_ix on api_keys (org_id);

-- Runs started per organization per calendar month (UTC), for plan limits.
create table organization_usage (
  org_id uuid   not null references organizations(id) on delete cascade,
  month  date   not null,
  runs   bigint not null default 0,
  primary key (org_id, month)
);

-- +goose Down
drop table organization_usage;
drop table api_keys;
drop table sessions;
drop table invites;
drop table memberships;
drop table organizations;
drop table users;
drop table plans;
