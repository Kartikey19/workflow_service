//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/store/gen"
)

func TestExtendLeaseFencing(t *testing.T) {
	for _, name := range []string{"current owner", "wrong owner", "expired lease", "completed node"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			pool := newIsolatedPool(t, ctx)
			seedReadyNodes(t, ctx, pool, 1)
			queries := gen.New(pool)
			node := claimOne(t, ctx, queries, "worker-a")
			owner := "worker-a"
			want := int64(0)
			switch name {
			case "current owner":
				want = 1
			case "wrong owner":
				owner = "worker-b"
			case "expired lease":
				if _, err := pool.Exec(ctx, `update node_executions set lease_expires_at = now() - interval '1 second' where id = $1`, node.ID); err != nil {
					t.Fatal(err)
				}
			case "completed node":
				if _, err := queries.CompleteNode(ctx, gen.CompleteNodeParams{ID: node.ID, WorkerID: text(owner), Status: "succeeded", Output: []byte(`{}`)}); err != nil {
					t.Fatal(err)
				}
			}
			var before pgtype.Timestamptz
			if err := pool.QueryRow(ctx, `select lease_expires_at from node_executions where id = $1`, node.ID).Scan(&before); err != nil {
				t.Fatal(err)
			}
			rows, err := queries.ExtendLease(ctx, gen.ExtendLeaseParams{
				ID: node.ID, WorkerID: text(owner),
				LeaseTtl: pgtype.Interval{Microseconds: (2 * time.Minute).Microseconds(), Valid: true},
			})
			if err != nil {
				t.Fatal(err)
			}
			if rows != want {
				t.Fatalf("extended rows = %d, want %d", rows, want)
			}
			var after pgtype.Timestamptz
			if err := pool.QueryRow(ctx, `select lease_expires_at from node_executions where id = $1`, node.ID).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if want == 1 {
				if !after.Valid || !after.Time.After(before.Time) {
					t.Fatal("current owner's lease was not extended")
				}
			} else if before.Valid != after.Valid || !before.Time.Equal(after.Time) {
				t.Fatal("fenced extension changed lease expiry")
			}
		})
	}
}
