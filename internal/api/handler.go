// Package api contains the /be/v1 HTTP handlers. Handlers stay thin: logic lives
// in services so it is reachable from tests and other entry points
// (04 §6). Server lifecycle, authentication and metrics live in package server.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/internal/services/agents"
	"github.com/kartikey/workflow_service/internal/services/connections"
	"github.com/kartikey/workflow_service/internal/services/identity"
	"github.com/kartikey/workflow_service/internal/services/providers"
	"github.com/kartikey/workflow_service/internal/services/runs"
	"github.com/kartikey/workflow_service/internal/services/tasks"
	"github.com/kartikey/workflow_service/internal/services/vault"
	"github.com/kartikey/workflow_service/internal/services/workflows"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/auth"
	"github.com/kartikey/workflow_service/pkg/events"
	"go.uber.org/zap"
)

// HandlerFunc serves one authenticated request.
type HandlerFunc func(http.ResponseWriter, *http.Request, auth.Principal)

// Router registers handlers behind authentication and a minimum role. The
// server package implements it.
type Router interface {
	Handle(pattern, role string, h HandlerFunc)
	// HandlePublic registers a route that needs no authentication (sign-in,
	// accepting an invite). The handler receives no principal.
	HandlePublic(pattern string, h func(http.ResponseWriter, *http.Request))
	// HandleAdmin registers a route for platform administrators only.
	HandleAdmin(pattern string, h HandlerFunc)
}

// WorkflowService is the workflow use-cases the handlers need.
type WorkflowService interface {
	Publish(ctx context.Context, p workflows.PublishParams) (workflows.PublishResult, error)
	List(ctx context.Context, tenantID pgtype.UUID, includeArchived bool) ([]gen.ListWorkflowsRow, error)
	Get(ctx context.Context, tenantID pgtype.UUID, name string) (workflows.Detail, error)
	GetVersion(ctx context.Context, tenantID pgtype.UUID, name string, number int32) (workflows.VersionDetail, error)
	SetArchived(ctx context.Context, tenantID pgtype.UUID, name string, archived bool) error
}

// RunService is the run use-cases the handlers need.
type RunService interface {
	Start(ctx context.Context, p runs.StartParams) (runs.StartResult, error)
	Get(ctx context.Context, tenantID, id pgtype.UUID) (gen.GetRunDetailRow, error)
	Progress(ctx context.Context, row gen.GetRunDetailRow) (map[string]int, error)
	List(ctx context.Context, params gen.ListRunsParams, limit int) ([]gen.ListRunsRow, bool, error)
	Nodes(ctx context.Context, tenantID, id pgtype.UUID) ([]gen.NodeExecution, error)
	Events(ctx context.Context, tenantID, id pgtype.UUID, after, limit int32) ([]gen.RunEvent, error)
	Status(ctx context.Context, tenantID, id pgtype.UUID) (string, error)
	Cancel(ctx context.Context, tenantID, id pgtype.UUID, reason string) error
	Retry(ctx context.Context, tenantID, id pgtype.UUID, actor, idempotencyKey string) (runs.StartResult, error)
	Signal(ctx context.Context, tenantID pgtype.UUID, key string, payload json.RawMessage) (runs.SignalResult, error)
}

// TaskService is the human-task worklist use-cases the handlers need.
type TaskService interface {
	List(ctx context.Context, f tasks.Filter, limit int) ([]gen.TaskDetail, bool, error)
	Get(ctx context.Context, tenantID, id pgtype.UUID) (gen.TaskDetail, error)
	Claim(ctx context.Context, tenantID, id pgtype.UUID, actor tasks.Actor) (gen.TaskDetail, error)
	Release(ctx context.Context, tenantID, id pgtype.UUID, actor tasks.Actor) (gen.TaskDetail, error)
	Complete(ctx context.Context, tenantID, id pgtype.UUID, actor tasks.Actor, payload json.RawMessage) (runs.SignalResult, error)
}

// IdentityService is the accounts, organization and platform use-cases.
type IdentityService interface {
	Login(ctx context.Context, email, password string) (identity.Session, error)
	Logout(ctx context.Context, sessionID pgtype.UUID) error
	Me(ctx context.Context, p auth.Principal) (identity.User, identity.Membership, []identity.Membership, error)
	SwitchOrg(ctx context.Context, p auth.Principal, orgID pgtype.UUID) (identity.Membership, error)
	PreviewInvite(ctx context.Context, token string) (identity.Invite, bool, error)
	AcceptInvite(ctx context.Context, token, password, name string) (identity.Session, error)

	ListMembers(ctx context.Context, orgID pgtype.UUID) ([]identity.Member, error)
	UpdateMemberRole(ctx context.Context, actor auth.Principal, userID pgtype.UUID, role string) error
	RemoveMember(ctx context.Context, actor auth.Principal, userID pgtype.UUID) error
	CreateInvite(ctx context.Context, actor auth.Principal, email, role string) (identity.Invite, string, error)
	ListInvites(ctx context.Context, orgID pgtype.UUID) ([]identity.Invite, error)
	RevokeInvite(ctx context.Context, orgID, id pgtype.UUID) error
	CreateAPIKey(ctx context.Context, actor auth.Principal, name, role string) (identity.APIKey, string, error)
	ListAPIKeys(ctx context.Context, orgID pgtype.UUID) ([]identity.APIKey, error)
	RevokeAPIKey(ctx context.Context, orgID, id pgtype.UUID) error
	Usage(ctx context.Context, orgID pgtype.UUID) (identity.Usage, error)
	ListPlans(ctx context.Context) ([]identity.PlanInfo, error)

	CreatePlatformInvite(ctx context.Context, actor auth.Principal, email, orgName string) (identity.Invite, string, error)
	ListOrganizations(ctx context.Context, limit int) ([]identity.OrgSummary, error)
	SetOrganizationPlan(ctx context.Context, orgID pgtype.UUID, planID string) error
}

// VaultService is the secrets use-cases.
type VaultService interface {
	Put(ctx context.Context, orgID pgtype.UUID, name, value, actor string) error
	PutGroup(ctx context.Context, orgID pgtype.UUID, group string, keys map[string]string, actor string) error
	List(ctx context.Context, orgID pgtype.UUID) ([]vault.Info, error)
	Delete(ctx context.Context, orgID pgtype.UUID, name string) error
	Rotate(ctx context.Context) (vault.RotateResult, error)
	KeyVersions(ctx context.Context) (map[int]int64, error)
	Current() int
}

// ProviderService manages LLM provider connections.
type ProviderService interface {
	Test(ctx context.Context, in providers.TestInput) (providers.TestResult, error)
	Create(ctx context.Context, orgID pgtype.UUID, actor string, in providers.CreateInput) (providers.Connection, providers.TestResult, error)
	List(ctx context.Context, orgID pgtype.UUID) ([]providers.Connection, error)
	TestStored(ctx context.Context, orgID, id pgtype.UUID) (providers.TestResult, error)
	Delete(ctx context.Context, orgID, id pgtype.UUID) error
}

// AgentService manages reusable agent definitions.
type AgentService interface {
	Create(ctx context.Context, orgID pgtype.UUID, actor string, in agents.Input) (agents.Agent, error)
	Update(ctx context.Context, orgID pgtype.UUID, actor, name string, in agents.Input) (agents.Agent, error)
	Get(ctx context.Context, orgID pgtype.UUID, name string) (agents.Agent, error)
	List(ctx context.Context, orgID pgtype.UUID) ([]agents.Agent, error)
	Delete(ctx context.Context, orgID pgtype.UUID, name string) error
	Test(ctx context.Context, tenantID pgtype.UUID, name string, input, prompt json.RawMessage) (agents.TestResult, error)
}

// ConnectionService manages connections to external services.
type ConnectionService interface {
	Kinds() []connections.KindInfo
	TestInput(ctx context.Context, in connections.Input) error
	Create(ctx context.Context, orgID pgtype.UUID, actor string, in connections.Input) (connections.Connection, error)
	List(ctx context.Context, orgID pgtype.UUID) ([]connections.Connection, error)
	Get(ctx context.Context, orgID pgtype.UUID, name string) (connections.Connection, error)
	Update(ctx context.Context, orgID pgtype.UUID, name string, in connections.Input) (connections.Connection, error)
	Delete(ctx context.Context, orgID pgtype.UUID, name string) error
	Test(ctx context.Context, orgID pgtype.UUID, name string) error
	StartOAuth(ctx context.Context, orgID, userID pgtype.UUID, kind, name string) (string, error)
	CompleteOAuth(ctx context.Context, state, code, providerError string) (string, error)
}

// AdminService is the operator interventions for stuck work (04 §5.1).
type AdminService interface {
	RetryNode(ctx context.Context, nodeExecutionID pgtype.UUID, actor string) (runs.NodeAdminResult, error)
	SkipNode(ctx context.Context, nodeExecutionID pgtype.UUID, actor string) (runs.NodeAdminResult, error)
	ReleaseWorkerLeases(ctx context.Context, workerID string) (int, error)
}

// ExecutorCatalog describes the registered node types.
type ExecutorCatalog interface {
	Descriptions() []executor.Description
}

// Services are the dependencies of the /be/v1 API.
type Services struct {
	// Pool carries the LISTEN connection behind server-sent events.
	Pool      *pgxpool.Pool
	Workflows WorkflowService
	Runs      RunService
	Executors ExecutorCatalog
	// Identity enables sign-in, members, invites, API keys and plans; nil
	// leaves those routes unregistered (the development authenticator needs none).
	Identity IdentityService
	// CookieSecure marks the session cookie Secure (set it behind HTTPS).
	CookieSecure bool
	// PublicURL is the externally visible base URL, used to build invite links.
	PublicURL string
	// Vault enables /be/v1/secrets; nil leaves them unregistered.
	Vault VaultService
	// Connections enables /be/v1/connections and /be/v1/integrations.
	Connections ConnectionService
	// Providers and Agents enable /be/v1/providers and /be/v1/agents.
	Providers ProviderService
	Agents    AgentService
	// Assistant and Triggers enable draft generation and webhook routes.
	Assistant AssistantService
	Triggers  TriggerService
	// Tasks enables the /be/v1/tasks routes; nil leaves them unregistered.
	Tasks TaskService
	// Admin enables the /be/v1/admin routes; nil leaves them unregistered.
	Admin AdminService
	Auth  auth.Authenticator
}

// Validate reports a missing required service.
func (s *Services) Validate() error {
	if s.Workflows == nil || s.Runs == nil || s.Executors == nil || s.Auth == nil {
		return errors.New("API services require workflows, runs, executors and auth")
	}
	return nil
}

// Handler serves the /be/v1 API.
type Handler struct {
	services *Services
	events   *events.Hub
	logger   *zap.Logger
	// closing ends SSE streams when shutdown starts, so they do not hold the
	// drain open until the grace period expires.
	closing <-chan struct{}
}

// NewHandler builds the handlers. closing may be nil when streams never need
// to be cut short.
func NewHandler(services *Services, hub *events.Hub, closing <-chan struct{}, logger *zap.Logger) *Handler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Handler{services: services, events: hub, logger: logger, closing: closing}
}

// Register mounts every /be/v1 route on r.
func (h *Handler) Register(r Router) {
	r.Handle("POST /be/v1/workflows", auth.RoleEditor, h.publishWorkflow)
	r.Handle("GET /be/v1/workflows", auth.RoleViewer, h.listWorkflows)
	r.Handle("GET /be/v1/workflows/{name}", auth.RoleViewer, h.getWorkflow)
	r.Handle("GET /be/v1/workflows/{name}/versions/{version}", auth.RoleViewer, h.getWorkflowVersion)
	r.Handle("DELETE /be/v1/workflows/{name}", auth.RoleEditor, h.archiveWorkflow)
	r.Handle("POST /be/v1/workflows/{name}/restore", auth.RoleEditor, h.restoreWorkflow)

	r.Handle("POST /be/v1/runs", auth.RoleEditor, h.createRun)
	r.Handle("GET /be/v1/runs", auth.RoleViewer, h.listRuns)
	r.Handle("GET /be/v1/runs/{id}", auth.RoleViewer, h.getRun)
	r.Handle("GET /be/v1/runs/{id}/nodes", auth.RoleViewer, h.listRunNodes)
	r.Handle("GET /be/v1/runs/{id}/events", auth.RoleViewer, h.runEvents)
	r.Handle("POST /be/v1/runs/{id}/cancel", auth.RoleEditor, h.cancelRun)
	r.Handle("POST /be/v1/runs/{id}/retry", auth.RoleEditor, h.retryRun)

	r.Handle("POST /be/v1/signals", auth.RoleEditor, h.signal)
	r.Handle("GET /be/v1/executors", auth.RoleViewer, h.listExecutors)

	if h.services.Assistant != nil {
		r.Handle("POST /be/v1/assistant/workflows", auth.RoleEditor, h.assistantWorkflow)
	}
	if h.services.Triggers != nil {
		r.HandlePublic("POST /hooks/{token}", h.webhook)
		r.Handle("POST /be/v1/triggers", auth.RoleAdmin, h.createTrigger)
		r.Handle("GET /be/v1/triggers", auth.RoleEditor, h.listTriggers)
		r.Handle("PATCH /be/v1/triggers/{id}", auth.RoleAdmin, h.updateTrigger)
		r.Handle("DELETE /be/v1/triggers/{id}", auth.RoleAdmin, h.deleteTrigger)
		r.Handle("POST /be/v1/triggers/{id}/rotate-token", auth.RoleAdmin, h.rotateTrigger)
	}
	if h.services.Tasks != nil {
		r.Handle("GET /be/v1/tasks", auth.RoleViewer, h.listTasks)
		r.Handle("GET /be/v1/tasks/{id}", auth.RoleViewer, h.getTask)
		r.Handle("POST /be/v1/tasks/{id}/claim", auth.RoleEditor, h.claimTask)
		r.Handle("POST /be/v1/tasks/{id}/release", auth.RoleEditor, h.releaseTask)
		r.Handle("POST /be/v1/tasks/{id}/complete", auth.RoleEditor, h.completeTask)
	}

	if h.services.Identity != nil {
		h.registerIdentity(r)
	}
	if h.services.Connections != nil {
		r.Handle("GET /be/v1/integrations", auth.RoleEditor, h.listIntegrations)
		r.Handle("POST /be/v1/connections/oauth/start", auth.RoleAdmin, h.startOAuth)
		r.HandlePublic("GET /be/v1/oauth/callback", h.oauthCallback)
		r.Handle("POST /be/v1/connections/test", auth.RoleAdmin, h.testConnectionInput)
		r.Handle("POST /be/v1/connections", auth.RoleAdmin, h.createConnection)
		r.Handle("GET /be/v1/connections", auth.RoleEditor, h.listConnections)
		r.Handle("GET /be/v1/connections/{name}", auth.RoleEditor, h.getConnection)
		r.Handle("PUT /be/v1/connections/{name}", auth.RoleAdmin, h.updateConnection)
		r.Handle("DELETE /be/v1/connections/{name}", auth.RoleAdmin, h.deleteConnection)
		r.Handle("POST /be/v1/connections/{name}/test", auth.RoleAdmin, h.testConnection)
	}
	if h.services.Providers != nil {
		r.Handle("POST /be/v1/providers/test", auth.RoleAdmin, h.testProvider)
		r.Handle("POST /be/v1/providers", auth.RoleAdmin, h.createProvider)
		r.Handle("GET /be/v1/providers", auth.RoleEditor, h.listProviders)
		r.Handle("POST /be/v1/providers/{id}/test", auth.RoleAdmin, h.testStoredProvider)
		r.Handle("DELETE /be/v1/providers/{id}", auth.RoleAdmin, h.deleteProvider)
	}
	if h.services.Agents != nil {
		r.Handle("GET /be/v1/agent-tools", auth.RoleViewer, h.listAgentTools)
		r.Handle("GET /be/v1/agents", auth.RoleViewer, h.listAgents)
		r.Handle("POST /be/v1/agents", auth.RoleEditor, h.createAgent)
		r.Handle("GET /be/v1/agents/{name}", auth.RoleViewer, h.getAgent)
		r.Handle("PUT /be/v1/agents/{name}", auth.RoleEditor, h.updateAgent)
		r.Handle("DELETE /be/v1/agents/{name}", auth.RoleEditor, h.deleteAgent)
		r.Handle("POST /be/v1/agents/{name}/test", auth.RoleEditor, h.testAgent)
	}
	if h.services.Vault != nil {
		r.Handle("GET /be/v1/secrets", auth.RoleEditor, h.listSecrets)
		r.Handle("PUT /be/v1/secrets/{name}", auth.RoleAdmin, h.putSecret)
		r.Handle("DELETE /be/v1/secrets/{name}", auth.RoleAdmin, h.deleteSecret)
		r.HandleAdmin("POST /be/v1/admin/vault/rotate", h.rotateVault)
		r.HandleAdmin("GET /be/v1/admin/vault", h.vaultStatus)
	}

	if h.services.Admin != nil {
		r.HandleAdmin("POST /be/v1/admin/node-executions/{id}/retry", h.adminRetryNode)
		r.HandleAdmin("POST /be/v1/admin/node-executions/{id}/skip", h.adminSkipNode)
		r.HandleAdmin("POST /be/v1/admin/workers/{worker_id}/release-leases", h.adminReleaseLeases)
	}

}
