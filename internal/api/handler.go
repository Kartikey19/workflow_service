// Package api contains the /v1 HTTP handlers. Handlers stay thin: logic lives
// in flow and workflows so it is reachable from tests and other entry points
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
	"github.com/kartikey/workflow_service/internal/services/runs"
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

// Services are the dependencies of the /v1 API.
type Services struct {
	// Pool carries the LISTEN connection behind server-sent events.
	Pool      *pgxpool.Pool
	Workflows WorkflowService
	Runs      RunService
	Executors ExecutorCatalog
	// Admin enables the /v1/admin routes; nil leaves them unregistered.
	Admin AdminService
	Auth  auth.Authenticator
	// Extra registers routes added by later milestones (identity, vault,
	// agents, webhooks, UI).
	Extra []func(Router)
}

// Validate reports a missing required service.
func (s *Services) Validate() error {
	if s.Workflows == nil || s.Runs == nil || s.Executors == nil || s.Auth == nil {
		return errors.New("API services require workflows, runs, executors and auth")
	}
	return nil
}

// Handler serves the /v1 API.
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

// Register mounts every /v1 route on r.
func (h *Handler) Register(r Router) {
	r.Handle("POST /v1/workflows", auth.RoleEditor, h.publishWorkflow)
	r.Handle("GET /v1/workflows", auth.RoleViewer, h.listWorkflows)
	r.Handle("GET /v1/workflows/{name}", auth.RoleViewer, h.getWorkflow)
	r.Handle("GET /v1/workflows/{name}/versions/{version}", auth.RoleViewer, h.getWorkflowVersion)
	r.Handle("DELETE /v1/workflows/{name}", auth.RoleEditor, h.archiveWorkflow)
	r.Handle("POST /v1/workflows/{name}/restore", auth.RoleEditor, h.restoreWorkflow)

	r.Handle("POST /v1/runs", auth.RoleEditor, h.createRun)
	r.Handle("GET /v1/runs", auth.RoleViewer, h.listRuns)
	r.Handle("GET /v1/runs/{id}", auth.RoleViewer, h.getRun)
	r.Handle("GET /v1/runs/{id}/nodes", auth.RoleViewer, h.listRunNodes)
	r.Handle("GET /v1/runs/{id}/events", auth.RoleViewer, h.runEvents)
	r.Handle("POST /v1/runs/{id}/cancel", auth.RoleEditor, h.cancelRun)
	r.Handle("POST /v1/runs/{id}/retry", auth.RoleEditor, h.retryRun)

	r.Handle("POST /v1/signals", auth.RoleEditor, h.signal)
	r.Handle("GET /v1/executors", auth.RoleViewer, h.listExecutors)

	if h.services.Admin != nil {
		r.HandleAdmin("POST /v1/admin/node-executions/{id}/retry", h.adminRetryNode)
		r.HandleAdmin("POST /v1/admin/node-executions/{id}/skip", h.adminSkipNode)
		r.HandleAdmin("POST /v1/admin/workers/{worker_id}/release-leases", h.adminReleaseLeases)
	}

	for _, register := range h.services.Extra {
		register(r)
	}
}
