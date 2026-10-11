//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/kartikey/workflow_service/migrations"
	"github.com/kartikey/workflow_service/pkg/db"
)

func TestMigrationsConcurrentAndRepeat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := newEmptyPool(t, ctx)
	var wg sync.WaitGroup
	counts := make(chan int, 2)
	for range 2 {
		wg.Go(func() {
			n, err := db.Migrate(ctx, pool, migrations.Files)
			if err != nil {
				t.Errorf("migrate: %v", err)
				return
			}
			counts <- n
		})
	}
	wg.Wait()
	close(counts)
	total := 0
	for n := range counts {
		total += n
	}
	if want := embeddedMigrationCount(t); total != want {
		t.Fatalf("applied %d migrations, want %d", total, want)
	}
	if n, err := db.Migrate(ctx, pool, migrations.Files); err != nil || n != 0 {
		t.Fatalf("repeat = %d, %v", n, err)
	}
	var tables int
	if err := pool.QueryRow(ctx, `select count(*) from information_schema.tables where table_schema = current_schema() and table_name in ('workflows','workflow_versions','runs','node_executions','run_events')`).Scan(&tables); err != nil || tables != 5 {
		t.Fatalf("tables = %d, %v", tables, err)
	}
}

func TestMigrationsRejectMissingOlderFiles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := newIsolatedPool(t, ctx)
	// Simulate a version that was never applied but is older than the latest:
	// goose refuses to backfill a historical gap instead of guessing.
	if _, err := pool.Exec(ctx, "delete from goose_db_version where version_id = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Migrate(ctx, pool, migrations.Files); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("older unapplied migration: %v", err)
	}
}

func TestMigrationsRollBackAFailedMigration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := newEmptyPool(t, ctx)
	files := fstest.MapFS{
		"0001_good.sql": {Data: []byte("-- +goose Up\ncreate table rollback_probe(id int);\n-- +goose Down\n")},
		"0002_bad.sql":  {Data: []byte("-- +goose Up\ncreate table half_done(id int);\nselect * from nonexistent_migration_table;\n-- +goose Down\n")},
	}
	if _, err := db.Migrate(ctx, pool, files); err == nil {
		t.Fatal("expected migration failure")
	}
	// Each migration is its own transaction: 0001 stays, 0002 left nothing.
	var good, bad bool
	if err := pool.QueryRow(ctx, "select to_regclass('rollback_probe') is not null, to_regclass('half_done') is null").Scan(&good, &bad); err != nil || !good || !bad {
		t.Fatalf("after failure: first applied=%v, second rolled back=%v, %v", good, bad, err)
	}
	files["0002_bad.sql"] = &fstest.MapFile{Data: []byte("-- +goose Up\ncreate table half_done(id int);\n-- +goose Down\n")}
	if n, err := db.Migrate(ctx, pool, files); err != nil || n != 1 {
		t.Fatalf("retry = %d, %v", n, err)
	}
}

func TestMigrationCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := newEmptyPool(t, ctx)
	var schema string
	if err := pool.QueryRow(ctx, "select current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if strings.HasPrefix(databaseURL, "postgres://") || strings.HasPrefix(databaseURL, "postgresql://") {
		parsed, err := url.Parse(databaseURL)
		if err != nil {
			t.Fatal("invalid database URL")
		}
		params := parsed.Query()
		params.Set("search_path", schema)
		parsed.RawQuery = params.Encode()
		databaseURL = parsed.String()
	} else {
		databaseURL += " search_path=" + schema
	}
	_, file, _, _ := runtime.Caller(0)
	for _, want := range []string{fmt.Sprintf("Applied %d migration(s)", embeddedMigrationCount(t)), "Applied 0 migration(s)"} {
		cmd := exec.CommandContext(ctx, "go", "run", "./cmd/api", "migrate")
		cmd.Dir = filepath.Join(filepath.Dir(file), "../..")
		cmd.Env = append(os.Environ(), "DATABASE_URL="+databaseURL)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("migration command failed: %v", err)
		}
		if !strings.Contains(string(output), want) {
			t.Fatalf("migration command did not report %q", want)
		}
	}
}

func embeddedMigrationCount(t *testing.T) int {
	t.Helper()
	names, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	return len(names)
}
