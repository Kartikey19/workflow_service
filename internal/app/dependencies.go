package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kartikey/workflow_service/internal/api"
	"github.com/kartikey/workflow_service/internal/config"
	"github.com/kartikey/workflow_service/internal/core/engine"
	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/internal/core/executor/agent"
	"github.com/kartikey/workflow_service/internal/core/executor/builtin"
	"github.com/kartikey/workflow_service/internal/core/executor/httpexec"
	"github.com/kartikey/workflow_service/internal/services/runs"
	"github.com/kartikey/workflow_service/internal/services/workflows"
	"github.com/kartikey/workflow_service/pkg/auth"
	"github.com/kartikey/workflow_service/pkg/llm"
	"github.com/kartikey/workflow_service/pkg/llm/anthropic"
	"github.com/kartikey/workflow_service/pkg/netguard"
	"go.uber.org/zap"
)

// Deps are the process-wide services shared by the API and the worker.
type Deps struct {
	Pool      *pgxpool.Pool
	Log       *zap.Logger
	Registry  *executor.Registry
	Workflows *workflows.Service
	Runs      *runs.Service
	Auth      auth.Authenticator
	Secrets   engine.SecretSource
	Extra     []func(api.Router)
}

// Build wires the shared services from configuration.
func Build(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, log *zap.Logger) (*Deps, error) {
	policy, err := EgressPolicy(cfg.Egress)
	if err != nil {
		return nil, err
	}
	registry := executor.NewRegistry()
	for _, e := range append(builtin.All(), httpexec.New(policy)) {
		if err := registry.Register(e); err != nil {
			return nil, err
		}
	}
	svc, err := runs.New(pool, log)
	if err != nil {
		return nil, err
	}
	if cfg.Worker.MaxNodesPerRun > 0 {
		svc.MaxNodesPerRun = cfg.Worker.MaxNodesPerRun
	}
	deps := &Deps{
		Pool: pool, Log: log, Registry: registry, Runs: svc,
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
		Pool: d.Pool, Workflows: d.Workflows, Runs: d.Runs, Executors: d.Registry,
		Admin: d.Runs, Auth: d.Auth, Extra: d.Extra,
	}
}

// Worker builds one worker sharing the process run service and registry.
func (d *Deps) Worker(cfg config.WorkerConfig) *engine.Worker {
	engineCfg := engine.Config{
		ID: cfg.ID, Concurrency: cfg.Concurrency, BatchSize: cfg.BatchSize,
		PollInterval: cfg.PollInterval, LeaseTTL: cfg.LeaseTTL, SweepInterval: cfg.SweepInterval,
	}
	dispatcher := engine.NewDispatcher(engineCfg, d.Runs, d.Registry, d.Secrets, d.Log)
	return engine.NewWorker(engineCfg, d.Log, d.Pool, d.Runs, dispatcher.Run)
}

// newAgentExecutor wires the agent node to the platform model key. The tool
// registry starts with the harmless built-ins; data and MCP tools join it in M15.
func newAgentExecutor(cfg config.LLMConfig) (*agent.Executor, error) {
	tools, err := agent.NewTools(agent.CurrentTime{})
	if err != nil {
		return nil, err
	}
	if cfg.AnthropicAPIKey == "" {
		return agent.New(agent.Unconfigured{}, tools), nil
	}
	pricing, err := llm.ParsePricing(cfg.PricingJSON)
	if err != nil {
		return nil, fmt.Errorf("LLM_PRICING_JSON: %w", err)
	}
	client := anthropic.New(cfg.AnthropicAPIKey, cfg.AnthropicBaseURL, nil)
	return agent.New(agent.NewStatic("anthropic", client, pricing), tools), nil
}
