package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kartikey/workflow_service/internal/config"
	"github.com/kartikey/workflow_service/migrations"
	"github.com/kartikey/workflow_service/pkg/db"
)

// Migrate applies pending migrations and returns the exit status. Rollback is
// intentionally unsupported.
func Migrate(name string) int {
	if err := migrate(); err != nil {
		fmt.Fprintf(os.Stderr, "%s migrate: %v\n", name, err)
		return 1
	}
	return 0
}

func migrate() error {
	if err := config.LoadEnv(); err != nil {
		return err
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return errors.New("DATABASE_URL is required")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return errors.New("invalid DATABASE_URL")
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return errors.New("create migration pool")
	}
	defer pool.Close()
	count, err := db.Migrate(ctx, pool, migrations.Files)
	if err != nil {
		return err
	}
	fmt.Printf("Applied %d migration(s). Database is up to date.\n", count)
	return nil
}
