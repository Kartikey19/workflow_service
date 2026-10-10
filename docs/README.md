# workflow_service — design documents

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
| 04 | [HTTP API](04-api.md) | Nine endpoints, error model, idempotency semantics |
| 05 | [Implementation plan](05-implementation-plan.md) | Repo layout, dependencies, M0–M9 with acceptance criteria, testing, ops |

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
go run .
# Or apply database migrations:
make migrate
```

`make migrate` runs `go run ./cmd/migrate`. SQL is embedded in the binary, so
there is no runtime dependency on the migrations directory. It applies only Up
sections, records versions and full-file checksums in `schema_migrations`, and
prints how many migrations were applied. Repeating it applies zero migrations.
All pending changes commit together; failure rolls back DDL and ledger entries.
Concurrent runners serialize using a transaction advisory lock. The command
has a two-minute deadline and supports cancellation with SIGINT/SIGTERM.

Migrations are forward-only: add a new numbered SQL file with the existing
`-- +goose Up` and `-- +goose Down` markers. The runner never executes Down SQL.
Keep statements transactional; Goose procedural directives are not supported.
Do not edit applied files. Changed/missing applied files and historical gaps
are rejected. Existing tables without this runner's compatible migration ledger
are not automatically adopted; inspect the existing migration history first.

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

Set `APP_MODE=api` in `.env` and start the currently implemented API with `go run .`. API mode does not require `WORKER_*` settings. `worker` and `all`
are recognized configuration modes but startup rejects them until M2 implements
the worker. `SHUTDOWN_GRACE` belongs to application configuration.

`main.go` owns environment loading, OS signals, and exit status. `internal/app.Run`
accepts a context and concrete configuration, constructs the logger, reserves the
HTTP address, opens PostgreSQL, and constructs the API. It reports the actual bound
address only after dependencies are ready. Startup failures release resources.

`internal/api.Server.Run` owns serving and request draining. The first signal stops
accepting connections while allowing active requests to finish. If the grace period
expires, it closes connections and reports the timeout. There is no custom handler tracking
or waiting after forced closure; handlers should honor request cancellation. A second signal restores normal
process termination behavior. PostgreSQL is closed after the API has stopped.

One serving goroutine currently needs only an owned result channel. Introduce
`errgroup` when workers and the sweeper add independently running components.
`make check` includes `go mod tidy -diff` to enforce reproducible module metadata.
