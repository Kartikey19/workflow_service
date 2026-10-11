// Package app composes the service and owns its dependencies' lifetimes.
package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/kartikey/workflow_service/internal/config"
	"github.com/kartikey/workflow_service/internal/server"
	"github.com/kartikey/workflow_service/pkg/db"
	"github.com/kartikey/workflow_service/pkg/logger"
	"github.com/kartikey/workflow_service/pkg/obs"
	"go.uber.org/zap"
)

// Run constructs dependencies, serves, then releases resources in reverse order.
// Mode api serves HTTP; mode worker runs the claim loop and sweeper.
func Run(ctx context.Context, cfg config.Config) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate configuration: %w", err)
	}
	log, err := logger.NewLogger(logger.Config{
		ServiceName: cfg.App.ServiceName, Environment: cfg.App.Environment,
		Version: cfg.App.BuildVersion, Level: cfg.Log.LogLevel, AddSource: cfg.Log.AddSource,
	})
	if err != nil {
		return fmt.Errorf("construct logger: %w", err)
	}
	defer func() { _ = log.Sync() }()
	serveAPI := cfg.App.Mode == "api"
	runWorker := cfg.App.Mode == "worker"

	// Reserve addresses synchronously, before opening expensive dependencies.
	var lc net.ListenConfig
	var listener, metricsListener net.Listener
	if serveAPI {
		if listener, err = lc.Listen(ctx, "tcp", cfg.App.HTTPAddress); err != nil {
			return fmt.Errorf("bind HTTP listener: %w", err)
		}
		defer listener.Close()
	}
	if runWorker && cfg.Worker.MetricsAddress != "" {
		if metricsListener, err = lc.Listen(ctx, "tcp", cfg.Worker.MetricsAddress); err != nil {
			return fmt.Errorf("bind worker metrics listener: %w", err)
		}
		defer metricsListener.Close()
	}
	pool, err := db.Open(ctx, db.Config{
		DatabaseURL: cfg.Database.URL, MaxConns: cfg.Database.MaxConns,
		MinConns: cfg.Database.MinConns, ConnectTimeout: cfg.Database.ConnectTimeout,
		ApplicationName: cfg.App.ServiceName,
	})
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()

	deps, err := Build(ctx, cfg, pool, log)
	if err != nil {
		return err
	}

	var components []func(context.Context) error
	if serveAPI {
		srv, err := server.New(server.Config{Address: cfg.App.HTTPAddress, API: deps.Services()}, pool, log)
		if err != nil {
			return fmt.Errorf("construct API: %w", err)
		}
		log.Info("HTTP server listening", zap.String("address", listener.Addr().String()))
		components = append(components, func(ctx context.Context) error {
			if err := srv.Run(ctx, listener, cfg.App.ShutdownGrace); err != nil {
				return err
			}
			log.Info("HTTP server stopped")
			return nil
		})
	}
	if runWorker {
		worker := deps.Worker(cfg.Worker)
		log.Info("worker started", zap.String("worker_id", cfg.Worker.ID), zap.Int("concurrency", cfg.Worker.Concurrency))
		components = append(components, func(ctx context.Context) error {
			if err := worker.Run(ctx, cfg.App.ShutdownGrace); err != nil {
				return err
			}
			log.Info("worker stopped")
			return nil
		})
		obs.RegisterEngineGauges(pool)
	}
	if metricsListener != nil {
		// The worker's own /metrics and /healthz (WORKER_METRICS_ADDRESS).
		srv, err := server.New(server.Config{Address: cfg.Worker.MetricsAddress, Metrics: true}, pool, log)
		if err != nil {
			return fmt.Errorf("construct worker metrics server: %w", err)
		}
		log.Info("worker metrics listening", zap.String("address", metricsListener.Addr().String()))
		components = append(components, func(ctx context.Context) error {
			return srv.Run(ctx, metricsListener, cfg.App.ShutdownGrace)
		})
	}
	return runAll(ctx, components)
}

// runAll runs components until ctx ends or one fails; a failure stops the rest.
func runAll(ctx context.Context, components []func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make([]error, len(components))
	var wg sync.WaitGroup
	for i, component := range components {
		wg.Go(func() {
			errs[i] = component(ctx)
			cancel()
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}
