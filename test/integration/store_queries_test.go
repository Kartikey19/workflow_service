//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	workflowstore "github.com/kartikey/workflow_service/internal/store"
	"github.com/kartikey/workflow_service/internal/store/gen"
)

func TestCompleteNodeFencing(t *testing.T) {
	t.Run("current lease owner completes the node", func(t *testing.T) {
		ctx := context.Background()
		pool := newIsolatedPool(t, ctx)
		seedReadyNodes(t, ctx, pool, 1)
		queries := gen.New(pool)
		node := claimOne(t, ctx, queries, "worker-a")

		rows, err := queries.CompleteNode(ctx, gen.CompleteNodeParams{
			Status:  "succeeded",
			Output:  []byte(`{"result":"ok"}`),
			Metrics: []byte(`{"duration_ms":12}`),
			ID:      node.ID, LeaseToken: node.LeaseToken,
			WorkerID: text("worker-a"),
		})
		if err != nil {
			t.Fatalf("complete node: %v", err)
		}
		if rows != 1 {
			t.Fatalf("completed rows = %d, want 1", rows)
		}

		var status string
		var output []byte
		var leaseOwner pgtype.Text
		var leaseExpiry, endedAt pgtype.Timestamptz
		if err := pool.QueryRow(ctx, `
			select status, output, lease_owner, lease_expires_at, ended_at
			from node_executions where id = $1
		`, node.ID).Scan(&status, &output, &leaseOwner, &leaseExpiry, &endedAt); err != nil {
			t.Fatalf("read completed node: %v", err)
		}
		var gotOutput map[string]string
		if err := json.Unmarshal(output, &gotOutput); err != nil {
			t.Fatalf("decode completed output: %v", err)
		}
		if status != "succeeded" || gotOutput["result"] != "ok" || leaseOwner.Valid || leaseExpiry.Valid || !endedAt.Valid {
			t.Fatalf("unexpected completed state: status=%q output=%s lease_owner=%v lease_expiry=%v ended_at=%v", status, output, leaseOwner.Valid, leaseExpiry.Valid, endedAt.Valid)
		}
	})

	t.Run("wrong lease owner cannot overwrite the result", func(t *testing.T) {
		ctx := context.Background()
		pool := newIsolatedPool(t, ctx)
		seedReadyNodes(t, ctx, pool, 1)
		queries := gen.New(pool)
		node := claimOne(t, ctx, queries, "worker-a")

		rows, err := queries.CompleteNode(ctx, gen.CompleteNodeParams{
			Status: "succeeded",
			Output: []byte(`{"result":"stale"}`),
			ID:     node.ID, LeaseToken: node.LeaseToken,
			WorkerID: text("worker-b"),
		})
		if err != nil {
			t.Fatalf("complete as wrong worker: %v", err)
		}
		if rows != 0 {
			t.Fatalf("completed rows = %d, want 0", rows)
		}
		assertRunningLease(t, ctx, pool, node.ID, "worker-a")
	})

	t.Run("expired lease cannot complete the node", func(t *testing.T) {
		ctx := context.Background()
		pool := newIsolatedPool(t, ctx)
		seedReadyNodes(t, ctx, pool, 1)
		queries := gen.New(pool)
		node := claimOne(t, ctx, queries, "worker-a")

		if _, err := pool.Exec(ctx, `update node_executions set lease_expires_at = now() - interval '1 second' where id = $1`, node.ID); err != nil {
			t.Fatalf("expire lease: %v", err)
		}
		rows, err := queries.CompleteNode(ctx, gen.CompleteNodeParams{
			Status: "succeeded",
			Output: []byte(`{"result":"stale"}`),
			ID:     node.ID, LeaseToken: node.LeaseToken,
			WorkerID: text("worker-a"),
		})
		if err != nil {
			t.Fatalf("complete after lease expiry: %v", err)
		}
		if rows != 0 {
			t.Fatalf("completed rows = %d, want 0", rows)
		}
		assertRunningLease(t, ctx, pool, node.ID, "worker-a")
	})
}

// The fence is the per-claim token, not the worker ID: when a worker
// re-claims its own expired row, the earlier attempt's write must not land.
func TestCompleteNodeFencedByClaimToken(t *testing.T) {
	ctx := context.Background()
	pool := newIsolatedPool(t, ctx)
	seedReadyNodes(t, ctx, pool, 1)
	queries := gen.New(pool)
	stale := claimOne(t, ctx, queries, "worker-a")
	if _, err := pool.Exec(ctx, `update node_executions set lease_expires_at = now() - interval '1 second' where id = $1`, stale.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ReclaimExpiredLeases(ctx); err != nil {
		t.Fatal(err)
	}
	current := claimOne(t, ctx, queries, "worker-a")
	if current.ID != stale.ID || current.LeaseToken == stale.LeaseToken {
		t.Fatalf("re-claim did not issue a new token")
	}
	rows, err := queries.CompleteNode(ctx, gen.CompleteNodeParams{ID: stale.ID, LeaseToken: stale.LeaseToken,
		WorkerID: text("worker-a"), Status: "succeeded", Output: []byte(`{"result":"stale"}`)})
	if err != nil || rows != 0 {
		t.Fatalf("stale token completed %d rows (%v), want 0", rows, err)
	}
	rows, err = queries.CompleteNode(ctx, gen.CompleteNodeParams{ID: current.ID, LeaseToken: current.LeaseToken,
		WorkerID: text("worker-a"), Status: "succeeded", Output: []byte(`{"result":"fresh"}`)})
	if err != nil || rows != 1 {
		t.Fatalf("current token completed %d rows (%v), want 1", rows, err)
	}
}

func TestEnqueueNodeConcurrentInsertCreatesOneRow(t *testing.T) {
	ctx := context.Background()
	pool := newIsolatedPool(t, ctx)
	fixture := seedReadyNodes(t, ctx, pool, 1)
	queries := gen.New(pool)

	start := make(chan struct{})
	results := make(chan error, 2)
	ids := [2]pgtype.UUID{newUUID(t), newUUID(t)}
	for i := range ids {
		go func(i int) {
			<-start
			_, err := queries.EnqueueNode(ctx, gen.EnqueueNodeParams{
				ID:             ids[i],
				RunID:          fixture.RunID,
				TenantID:       fixture.TenantID,
				NodeID:         "join",
				NodeType:       "noop",
				BranchKey:      "",
				Status:         "ready",
				Input:          []byte(`{}`),
				IdempotencyKey: "join-once",
				RunAfter:       pgtype.Timestamptz{},
			})
			results <- err
		}(i)
	}

	close(start)
	var succeeded, noRows int
	for range ids {
		err := <-results
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, pgx.ErrNoRows):
			noRows++
		default:
			t.Fatalf("enqueue node: %v", err)
		}
	}
	if succeeded != 1 || noRows != 1 {
		t.Fatalf("enqueue results: successes=%d no_rows=%d, want 1 and 1", succeeded, noRows)
	}

	var count int
	if err := pool.QueryRow(ctx, `select count(*) from node_executions where run_id = $1 and node_id = 'join'`, fixture.RunID).Scan(&count); err != nil {
		t.Fatalf("count enqueued nodes: %v", err)
	}
	if count != 1 {
		t.Fatalf("enqueued rows = %d, want 1", count)
	}
}

func TestDepStatesUsesLatestAttempt(t *testing.T) {
	ctx := context.Background()
	pool := newIsolatedPool(t, ctx)
	fixture := seedReadyNodes(t, ctx, pool, 1)

	insertNode(t, ctx, pool, fixture, "retry", "", 1, "failed")
	insertNode(t, ctx, pool, fixture, "retry", "", 2, "succeeded")
	insertNode(t, ctx, pool, fixture, "active", "", 1, "running")
	insertNode(t, ctx, pool, fixture, "retry", "other", 1, "failed")

	states, err := gen.New(pool).DepStates(ctx, gen.DepStatesParams{
		RunID:      fixture.RunID,
		BranchKey:  "",
		DepNodeIds: []string{"retry", "active"},
	})
	if err != nil {
		t.Fatalf("read dependency states: %v", err)
	}
	got := make(map[string]string, len(states))
	for _, state := range states {
		got[state.NodeID] = state.Status
	}
	if len(got) != 2 || got["retry"] != "succeeded" || got["active"] != "running" {
		t.Fatalf("dependency states = %#v, want retry=succeeded and active=running", got)
	}
}

func TestReclaimExpiredLeasesOnlyReclaimsExpiredRunningNodes(t *testing.T) {
	ctx := context.Background()
	pool := newIsolatedPool(t, ctx)
	fixture := seedReadyNodes(t, ctx, pool, 1)

	expiredID := insertNode(t, ctx, pool, fixture, "expired", "", 1, "running")
	futureID := insertNode(t, ctx, pool, fixture, "future", "", 1, "running")
	if _, err := pool.Exec(ctx, `
		update node_executions
		   set lease_owner = 'worker-a', lease_expires_at = now() - interval '1 second'
		 where id = $1
	`, expiredID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		update node_executions
		   set lease_owner = 'worker-b', lease_expires_at = now() + interval '1 hour'
		 where id = $1
	`, futureID); err != nil {
		t.Fatalf("set future lease: %v", err)
	}

	reclaimed, err := gen.New(pool).ReclaimExpiredLeases(ctx)
	if err != nil {
		t.Fatalf("reclaim expired leases: %v", err)
	}
	if len(reclaimed) != 1 || reclaimed[0].ID.Bytes != expiredID.Bytes {
		t.Fatalf("reclaimed rows = %#v, want only expired node", reclaimed)
	}

	var status string
	var owner pgtype.Text
	var expiry pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `select status, lease_owner, lease_expires_at from node_executions where id = $1`, expiredID).Scan(&status, &owner, &expiry); err != nil {
		t.Fatalf("read reclaimed node: %v", err)
	}
	if status != "ready" || owner.Valid || expiry.Valid {
		t.Fatalf("reclaimed node state: status=%q owner=%v expiry=%v", status, owner.Valid, expiry.Valid)
	}
	assertRunningLease(t, ctx, pool, futureID, "worker-b")
}

func TestInTxRollsBackOnErrorAndPanic(t *testing.T) {
	ctx := context.Background()

	t.Run("error", func(t *testing.T) {
		pool := newIsolatedPool(t, ctx)
		want := errors.New("stop transaction")
		err := workflowstore.InTx(ctx, pool, func(tx pgx.Tx) error {
			insertWorkflow(t, ctx, tx, "tx-error")
			return want
		})
		if !errors.Is(err, want) {
			t.Fatalf("InTx error = %v, want %v", err, want)
		}
		assertWorkflowCount(t, ctx, pool, 0)
	})

	t.Run("panic", func(t *testing.T) {
		pool := newIsolatedPool(t, ctx)
		defer func() {
			if recovered := recover(); recovered != "stop transaction" {
				t.Fatalf("panic = %v, want stop transaction", recovered)
			}
			assertWorkflowCount(t, ctx, pool, 0)
		}()

		_ = workflowstore.InTx(ctx, pool, func(tx pgx.Tx) error {
			insertWorkflow(t, ctx, tx, "tx-panic")
			panic("stop transaction")
		})
	})
}

func claimOne(t *testing.T, ctx context.Context, queries *gen.Queries, workerID string) gen.NodeExecution {
	t.Helper()

	nodes, err := queries.ClaimReady(ctx, gen.ClaimReadyParams{
		WorkerID:  text(workerID),
		LeaseTtl:  pgtype.Interval{Microseconds: int64(30 * time.Second / time.Microsecond), Valid: true},
		BatchSize: 1,
	})
	if err != nil {
		t.Fatalf("claim one node: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("claimed %d nodes, want 1", len(nodes))
	}
	return nodes[0]
}

func insertNode(t *testing.T, ctx context.Context, pool *pgxpool.Pool, fixture runFixture, nodeID, branchKey string, attempt int32, status string) pgtype.UUID {
	t.Helper()

	id := newUUID(t)
	if _, err := pool.Exec(ctx, `
		insert into node_executions (
		  id, run_id, tenant_id, node_id, node_type, branch_key, attempt,
		  status, input, idempotency_key, run_after
		) values ($1, $2, $3, $4, 'noop', $5, $6, $7, '{}'::jsonb, $8, now())
	`, id, fixture.RunID, fixture.TenantID, nodeID, branchKey, attempt, status, fmt.Sprintf("%s-%d", nodeID, attempt)); err != nil {
		t.Fatalf("insert node %q attempt %d: %v", nodeID, attempt, err)
	}
	return id
}

func assertRunningLease(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id pgtype.UUID, workerID string) {
	t.Helper()

	var status string
	var owner pgtype.Text
	if err := pool.QueryRow(ctx, `select status, lease_owner from node_executions where id = $1`, id).Scan(&status, &owner); err != nil {
		t.Fatalf("read running lease: %v", err)
	}
	if status != "running" || !owner.Valid || owner.String != workerID {
		t.Fatalf("running lease: status=%q owner=%q valid=%v, want running/%q", status, owner.String, owner.Valid, workerID)
	}
}

func insertWorkflow(t *testing.T, ctx context.Context, tx pgx.Tx, name string) {
	t.Helper()

	if _, err := tx.Exec(ctx, `insert into workflows (id, tenant_id, name) values ($1, $2, $3)`, newUUID(t), newUUID(t), name); err != nil {
		t.Fatalf("insert workflow in transaction: %v", err)
	}
}

func assertWorkflowCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, want int) {
	t.Helper()

	var count int
	if err := pool.QueryRow(ctx, `select count(*) from workflows`).Scan(&count); err != nil {
		t.Fatalf("count workflows: %v", err)
	}
	if count != want {
		t.Fatalf("workflow rows = %d, want %d", count, want)
	}
}

func text(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: true}
}
