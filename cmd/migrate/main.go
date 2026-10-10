// Command migrate applies pending migrations; rollback is intentionally unsupported.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kartikey/workflow_service/migrations"
	"github.com/kartikey/workflow_service/pkg/config"
	"github.com/kartikey/workflow_service/pkg/migrate"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 1 {
		return errors.New("usage: migrate (set DATABASE_URL; only forward migrations are supported)")
	}
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
	count, err := migrate.Up(ctx, pool, migrations.Files)
	if err != nil {
		return err
	}
	fmt.Printf("Applied %d migration(s). Database is up to date.\n", count)
	return nil
}
