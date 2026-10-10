// Package app composes the service and owns its dependencies' lifetimes.
package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kartikey/workflow_service/internal/api"
	"github.com/kartikey/workflow_service/internal/auth"
	"github.com/kartikey/workflow_service/internal/config"
	"github.com/kartikey/workflow_service/internal/db"
	"github.com/kartikey/workflow_service/internal/engine"
	"github.com/kartikey/workflow_service/internal/executor"
	"github.com/kartikey/workflow_service/internal/executors/core"
	"github.com/kartikey/workflow_service/internal/executors/httpexec"
	"github.com/kartikey/workflow_service/internal/flow"
	"github.com/kartikey/workflow_service/internal/logger"
	"github.com/kartikey/workflow_service/internal/netguard"
	"github.com/kartikey/workflow_service/internal/obs"
	"github.com/kartikey/workflow_service/internal/workflows"
	"go.uber.org/zap"
)

// Run constructs dependencies, serves, then releases resources in reverse order.
// APP_MODE=api serves HTTP, worker runs the claim loop and sweeper, and all runs
// both in one process for local development.
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
	serveAPI := cfg.App.Mode != "worker"
	runWorker := cfg.App.Mode != "api"

	// Reserve addresses synchronously, before opening expensive dependencies.
	var lc net.ListenConfig
	var listener, metricsListener net.Listener
	if serveAPI {
		if listener, err = lc.Listen(ctx, "tcp", cfg.App.HTTPAddress); err != nil {
			return fmt.Errorf("bind HTTP listener: %w", err)
		}
		defer listener.Close()
	}
	if runWorker && !serveAPI && cfg.Worker.MetricsAddress != "" {
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
		server, err := api.New(api.Config{Address: cfg.App.HTTPAddress, Services: deps.Services()}, pool, log)
		if err != nil {
			return fmt.Errorf("construct API: %w", err)
		}
		log.Info("HTTP server listening", zap.String("address", listener.Addr().String()))
		components = append(components, func(ctx context.Context) error {
			if err := server.Run(ctx, listener, cfg.App.ShutdownGrace); err != nil {
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
		server, err := api.New(api.Config{Address: cfg.Worker.MetricsAddress, Metrics: true}, pool, log)
		if err != nil {
			return fmt.Errorf("construct worker metrics server: %w", err)
		}
		log.Info("worker metrics listening", zap.String("address", metricsListener.Addr().String()))
		components = append(components, func(ctx context.Context) error {
			return server.Run(ctx, metricsListener, cfg.App.ShutdownGrace)
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

// Deps are the process-wide services shared by the API and the worker.
type Deps struct {
	Pool      *pgxpool.Pool
	Log       *zap.Logger
	Registry  *executor.Registry
	Flow      *flow.Flow
	Workflows *workflows.Service
	Auth      auth.Authenticator
	Plans     api.Plans
	Secrets   engine.SecretSource
	Extra     []func(*http.ServeMux, api.Router)
}

// Build wires the shared services from configuration.
func Build(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, log *zap.Logger) (*Deps, error) {
	policy, err := EgressPolicy(cfg.Egress)
	if err != nil {
		return nil, err
	}
	registry := executor.NewRegistry()
	for _, e := range append(core.All(), httpexec.New(policy)) {
		if err := registry.Register(e); err != nil {
			return nil, err
		}
	}
	f, err := flow.New(pool, log)
	if err != nil {
		return nil, err
	}
	if cfg.Worker.MaxNodesPerRun > 0 {
		f.MaxNodesPerRun = cfg.Worker.MaxNodesPerRun
	}
	deps := &Deps{
		Pool: pool, Log: log, Registry: registry, Flow: f,
		Workflows: workflows.NewService(pool, registry),
	}
	if cfg.App.Environment == "production" {
		return nil, errors.New("production requires authentication; development header auth is disabled")
	}
	deps.Auth = auth.DevHeader{}
	return deps, nil
}

// EgressPolicy builds the netguard policy for every outbound tenant call.
func EgressPolicy(cfg config.EgressConfig) (netguard.Policy, error) {
	allow, err := netguard.ParseAllow(cfg.AllowCIDRs)
	if err != nil {
		return netguard.Policy{}, fmt.Errorf("EGRESS_ALLOW_CIDRS: %w", err)
	}
	return netguard.Policy{AllowPrivate: cfg.AllowPrivate, Allow: allow}, nil
}

// Services returns the API dependencies.
func (d *Deps) Services() *api.Services {
	return &api.Services{
		Pool: d.Pool, Flow: d.Flow, Workflows: d.Workflows, Registry: d.Registry,
		Auth: d.Auth, Plans: d.Plans, Extra: d.Extra,
	}
}

// Worker builds one worker sharing the process flow and registry.
func (d *Deps) Worker(cfg config.WorkerConfig) *engine.Worker {
	dispatcher := engine.NewDispatcher(cfg, d.Flow, d.Registry, d.Secrets, d.Log)
	return engine.NewWorker(cfg, d.Log, d.Pool, d.Flow, dispatcher.Run)
}
