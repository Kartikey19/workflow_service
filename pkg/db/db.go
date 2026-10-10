// Package db owns PostgreSQL connectivity.
package db

import (
	"context"
	"errors"

	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	DatabaseURL     string
	MaxConns        int32
	MinConns        int32
	ConnectTimeout  time.Duration
	ApplicationName string
}

func Open(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {

	poolConfig, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		// Do not wrap this error: a malformed URL error can include credentials.
		return nil, errors.New("parse DATABASE_URL")
	}

	if cfg.ConnectTimeout <= 0 {
		return nil, errors.New("database connect timeout must be positive")
	}
	if cfg.MaxConns <= 0 || cfg.MinConns < 0 || cfg.MinConns > cfg.MaxConns {
		return nil, errors.New("invalid database pool size")
	}
	poolConfig.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	poolConfig.MaxConns = cfg.MaxConns
	poolConfig.MinConns = cfg.MinConns
	if poolConfig.ConnConfig.RuntimeParams == nil {
		poolConfig.ConnConfig.RuntimeParams = make(map[string]string)
	}
	poolConfig.ConnConfig.RuntimeParams["application_name"] = cfg.ApplicationName
	poolConfig.ConnConfig.RuntimeParams["TimeZone"] = "UTC"

	connectCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(connectCtx, poolConfig)
	if err != nil {
		return nil, errors.New("create postgres pool")
	}

	if err := pool.Ping(connectCtx); err != nil {
		pool.Close()
		return nil, errors.New("connect PostgreSQL: initial ping failed")
	}

	return pool, nil
}
