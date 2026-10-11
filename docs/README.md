# Weft (workflow_service) — design documents

A YAML-configured, durable DAG execution engine in Go. One engine, many domains:
agentic workflows, delivery/logistics orchestration, and long-running healthcare
or compliance processes all run on the same seven tables.

**Stack:** Go 1.27, PostgreSQL 16+. No broker, no scheduler, no leader election.

## Read in order

| # | Document | What's in it |
|---|---|---|
| 00 | [Overview](00-overview.md) | Scope decision, the 7 design principles, architecture, non-goals, milestones |
| 01 | [Data model](01-data-model.md) | Full DDL, state machines, invariants, the query cookbook, **worked row traces** |
| 02 | [Workflow spec](02-workflow-spec.md) | The YAML surface, expressions, every node type, the compiler, validation rules |
| 03 | [Engine runtime](03-engine.md) | Executor interface, claim loop, dispatcher, resolver, retries, leases, sweeper |
| 04 | [HTTP API](04-api.md) | Workflows, runs, signals, tasks and admin endpoints; error model; idempotency semantics |
| 05 | [Implementation plan](05-implementation-plan.md) | Product scope and decisions, repo layout, engine M0–M9 and product M10–M19 with acceptance criteria, testing, ops, hosting |
| 06 | [Operations runbook](06-runbook.md) | Local UI, deployment baseline, releases, backups, restore, observability and key rotation |
| 07 | [Deployment guide](07-deployment.md) | Hosting cost, Neon, GoDaddy DNS, environment separation and CI/CD setup |
| 08 | [Custom code nodes](08-code-nodes.md) | Scope, contract, sandbox options, threat model and milestones for running Python, TypeScript and Go in a workflow |

If you only read two: **01** for the schema and **03 §5** for the resolver. Those
two are where correctness lives; everything else is plumbing around them.

## The one-paragraph version

A user publishes YAML. It compiles once, at publish time, into an immutable
versioned spec. Starting a run inserts one `runs` row and its root
`node_executions` rows. Stateless workers claim ready rows with
`SELECT ... FOR UPDATE SKIP LOCKED`, hand each to a registered `Executor` for its
node type, and — in a single transaction — write the fenced result, append an
event, and enqueue whichever successors just became ready. A node that needs to
wait parks as a row with a `run_after` timestamp, holding no goroutine and no
lease, so a 30-second retry backoff and a 30-day healthcare follow-up cost exactly
the same thing. Domain logic lives entirely behind the `Executor` interface;
control flow that the engine doesn't understand (`branch`, `sleep`, `wait_event`)
arrives as a `Control` directive returned from an executor rather than a `switch`
in the core.

## The load-bearing decisions

1. **Compile at publish, never at runtime** — bad workflows can't reach the engine.
2. **Runs pin to an immutable version** — deploying v2 mid-flight is a non-event.
3. **No shared mutable run state** — expressions read other nodes' immutable outputs.
4. **Postgres is the queue** — `SKIP LOCKED` over a partial index.
5. **At-least-once with fencing** — idempotency keys are *stable across retries*.
6. **Control flow is a return value** — new primitives are new executors.
7. **Every transition is an event** — append-only, gap-free per run.

## Local development

For a small support-ticket YAML example, Cursor debugger setup, and planned
Postman/cURL publish requests, see [the example guide](../examples/README.md).

Use Go from `go.mod` and an existing PostgreSQL 16+ database. The database role
must be able to create tables and schemas. No target starts or restarts Docker.

The server and migration command automatically read `.env` from the current
working directory. Existing environment variables take precedence; a missing
`.env` is fine when configuration is supplied by the environment. From the project
root:

```sh
make migrate                 # apply migrations (goose, embedded SQL)
go run ./cmd/api             # HTTP API
go run ./cmd/worker          # claim loop, dispatcher, sweeper
```

Agent nodes call the platform model provider: set `ANTHROPIC_API_KEY` (and
optionally `ANTHROPIC_BASE_URL` and `LLM_PRICING_JSON`, a JSON object of model
name or family prefix to `{input_per_mtok, output_per_mtok}` that overrides the
built-in price estimates behind `cost_usd`). Without a key, workflows with agent
nodes still publish but those nodes fail permanently with `llm_not_configured`.

Organizations add their own provider keys under `/be/v1/providers` (`04 §4d`); these are
sealed with the vault key, so they need `VAULT_MASTER_KEY`. `OPENAI_BASE_URL` and
`GEMINI_BASE_URL` let an operator route those providers through a proxy.

OAuth integrations appear only for providers you register an app with, and need
`PUBLIC_URL` (redirect URI `{PUBLIC_URL}/be/v1/oauth/callback`): `GOOGLE_CLIENT_ID` and
`GOOGLE_CLIENT_SECRET` (Gmail and Drive), `DROPBOX_APP_KEY` and `DROPBOX_APP_SECRET`,
`MICROSOFT_CLIENT_ID`, `MICROSOFT_CLIENT_SECRET` and `MICROSOFT_TENANT` (default `common`).
`INTEGRATION_ENDPOINTS` (a JSON object) overrides provider URLs for a proxy or a test server.

Connections (`04 §4e`) seal their credentials with the vault key too. `RESEND_BASE_URL`
routes Resend through a proxy. Key rotation (`04 §4c`) re-seals vault secrets, provider
keys and connection credentials together.

Agents search the connectors listed in `tools` with three read-only tools, `glob`,
`grep` and `read` (`02 §4.14`); SQL runs in read-only transactions with row, size and
time caps. MCP servers are saved as
connections with kind `mcp`, `config: {url, transport: streamable_http}` (or `sse`)
and optional `secret: {token}`. Agents use `tools: ["mcp:server-name"]`; workflows
can call a tool directly with `mcp.call` (`02 §4.14`). Public DeepWiki was smoke-tested
with a scripted LLM; other MCP servers and real LLM accounts remain unverified.

Sign-in uses sessions and API keys (`04 §4b`). Set `BOOTSTRAP_ADMIN_EMAIL` and
`BOOTSTRAP_ADMIN_PASSWORD` once to create the first platform administrator;
`PUBLIC_URL` builds invite links, `SESSION_TTL` (default 336h) sets the session
lifetime, and `COOKIE_SECURE=true` is required in production. Outside production
the development `X-Tenant-ID` header still works, alongside real credentials.

The vault (`04 §4c`) turns on with `VAULT_MASTER_KEY`, a base64 32-byte key
(`openssl rand -base64 32`); `VAULT_KEY_VERSION` (default 1) and `VAULT_OLD_KEYS`
(`1:<key>,2:<key>`) support rotation. Every API and worker process needs the same
keys. Losing the key loses the secrets: back it up apart from the database.

Only `DATABASE_URL` is required; every other setting has a default (see
`internal/config/config.go`, where each field's `env` tag names its variable).

`make migrate` runs `go run ./cmd/api migrate`, which applies pending migrations
with goose. SQL is embedded in the binary. Versions are recorded in
`goose_db_version`; a PostgreSQL session advisory lock serialises concurrent
runners; each migration runs in its own transaction; an unapplied migration
older than the latest applied one is rejected. The service never runs Down SQL.
Databases migrated by the earlier hand-written runner (`schema_migrations`)
are not adopted automatically: recreate a local database, or insert the
applied versions into `goose_db_version` once.

Migrations are forward-only: add a new numbered SQL file with `-- +goose Up`
and `-- +goose Down` markers, wrapping dollar-quoted function bodies in
`-- +goose StatementBegin` / `-- +goose StatementEnd`. Do not edit applied files.

Database-node tests also run against MySQL when `MYSQL_URL` is set (a go-sql-driver DSN,
e.g. `user:pass@tcp(127.0.0.1:3306)/dbname`); CI provides one.

For integration tests, export `DATABASE_URL` (or source your trusted `.env` with
`set -a; source .env; set +a`) and run `make test-integration`.
Integration tests create and remove temporary schemas in the supplied database;
they do not migrate your application schema. They exercise the same runner as
the command and fail if `DATABASE_URL` is missing.

```sh
make fmt              # format Go
make generate         # regenerate sqlc after query changes
make check            # formatting, sqlc freshness, vet, unit + integration tests
```

GitHub Actions runs these checks with a PostgreSQL 16 service, checks the migration
command twice, and runs integration tests with the race detector. The workflow
will run on pushes and pull requests once this project is in a GitHub repository.

## Application lifecycle

`cmd/api` and `cmd/worker` are the two binaries; each fixes its mode. Worker IDs default to the hostname plus a random
suffix, so `docker compose up --scale worker=N` works. A worker serves
`/metrics` and `/healthz` on `WORKER_METRICS_ADDRESS` when it is set.

`internal/app.Main` owns environment loading, OS signals, and exit status. `internal/app.Run`
accepts a context and concrete configuration, constructs the logger, reserves the
HTTP address, opens PostgreSQL, and constructs the API. It reports the actual bound
address only after dependencies are ready. Startup failures release resources.

`internal/server.Server.Run` owns serving and request draining. The first signal stops
accepting connections while allowing active requests to finish. If the grace period
expires, it closes connections and reports the timeout. There is no custom handler tracking
or waiting after forced closure; handlers should honor request cancellation. A second signal restores normal
process termination behavior. PostgreSQL is closed after the API has stopped.

In `all` mode the API and the worker run side by side; the first to fail stops
the other, and both drain on shutdown (the worker releases its leases).
`make check` includes `go mod tidy -diff` to enforce reproducible module metadata.

## Web application and release packaging

The API embeds a React/Vite workspace. Run `make ui-install ui-build` after
frontend changes; Go-only builds use the committed bundle. Sign in at the API
root to manage workflows, agents, connections, run events, tasks, secrets,
webhooks, members, API keys, and usage. Workflows can be created from agents
without editing YAML, or drafted by the assistant using ASSISTANT_PROVIDER,
ASSISTANT_API_KEY, and ASSISTANT_MODEL (or a tenant provider connection).

See [the operations runbook](06-runbook.md) for the provisional Docker/Compose
setup and local backup/rolling-release acceptance. Production hosting remains
open for discussion; nothing has been deployed remotely.
