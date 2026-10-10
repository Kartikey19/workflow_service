package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kartikey/workflow_service/internal/api/problem"
	"github.com/kartikey/workflow_service/internal/auth"
	"github.com/kartikey/workflow_service/internal/executor"
	"github.com/kartikey/workflow_service/internal/flow"
	"github.com/kartikey/workflow_service/internal/obs"
	"github.com/kartikey/workflow_service/internal/workflows"
	"go.uber.org/zap"
)

// maxBodyBytes bounds request bodies: workflow YAML, run input, signal payloads.
const maxBodyBytes = 1 << 20

// Plans enforces plan limits per organization (M11). Nil means unlimited.
type Plans interface {
	PublishLimits(ctx context.Context, tenantID pgtype.UUID) (workflows.Limits, error)
	// CheckRun returns a *workflows.LimitError when the monthly run allowance is used.
	CheckRun(ctx context.Context, tenantID pgtype.UUID) error
}

// Services are the dependencies of the /v1 API. Handlers stay thin: logic
// lives in flow and workflows so it is reachable from the CLI and tests (04 §6).
type Services struct {
	Pool      *pgxpool.Pool
	Flow      *flow.Flow
	Workflows *workflows.Service
	Registry  *executor.Registry
	Auth      auth.Authenticator
	Plans     Plans
	// Extra registers routes added by later milestones (identity, vault,
	// agents, webhooks, UI). It receives the authenticated route helper.
	Extra []func(mux *http.ServeMux, r Router)
}

// Router registers handlers behind authentication and a minimum role.
type Router struct{ s *Server }

// Handle registers pattern for principals with at least role.
func (r Router) Handle(mux *http.ServeMux, pattern, role string, h func(http.ResponseWriter, *http.Request, auth.Principal)) {
	mux.Handle(pattern, r.s.authed(role, h))
}

// Problem writes an error, mapping known domain errors to their status.
func (r Router) Problem(w http.ResponseWriter, req *http.Request, err error) {
	r.s.writeError(w, req, err)
}

func (s *Server) routesV1(mux *http.ServeMux) {
	r := Router{s}
	mux.Handle("GET /metrics", obs.Handler())

	r.Handle(mux, "POST /v1/workflows", auth.RoleEditor, s.publishWorkflow)
	r.Handle(mux, "GET /v1/workflows", auth.RoleViewer, s.listWorkflows)
	r.Handle(mux, "GET /v1/workflows/{name}", auth.RoleViewer, s.getWorkflow)
	r.Handle(mux, "GET /v1/workflows/{name}/versions/{version}", auth.RoleViewer, s.getWorkflowVersion)
	r.Handle(mux, "DELETE /v1/workflows/{name}", auth.RoleEditor, s.archiveWorkflow)
	r.Handle(mux, "POST /v1/workflows/{name}/restore", auth.RoleEditor, s.restoreWorkflow)

	r.Handle(mux, "POST /v1/runs", auth.RoleEditor, s.createRun)
	r.Handle(mux, "GET /v1/runs", auth.RoleViewer, s.listRuns)
	r.Handle(mux, "GET /v1/runs/{id}", auth.RoleViewer, s.getRun)
	r.Handle(mux, "GET /v1/runs/{id}/nodes", auth.RoleViewer, s.listRunNodes)
	r.Handle(mux, "GET /v1/runs/{id}/events", auth.RoleViewer, s.runEvents)
	r.Handle(mux, "POST /v1/runs/{id}/cancel", auth.RoleEditor, s.cancelRun)
	r.Handle(mux, "POST /v1/runs/{id}/retry", auth.RoleEditor, s.retryRun)

	r.Handle(mux, "POST /v1/signals", auth.RoleEditor, s.signal)
	r.Handle(mux, "GET /v1/executors", auth.RoleViewer, s.listExecutors)

	for _, register := range s.services.Extra {
		register(mux, r)
	}
}

// authed resolves the principal and checks the minimum role.
func (s *Server) authed(role string, h func(http.ResponseWriter, *http.Request, auth.Principal)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			obs.HTTPRequests.WithLabelValues(r.Pattern, strconv.Itoa(recorder.status)).Observe(time.Since(started).Seconds())
		}()
		principal, err := s.services.Auth.Authenticate(r)
		if err != nil {
			problem.Write(recorder, r, problem.Unauthorized())
			return
		}
		if !principal.Can(role) {
			problem.Write(recorder, r, problem.Forbidden(fmt.Sprintf("this action needs the %s role or higher", role)))
			return
		}
		r.Body = http.MaxBytesReader(recorder, r.Body, maxBodyBytes)
		h(recorder, r.WithContext(auth.WithPrincipal(r.Context(), principal)), principal)
	})
}

// statusRecorder captures the status for metrics and keeps Flush for SSE.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// writeError maps domain errors to problems; anything unknown is a logged 500
// with no internal detail in the body.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var p *problem.Problem
	var compileErr *workflows.CompileError
	var limitErr *workflows.LimitError
	var inputErr *flow.InputError
	var payloadErr *flow.PayloadError
	var maxBytes *http.MaxBytesError
	switch {
	case errors.As(err, &p):
	case errors.As(err, &compileErr):
		p = problem.New(http.StatusBadRequest, "spec_invalid", "Workflow specification is invalid", compileErr.Error())
		p.Errors = errorDiagnostics(compileErr)
	case errors.As(err, &limitErr):
		p = problem.PaymentRequired(limitErr.Error() + "; upgrade the plan or archive unused workflows")
	case errors.As(err, &inputErr):
		p = problem.New(http.StatusBadRequest, "input_invalid", "Run input is invalid", inputErr.Error())
		p.Errors = inputErr.Fields
	case errors.As(err, &payloadErr):
		p = problem.New(http.StatusUnprocessableEntity, "payload_invalid", "Signal payload does not match the wait schema", payloadErr.Error())
		p.Errors = payloadErr.Fields
	case errors.As(err, &maxBytes):
		p = problem.New(http.StatusRequestEntityTooLarge, "too_large", "Request body is too large", fmt.Sprintf("the limit is %d bytes", maxBytes.Limit))
	case errors.Is(err, flow.ErrNotFound):
		p = problem.NotFound("the requested resource")
	case errors.Is(err, flow.ErrArchived), errors.Is(err, workflows.ErrArchived):
		p = problem.Conflict("workflow_archived", "the workflow is archived; restore it or publish under a new name")
	case errors.Is(err, flow.ErrIdempotencyConflict):
		p = problem.Conflict("idempotency_conflict", "this Idempotency-Key was already used with a different request body")
	case errors.Is(err, flow.ErrRunTerminal):
		p = problem.Conflict("run_terminal", "the run has already finished")
	case errors.Is(err, flow.ErrNoWaiter):
		p = problem.New(http.StatusNotFound, "no_waiter", "No live waiter for this key",
			"no run is waiting on this key; signals that arrive before the wait are rejected, so retry later")
	case errors.Is(err, auth.ErrForbidden):
		p = problem.Forbidden("your role does not allow this action")
	case errors.Is(err, context.Canceled):
		return
	default:
		s.logger.Error("request failed", zap.String("route", r.Pattern), zap.Error(err))
		p = problem.Internal()
	}
	problem.Write(w, r, p)
}

func errorDiagnostics(err *workflows.CompileError) []any {
	out := make([]any, 0, len(err.Diagnostics))
	for _, d := range err.Diagnostics {
		if d.Severity == "error" {
			out = append(out, d)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// decodeJSON reads one JSON object, rejecting unknown fields and trailing data.
func decodeJSON(r *http.Request, into any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			return err
		}
		if errors.Is(err, io.EOF) {
			return problem.BadRequest("request body is required")
		}
		return problem.BadRequest("invalid JSON: " + err.Error())
	}
	if decoder.More() {
		return problem.BadRequest("request body must contain one JSON object")
	}
	return nil
}

// Time is RFC 3339 UTC with millisecond precision (04 §1); null when unset.
func timeJSON(t pgtype.Timestamptz) *string {
	if !t.Valid {
		return nil
	}
	formatted := t.Time.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	return &formatted
}

func textJSON(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

// rawJSON passes stored JSONB through; SQL NULL becomes JSON null.
func rawJSON(data []byte) json.RawMessage {
	if len(data) == 0 {
		return json.RawMessage("null")
	}
	return json.RawMessage(data)
}

