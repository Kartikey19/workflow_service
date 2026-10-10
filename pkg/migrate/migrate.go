// Package migrate applies forward-only SQL migrations transactionally.
package migrate

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Up applies all pending files atomically. Files must use NNNN_name.sql names
// and Goose Up/Down markers. Applied files cannot be changed or removed.
// The caller owns the pool and must supply a context with a deadline.
func Up(ctx context.Context, pool *pgxpool.Pool, files fs.FS) (int, error) {
	names, err := fs.Glob(files, "*.sql")
	if err != nil || len(names) == 0 {
		return 0, fmt.Errorf("find migrations: no SQL files")
	}
	type migration struct {
		version             int64
		name, checksum, sql string
	}
	pending := make([]migration, 0, len(names))
	seen := make(map[int64]bool)
	for _, name := range names {
		prefix, _, _ := strings.Cut(name, "_")
		version, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil || version <= 0 || seen[version] {
			return 0, fmt.Errorf("invalid or duplicate migration version: %s", name)
		}
		seen[version] = true
		data, err := fs.ReadFile(files, name)
		if err != nil {
			return 0, fmt.Errorf("read migration %s", name)
		}
		up, _, ok := strings.Cut(string(data), "-- +goose Down")
		if !ok || !strings.HasPrefix(up, "-- +goose Up\n") || strings.Contains(up, "-- +goose Statement") || strings.Contains(up, "-- +goose NO TRANSACTION") {
			return 0, fmt.Errorf("unsupported migration format: %s", name)
		}
		pending = append(pending, migration{version, name, fmt.Sprintf("%x", sha256.Sum256(data)), up})
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].version < pending[j].version })
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin migration transaction")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	// Serialize migration runners, including the first creation of the ledger.
	if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock(780001)"); err != nil {
		return 0, fmt.Errorf("lock migrations")
	}
	if _, err := tx.Exec(ctx, `create table if not exists schema_migrations (
 version bigint primary key, name text not null, checksum text not null,
 applied_at timestamptz not null default now())`); err != nil {
		return 0, fmt.Errorf("create migration ledger")
	}
	rows, err := tx.Query(ctx, "select version, name, checksum from schema_migrations order by version")
	if err != nil {
		return 0, fmt.Errorf("read migration ledger")
	}
	applied := make(map[int64]migration)
	var latest int64
	for rows.Next() {
		var m migration
		if err := rows.Scan(&m.version, &m.name, &m.checksum); err != nil {
			rows.Close()
			return 0, fmt.Errorf("decode migration ledger")
		}
		applied[m.version] = m
		latest = m.version
	}
	rows.Close()
	if rows.Err() != nil {
		return 0, fmt.Errorf("read migration ledger rows")
	}
	for version := range applied {
		if !seen[version] {
			return 0, fmt.Errorf("applied migration %d is missing", version)
		}
	}
	count := 0
	for _, m := range pending {
		if old, ok := applied[m.version]; ok {
			if old.name != m.name || old.checksum != m.checksum {
				return 0, fmt.Errorf("applied migration %s has changed", m.name)
			}
			continue
		}
		if m.version <= latest {
			return 0, fmt.Errorf("migration %s is older than the latest applied version", m.name)
		}
		if _, err := tx.Exec(ctx, m.sql, pgx.QueryExecModeSimpleProtocol); err != nil {
			return 0, fmt.Errorf("apply migration %s", m.name)
		}
		if _, err := tx.Exec(ctx, "insert into schema_migrations(version, name, checksum) values ($1, $2, $3)", m.version, m.name, m.checksum); err != nil {
			return 0, fmt.Errorf("record migration %s", m.name)
		}
		count++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit migrations")
	}
	return count, nil
}
