package app

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kartikey/workflow_service/internal/api"
	"github.com/kartikey/workflow_service/internal/config"
	"github.com/kartikey/workflow_service/internal/core/engine"
	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/internal/core/executor/agent"
	"github.com/kartikey/workflow_service/internal/core/executor/builtin"
	"github.com/kartikey/workflow_service/internal/core/executor/connectors"
	"github.com/kartikey/workflow_service/internal/core/executor/database"
	"github.com/kartikey/workflow_service/internal/core/executor/email"
	"github.com/kartikey/workflow_service/internal/core/executor/httpexec"
	"github.com/kartikey/workflow_service/internal/services/agents"
	"github.com/kartikey/workflow_service/internal/services/connections"
	"github.com/kartikey/workflow_service/internal/services/identity"
	"github.com/kartikey/workflow_service/internal/services/providers"
	"github.com/kartikey/workflow_service/internal/services/runs"
	"github.com/kartikey/workflow_service/internal/services/tasks"
	"github.com/kartikey/workflow_service/internal/services/vault"
	"github.com/kartikey/workflow_service/internal/services/workflows"
	"github.com/kartikey/workflow_service/pkg/auth"
	"github.com/kartikey/workflow_service/pkg/llm"
	"github.com/kartikey/workflow_service/pkg/llm/anthropic"
	"github.com/kartikey/workflow_service/pkg/netguard"
	vaultcrypto "github.com/kartikey/workflow_service/pkg/vault"
	"go.uber.org/zap"
)

// Deps are the process-wide services shared by the API and the worker.
type Deps struct {
	Pool      *pgxpool.Pool
	Log       *zap.Logger
	Registry  *executor.Registry
	Workflows *workflows.Service
	Runs      *runs.Service
	Tasks     *tasks.Service
	Identity  *identity.Service
	Vault     *vault.Service
	Providers *providers.Service
	Agents    *agents.Service
	Conns     *connections.Service
	Auth      auth.Authenticator
	Secrets   engine.SecretSource
	Extra     []func(api.Router)

	cfgAuth config.AuthConfig
}

// Build wires the shared services from configuration. The API and the worker
// both call it, so a node type or service exists in both or in neither.
func Build(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, log *zap.Logger) (*Deps, error) {
	policy, err := EgressPolicy(cfg.Egress)
	if err != nil {
		return nil, err
	}
	svc, err := runs.New(pool, log)
	if err != nil {
		return nil, err
	}
	if cfg.Worker.MaxNodesPerRun > 0 {
		svc.MaxNodesPerRun = cfg.Worker.MaxNodesPerRun
	}

	// Secrets: the vault master key seals both vault entries and provider keys.
	var keyring *vaultcrypto.Keyring
	if cfg.Vault.MasterKey != "" {
		if keyring, err = vaultcrypto.ParseKeyring(cfg.Vault.MasterKey, cfg.Vault.KeyVersion, cfg.Vault.OldKeys); err != nil {
			return nil, err
		}
	}
	vaultSvc := vault.NewService(pool, keyring, log)

	// Agents: a registry of tools, the platform provider, tenant-owned provider
	// connections and stored agent definitions.
	pricing, err := llm.ParsePricing(cfg.LLM.PricingJSON)
	if err != nil {
		return nil, fmt.Errorf("LLM_PRICING_JSON: %w", err)
	}
	tools, err := agent.NewTools(agent.CurrentTime{}, agent.NewHTTPRequest(policy))
	if err != nil {
		return nil, err
	}
	var platform agent.Clients = agent.Unconfigured{}
	if cfg.LLM.AnthropicAPIKey != "" {
		client := anthropic.New(cfg.LLM.AnthropicAPIKey, cfg.LLM.AnthropicBaseURL, nil)
		platform = agent.NewStatic("anthropic", client, pricing)
	}
	providerSvc := providers.NewService(pool, keyring, policy, pricing).WithBaseURLs(map[string]string{
		providers.Anthropic: cfg.LLM.AnthropicBaseURL, providers.OpenAI: cfg.LLM.OpenAIBaseURL, providers.Gemini: cfg.LLM.GeminiBaseURL,
	})
	agentSvc := agents.NewService(pool, tools)
	agentExecutor := agent.New(agent.Router{Named: providerSvc, Default: platform}, tools, agentSvc)
	agentSvc.Attach(agentExecutor)

	endpoints, err := connectors.ParseEndpoints(cfg.Services.IntegrationEndpoints)
	if err != nil {
		return nil, err
	}
	connectionSvc := connections.NewService(pool, keyring, policy, connections.Options{
		ResendURL: cfg.Services.ResendBaseURL, PublicURL: cfg.Auth.PublicURL, Endpoints: endpoints,
		Google:          connections.OAuthApp{ClientID: cfg.Services.GoogleClientID, ClientSecret: cfg.Services.GoogleClientSecret},
		Dropbox:         connections.OAuthApp{ClientID: cfg.Services.DropboxAppKey, ClientSecret: cfg.Services.DropboxAppSecret},
		Microsoft:       connections.OAuthApp{ClientID: cfg.Services.MicrosoftClientID, ClientSecret: cfg.Services.MicrosoftClientSecret},
		MicrosoftTenant: cfg.Services.MicrosoftTenant,
	})
	emailExecutor := email.New(connectionSvc, policy, cfg.Services.ResendBaseURL)
	appNodes := []executor.Executor{
		connectors.NewGmail(connectionSvc, policy, endpoints), connectors.NewGDrive(connectionSvc, policy, endpoints),
		connectors.NewDropbox(connectionSvc, policy, endpoints), connectors.NewOneDrive(connectionSvc, policy, endpoints),
		database.NewQuery(connectionSvc, policy), database.NewInsert(connectionSvc, policy),
	}

	registry := executor.NewRegistry()
	nodes := append(builtin.All(), httpexec.New(policy), agentExecutor, builtin.SubWorkflow{Starter: svc}, emailExecutor)
	for _, e := range append(nodes, appNodes...) {
		if err := registry.Register(e); err != nil {
			return nil, err
		}
	}

	ident, err := identity.NewService(pool, identity.Config{SessionTTL: cfg.Auth.SessionTTL, CookieSecure: cfg.Auth.CookieSecure})
	if err != nil {
		return nil, err
	}
	deps := &Deps{
		Pool: pool, Log: log, Registry: registry, Runs: svc, Tasks: tasks.NewService(svc),
		Workflows: workflows.NewService(pool, registry), Identity: ident, Vault: vaultSvc,
		Providers: providerSvc, Agents: agentSvc, Conns: connectionSvc, cfgAuth: cfg.Auth,
	}
	deps.Workflows.Plans = ident.Plans()
	deps.Runs.Allowance = ident.Plans()
	if keyring != nil {
		deps.Secrets = vaultSvc // workers decrypt a run's secrets on demand
	}
	// Sessions and API keys always work. The development header (an X-Tenant-ID
	// that is trusted blindly) exists only outside production.
	deps.Auth = ident.Authenticator()
	if cfg.App.Environment != "production" {
		deps.Auth = auth.Chain{ident.Authenticator(), auth.DevHeader{}}
	}
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
		Tasks: d.Tasks, Admin: d.Runs, Identity: d.Identity, Vault: d.Vault,
		Providers: d.Providers, Agents: d.Agents, Connections: d.Conns,
		CookieSecure: d.cfgAuth.CookieSecure, PublicURL: d.cfgAuth.PublicURL, Auth: d.Auth, Extra: d.Extra,
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
