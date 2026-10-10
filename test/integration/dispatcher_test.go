//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap/zaptest"

	"github.com/kartikey/workflow_service/internal/engine"
	"github.com/kartikey/workflow_service/internal/executor"
	"github.com/kartikey/workflow_service/internal/flow"
	"github.com/kartikey/workflow_service/internal/spec"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/config"
)

const dispatchWorkflow = `
apiVersion: workflow/v1
name: dispatch-test
nodes:
  - id: upstream
    type: noop
    config: {}
  - id: target
    type: fake
    needs: [upstream]
    if: '${input.run_target}'
    retry: {max_attempts: 2, backoff: fixed, initial: 1m, max: 1m}
    config:
      value: '${nodes.upstream.output.x}'
      greeting: 'hi ${input.name}'
`

type fakeExecutor struct {
	execute func(context.Context, executor.Request) (executor.Result, error)
}

func (fakeExecutor) Type() string                         { return "fake" }
func (fakeExecutor) ValidateConfig(json.RawMessage) error { return nil }
func (f fakeExecutor) Execute(ctx context.Context, req executor.Request) (executor.Result, error) {
	return f.execute(ctx, req)
}

type targetRow struct {
	Status               string
	Input, Output, Error map[string]any
	Metrics              map[string]any
	LeaseOwner           pgtype.Text
	IdempotencyKey       string
	RunAfter             time.Time
	Attempt              int32
}

func TestDispatcherRun(t *testing.T) {
	cases := []struct {
		name      string
		runTarget bool
		execute   func(*pgxpool.Pool) func(context.Context, executor.Request) (executor.Result, error)
		check     func(t *testing.T, rows []targetRow, calls int32)
	}{
		{
			name: "succeeds with resolved config", runTarget: true,
			execute: echoConfig,
			check: func(t *testing.T, rows []targetRow, calls int32) {
				want := map[string]any{"greeting": "hi ada", "value": float64(5)}
				row := onlyAttempt(t, rows)
				if row.Status != "succeeded" || !reflect.DeepEqual(row.Input, want) || !reflect.DeepEqual(row.Output, want) {
					t.Fatalf("row = %+v, want succeeded with input and output %v", row, want)
				}
				if _, ok := row.Metrics["duration_ms"]; !ok || row.LeaseOwner.Valid {
					t.Fatalf("metrics = %v, lease owner = %v; want duration_ms and cleared lease", row.Metrics, row.LeaseOwner)
				}
			},
		},
		{
			name: "false condition skips without executing", runTarget: false,
			execute: echoConfig,
			check: func(t *testing.T, rows []targetRow, calls int32) {
				if row := onlyAttempt(t, rows); row.Status != "skipped" || calls != 0 {
					t.Fatalf("status = %q, executor calls = %d; want skipped and 0", row.Status, calls)
				}
			},
		},
		{
			name: "retryable failure enqueues next attempt with same key", runTarget: true,
			execute: failWith(&executor.RetryableError{Code: "upstream_5xx"}),
			check: func(t *testing.T, rows []targetRow, calls int32) {
				if len(rows) != 2 {
					t.Fatalf("attempt rows = %d, want 2", len(rows))
				}
				first, retry := rows[0], rows[1]
				if first.Status != "failed" || first.Error["code"] != "upstream_5xx" || first.Error["retryable"] != true {
					t.Fatalf("first attempt = %+v, want failed upstream_5xx retryable", first)
				}
				if retry.Status != "ready" || retry.Attempt != 2 || retry.IdempotencyKey != first.IdempotencyKey {
					t.Fatalf("retry = %+v, want ready attempt 2 with key %q", retry, first.IdempotencyKey)
				}
				if delay := time.Until(retry.RunAfter); delay < 50*time.Second {
					t.Fatalf("retry run_after in %v, want about 1m of fixed backoff", delay)
				}
			},
		},
		{
			name: "permanent failure creates no retry", runTarget: true,
			execute: failWith(&executor.PermanentError{Code: "bad_request"}),
			check: func(t *testing.T, rows []targetRow, calls int32) {
				if row := onlyAttempt(t, rows); row.Status != "failed" || row.Error["code"] != "bad_request" || row.Error["retryable"] != false {
					t.Fatalf("row = %+v, want permanent bad_request failure", row)
				}
			},
		},
		{
			name: "executor panic fails permanently", runTarget: true,
			execute: func(*pgxpool.Pool) func(context.Context, executor.Request) (executor.Result, error) {
				return func(context.Context, executor.Request) (executor.Result, error) { panic("boom") }
			},
			check: func(t *testing.T, rows []targetRow, calls int32) {
				if row := onlyAttempt(t, rows); row.Status != "failed" || row.Error["code"] != "executor_panic" {
					t.Fatalf("row = %+v, want executor_panic failure", row)
				}
			},
		},
		{
			name: "lost lease discards the result", runTarget: true,
			execute: func(pool *pgxpool.Pool) func(context.Context, executor.Request) (executor.Result, error) {
				return func(ctx context.Context, req executor.Request) (executor.Result, error) {
					id := pgtype.UUID{Bytes: req.NodeExecutionID, Valid: true}
					if _, err := pool.Exec(ctx, `update node_executions set lease_expires_at = now() - interval '1 second' where id = $1`, id); err != nil {
						return executor.Result{}, err
					}
					return executor.Result{Output: json.RawMessage(`{"stale":true}`)}, nil
				}
			},
			check: func(t *testing.T, rows []targetRow, calls int32) {
				if row := onlyAttempt(t, rows); row.Status != "running" || row.Output != nil {
					t.Fatalf("row = %+v, want stale result discarded and node still running", row)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			pool := newIsolatedPool(t, ctx)
			seedDispatchRun(t, ctx, pool, tc.runTarget)

			var calls atomic.Int32
			run := tc.execute(pool)
			registry := executor.NewRegistry()
			for _, ex := range []executor.Executor{executor.Noop{}, fakeExecutor{execute: func(ctx context.Context, req executor.Request) (executor.Result, error) {
				calls.Add(1)
				return run(ctx, req)
			}}} {
				if err := registry.Register(ex); err != nil {
					t.Fatal(err)
				}
			}
			logger := zaptest.NewLogger(t)
			f, err := flow.New(pool, logger)
			if err != nil {
				t.Fatal(err)
			}
			dispatcher := engine.NewDispatcher(config.WorkerConfig{ID: "worker-a", LeaseTTL: 30 * time.Second}, f, registry, nil, logger)

			dispatcher.Run(ctx, claimOne(t, ctx, gen.New(pool), "worker-a"))
			tc.check(t, readTargetRows(t, ctx, pool), calls.Load())
		})
	}
}

func echoConfig(*pgxpool.Pool) func(context.Context, executor.Request) (executor.Result, error) {
	return func(_ context.Context, req executor.Request) (executor.Result, error) {
		return executor.Result{Output: req.Config}, nil
	}
}

func failWith(err error) func(*pgxpool.Pool) func(context.Context, executor.Request) (executor.Result, error) {
	return func(*pgxpool.Pool) func(context.Context, executor.Request) (executor.Result, error) {
		return func(context.Context, executor.Request) (executor.Result, error) { return executor.Result{}, err }
	}
}

// seedDispatchRun publishes dispatchWorkflow, records upstream as succeeded, and
// leaves target as the only ready node.
func seedDispatchRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, runTarget bool) {
	t.Helper()

	compiled, diagnostics := spec.Compile([]byte(dispatchWorkflow))
	if compiled == nil {
		t.Fatalf("compile workflow: %v", diagnostics)
	}
	specJSON, err := json.Marshal(compiled.Spec)
	if err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(map[string]any{"run_target": runTarget, "name": "ada"})
	if err != nil {
		t.Fatal(err)
	}

	tenantID, workflowID, versionID, runID := newUUID(t), newUUID(t), newUUID(t), newUUID(t)
	statements := []struct {
		sql  string
		args []any
	}{
		{`insert into workflows (id, tenant_id, name) values ($1, $2, 'dispatch-test')`, []any{workflowID, tenantID}},
		{`insert into workflow_versions (id, workflow_id, version, yaml_source, spec, spec_hash)
		  values ($1, $2, 1, $3, $4, 'dispatch-test')`, []any{versionID, workflowID, dispatchWorkflow, specJSON}},
		{`insert into runs (id, tenant_id, workflow_id, workflow_version_id, status, input, started_at)
		  values ($1, $2, $3, $4, 'running', $5, now())`, []any{runID, tenantID, workflowID, versionID, input}},
		{`insert into node_executions (id, run_id, tenant_id, node_id, node_type, status, output, idempotency_key, ended_at)
		  values ($1, $2, $3, 'upstream', 'noop', 'succeeded', '{"x": 5}', 'upstream-key', now())`, []any{newUUID(t), runID, tenantID}},
		{`insert into node_executions (id, run_id, tenant_id, node_id, node_type, status, idempotency_key, run_after)
		  values ($1, $2, $3, 'target', 'fake', 'ready', 'target-key', now() - interval '1 second')`, []any{newUUID(t), runID, tenantID}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("seed dispatch run: %v", err)
		}
	}
}

func readTargetRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []targetRow {
	t.Helper()

	rows, err := pool.Query(ctx, `
		select status, input, output, error, metrics, lease_owner, idempotency_key, run_after, attempt
		  from node_executions
		 where node_id = 'target'
		 order by attempt`)
	if err != nil {
		t.Fatalf("read target rows: %v", err)
	}
	defer rows.Close()

	var out []targetRow
	for rows.Next() {
		var row targetRow
		if err := rows.Scan(&row.Status, &row.Input, &row.Output, &row.Error, &row.Metrics,
			&row.LeaseOwner, &row.IdempotencyKey, &row.RunAfter, &row.Attempt); err != nil {
			t.Fatalf("scan target row: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read target rows: %v", err)
	}
	return out
}

func onlyAttempt(t *testing.T, rows []targetRow) targetRow {
	t.Helper()

	if len(rows) != 1 {
		t.Fatalf("target rows = %d, want 1", len(rows))
	}
	return rows[0]
}
