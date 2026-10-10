//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kartikey/workflow_service/internal/db"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/migrations"
)

const (
	readyNodeCount = 100
	claimerCount   = 4
)

type runFixture struct {
	TenantID pgtype.UUID
	RunID    pgtype.UUID
}

func TestClaimReadyConcurrentlyClaimsEveryReadyNodeOnce(t *testing.T) {
	ctx := context.Background()
	pool := newIsolatedPool(t, ctx)
	seedReadyNodes(t, ctx, pool, readyNodeCount)

	queries := gen.New(pool)
	start := make(chan struct{})
	claimed := make(chan pgtype.UUID, readyNodeCount)
	errs := make(chan error, claimerCount)

	var wg sync.WaitGroup
	for worker := range claimerCount {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start

			for {
				nodes, err := queries.ClaimReady(ctx, gen.ClaimReadyParams{
					WorkerID:  pgtype.Text{String: fmt.Sprintf("worker-%d", worker), Valid: true},
					LeaseTtl:  pgtype.Interval{Microseconds: int64(30 * time.Second / time.Microsecond), Valid: true},
					BatchSize: 10,
				})
				if err != nil {
					errs <- fmt.Errorf("worker %d: claim ready: %w", worker, err)
					return
				}

				for _, node := range nodes {
					claimed <- node.ID
				}
				if len(nodes) == 0 {
					return
				}
			}
		}(worker)
	}

	close(start)
	wg.Wait()
	close(errs)
	close(claimed)

	for err := range errs {
		t.Error(err)
	}

	seen := make(map[[16]byte]struct{}, readyNodeCount)
	claimCount := 0
	for id := range claimed {
		if !id.Valid {
			t.Fatal("ClaimReady returned an invalid node ID")
		}
		claimCount++
		if _, duplicate := seen[id.Bytes]; duplicate {
			t.Fatalf("node %s was claimed more than once", id.String())
		}
		seen[id.Bytes] = struct{}{}
	}

	if claimCount != readyNodeCount {
		t.Fatalf("claimed %d nodes, want %d", claimCount, readyNodeCount)
	}
	if len(seen) != readyNodeCount {
		t.Fatalf("claimed %d unique nodes, want %d", len(seen), readyNodeCount)
	}

	var running, leased int
	err := pool.QueryRow(ctx, `
		select
		  count(*) filter (where status = 'running'),
		  count(*) filter (
		    where status = 'running'
		      and lease_owner is not null
		      and lease_expires_at > now()
		      and started_at is not null
		  )
		from node_executions
	`).Scan(&running, &leased)
	if err != nil {
		t.Fatalf("verify claimed rows: %v", err)
	}
	if running != readyNodeCount {
		t.Fatalf("running rows = %d, want %d", running, readyNodeCount)
	}
	if leased != readyNodeCount {
		t.Fatalf("properly leased rows = %d, want %d", leased, readyNodeCount)
	}
}

func newIsolatedPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	pool := newEmptyPool(t, ctx)
	applyMigrations(t, ctx, pool)
	return pool
}

func newEmptyPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("DATABASE_URL is required for integration tests")
	}

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	admin, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	t.Cleanup(admin.Close)

	schema := "test_claim_ready_" + randomHex(t, 8)
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "create schema "+quotedSchema); err != nil {
		t.Fatalf("create test schema: %v", err)
	}

	testConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse test DATABASE_URL: %v", err)
	}
	if testConfig.ConnConfig.RuntimeParams == nil {
		testConfig.ConnConfig.RuntimeParams = make(map[string]string)
	}
	testConfig.ConnConfig.RuntimeParams["search_path"] = schema

	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(context.Background(), "drop schema "+quotedSchema+" cascade"); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
	})

	return pool
}

func applyMigrations(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()

	if _, err := db.Migrate(ctx, pool, migrations.Files); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

}

func seedReadyNodes(t *testing.T, ctx context.Context, pool *pgxpool.Pool, count int) runFixture {
	t.Helper()

	tenantID := newUUID(t)
	workflowID := newUUID(t)
	workflowVersionID := newUUID(t)
	runID := newUUID(t)

	batch := &pgx.Batch{}
	batch.Queue(`
		insert into workflows (id, tenant_id, name)
		values ($1, $2, 'claim-ready-test')
	`, workflowID, tenantID)
	batch.Queue(`
		insert into workflow_versions (id, workflow_id, version, yaml_source, spec, spec_hash)
		values ($1, $2, 1, '{}', '{}'::jsonb, 'claim-ready-test')
	`, workflowVersionID, workflowID)
	batch.Queue(`
		insert into runs (id, tenant_id, workflow_id, workflow_version_id, status)
		values ($1, $2, $3, $4, 'pending')
	`, runID, tenantID, workflowID, workflowVersionID)

	for i := range count {
		batch.Queue(`
			insert into node_executions (
			  id, run_id, tenant_id, node_id, node_type, branch_key, attempt,
			  status, input, idempotency_key, run_after
			) values (
			  $1, $2, $3, $4, 'noop', '', 1,
			  'ready', '{}'::jsonb, $5, now() - interval '1 second'
			)
		`, newUUID(t), runID, tenantID, fmt.Sprintf("node-%03d", i), fmt.Sprintf("claim-ready-%03d", i))
	}

	results := pool.SendBatch(ctx, batch)
	defer results.Close()
	for range count + 3 {
		if _, err := results.Exec(); err != nil {
			t.Fatalf("seed ready nodes: %v", err)
		}
	}

	return runFixture{TenantID: tenantID, RunID: runID}
}

func newUUID(t *testing.T) pgtype.UUID {
	t.Helper()

	var id pgtype.UUID
	if _, err := rand.Read(id.Bytes[:]); err != nil {
		t.Fatalf("generate UUID: %v", err)
	}
	id.Bytes[6] = (id.Bytes[6] & 0x0f) | 0x40
	id.Bytes[8] = (id.Bytes[8] & 0x3f) | 0x80
	id.Valid = true
	return id
}

func randomHex(t *testing.T, bytes int) string {
	t.Helper()

	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generate random schema suffix: %v", err)
	}
	return hex.EncodeToString(b)
}
