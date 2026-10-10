# workflow_service engineering guidance

## Go baseline

- Target Go 1.27 as declared in `go.mod`. Keep the `go` directive current and
  use only APIs supported by that directive; Go 1.27's default `stdversion` vet
  check is part of `go test`.
- Format all Go with `gofmt`. Before handing off a change, run `go test ./...`;
  run integration tests explicitly with `go test -tags=integration ./test/integration`.
- Prefer the standard library and small, explicit APIs. Wrap errors with useful
  operation context, never secrets such as `DATABASE_URL` or passwords.
- Use `context.Context` for operations that can block. Do not start goroutines
  without a defined owner, cancellation path, and testable completion behavior.
- Treat Go 1.27 additions as opt-in tools, not mandatory rewrites. In particular,
  do not migrate JSON behavior to `encoding/json/v2` without compatibility tests.

## Layout

- Standard Go application layout: each binary is `cmd/<name>/main.go` and only
  calls `internal/app.Main`; everything else lives under `internal/`. There is
  no `pkg/`: nothing here is a library for other modules.
- Prefer well-known libraries over hand-written infrastructure: configuration
  uses `caarlos0/env` struct tags plus `joho/godotenv`, migrations use
  `pressly/goose`, logging uses `zap`.

## Generics

- Use generics only when one algorithm or container has two or more real type
  instantiations and a type parameter makes the call site clearer.
- Prefer concrete types for domain entities, SQL queries, pgx parameters,
  transactions, and workflow state transitions. Do not create generic
  repositories, generic CRUD helpers, or `any`-based database abstractions.
- Go 1.27 generic methods may organize genuinely type-parameterized collection
  behavior. They cannot be used to implement generic interface methods; keep
  public interfaces concrete and small.

## PostgreSQL and sqlc

- PostgreSQL is the durable queue. Keep locking, leasing, fencing, and conflict
  behavior visible in SQL; do not introduce an ORM.
- SQL under `internal/store/queries/` is the source of truth. Run
  `go tool sqlc generate` after every query change. Never hand-edit
  `internal/store/gen/`.
- Maintain forward-only, additive migrations in goose format (`-- +goose Up`,
  `-- +goose Down`, and `StatementBegin/End` around dollar-quoted bodies).
  Never modify a migration that has been applied outside an isolated test schema.
- Store tests use real PostgreSQL, a fresh random schema, and `search_path`.
  Never mock `FOR UPDATE SKIP LOCKED`, lease fencing, or `ON CONFLICT` behavior.
- A zero-row fenced write is a normal stale-worker outcome: discard the result
  and log it; never force or retry that stale write.

## References

- Go 1.27 release notes: https://go.dev/doc/go1.27
- Go 1.27 generic methods: https://go.dev/blog/generic-methods
- Effective Go: https://go.dev/doc/effective_go
