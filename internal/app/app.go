// Package app composes the service and owns its dependencies' lifetimes.
package app

import (
	"context"
	"fmt"
	"net"

	"github.com/kartikey/workflow_service/internal/api"
	"github.com/kartikey/workflow_service/pkg/config"
	"github.com/kartikey/workflow_service/pkg/db"
	"github.com/kartikey/workflow_service/pkg/logger"
	"go.uber.org/zap"
)

// Run constructs dependencies, serves, then releases resources in reverse order.
func Run(ctx context.Context, cfg config.Config) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate configuration: %w", err)
	}
	if cfg.App.Mode != "api" {
		return fmt.Errorf("APP_MODE=%s is not implemented; use APP_MODE=api until the worker exists", cfg.App.Mode)
	}
	log, err := logger.NewLogger(logger.Config{
		ServiceName: cfg.App.ServiceName, Environment: cfg.App.Environment,
		Version: cfg.App.BuildVersion, Level: cfg.Log.LogLevel, AddSource: cfg.Log.AddSource,
	})
	if err != nil {
		return fmt.Errorf("construct logger: %w", err)
	}
	defer func() { _ = log.Sync() }()
	// Reserve the address synchronously, before opening expensive dependencies.
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", cfg.App.HTTPAddress)
	if err != nil {
		return fmt.Errorf("bind HTTP listener: %w", err)
	}
	defer listener.Close()
	pool, err := db.Open(ctx, db.Config{
		DatabaseURL: cfg.Database.URL, MaxConns: cfg.Database.MaxConns,
		MinConns: cfg.Database.MinConns, ConnectTimeout: cfg.Database.ConnectTimeout,
		ApplicationName: cfg.App.ServiceName,
	})
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()
	server, err := api.New(api.Config{Address: cfg.App.HTTPAddress}, pool, log)
	if err != nil {
		return fmt.Errorf("construct API: %w", err)
	}
	log.Info("HTTP server listening", zap.String("address", listener.Addr().String()))
	if err := server.Run(ctx, listener, cfg.App.ShutdownGrace); err != nil {
		return err
	}
	log.Info("HTTP server stopped")
	return nil
}
