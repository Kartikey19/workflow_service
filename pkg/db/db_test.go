package db

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestOpenRejectsMalformedDatabaseURLWithoutLeakingIt(t *testing.T) {
	const databaseURL = "postgres://workflow:super-secret@localhost:%zz/workflow"

	pool, err := Open(context.Background(), Config{
		DatabaseURL:    databaseURL,
		ConnectTimeout: time.Second,
	})
	if pool != nil {
		pool.Close()
		t.Fatal("Open() pool is non-nil for malformed URL")
	}
	if err == nil {
		t.Fatal("Open() error = nil, want malformed URL error")
	}
	if got, want := err.Error(), "parse DATABASE_URL"; got != want {
		t.Errorf("Open() error = %q, want %q", got, want)
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Errorf("Open() error leaked database credentials: %q", err)
	}
}
