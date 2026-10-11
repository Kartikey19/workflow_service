package api

import (
	"github.com/kartikey/workflow_service/pkg/httpx"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/services/runs"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/auth"
	"github.com/kartikey/workflow_service/pkg/exception"
	"github.com/kartikey/workflow_service/pkg/uid"
)

type createRunRequest struct {
	Workflow      string         `json:"workflow"`
	Version       int32          `json:"version"`
	Input         map[string]any `json:"input"`
	CorrelationID string         `json:"correlation_id"`
}

// createRun is POST /v1/runs (04 §3.1): 201 for a new run, 200 when the
// Idempotency-Key already exists, 409 when it was used with another body.
func (h *Handler) createRun(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var req createRunRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	if req.Workflow == "" {
		h.writeError(w, r, exception.BadRequest("workflow is required"))
		return
	}
	if req.Version < 0 {
		h.writeError(w, r, exception.BadRequest("version must be positive"))
		return
	}
	trigger := "api"
	if p.Via == "session" {
		trigger = "ui"
	}
	result, err := h.services.Runs.Start(r.Context(), runs.StartParams{
		TenantID: p.TenantID, Workflow: req.Workflow, Version: req.Version, Input: req.Input,
		CorrelationID: req.CorrelationID, IdempotencyKey: r.Header.Get("Idempotency-Key"),
		Trigger: trigger, CreatedBy: p.Actor(),
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	status := http.StatusCreated
	if !result.Created {
		status = http.StatusOK
	}
	httpx.WriteJSON(w, status, runSummary(result.Run, result.WorkflowName, result.Version))
}

// runID parses the {id} path value; a malformed ID is a 404, like an unknown one.
func runID(r *http.Request) (pgtype.UUID, error) {
	id, err := uid.Parse(r.PathValue("id"))
	if err != nil {
		return pgtype.UUID{}, exception.NotFound("run")
	}
	return id, nil
}

func (h *Handler) loadRun(r *http.Request, p auth.Principal) (gen.GetRunDetailRow, error) {
	id, err := runID(r)
	if err != nil {
		return gen.GetRunDetailRow{}, err
	}
	return h.services.Runs.Get(r.Context(), p.TenantID, id)
}

// getRun is GET /v1/runs/{id} (04 §3.2) with a progress summary over the latest
// attempt of every node; total is the spec's node count, an upper bound.
func (h *Handler) getRun(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	row, err := h.loadRun(r, p)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	view := runSummary(row.Run, row.WorkflowName, row.WorkflowVersion)
	view["input"] = httpx.Raw(row.Run.Input)
	view["output"] = httpx.Raw(row.Run.Output)
	view["error"] = httpx.Raw(row.Run.Error)
	progress, err := h.services.Runs.Progress(r.Context(), row)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	view["progress"] = progress
	httpx.WriteJSON(w, http.StatusOK, view)
}

// listRuns is GET /v1/runs (04 §3.3), keyset paginated, never offset.
func (h *Handler) listRuns(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	limit, err := queryLimit(r, 50, 200)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	params := gen.ListRunsParams{
		TenantID: p.TenantID, WorkflowName: optionalQueryText(r, "workflow"),
		Status: optionalQueryText(r, "status"), CorrelationID: optionalQueryText(r, "correlation_id"),
	}
	if parent := r.URL.Query().Get("parent_run_id"); parent != "" {
		if params.ParentRunID, err = uid.Parse(parent); err != nil {
			h.writeError(w, r, exception.BadRequest("parent_run_id must be a run ID"))
			return
		}
	}
	if params.CreatedAfter, err = optionalQueryTime(r, "created_after"); err != nil {
		h.writeError(w, r, err)
		return
	}
	if params.CreatedBefore, err = optionalQueryTime(r, "created_before"); err != nil {
		h.writeError(w, r, err)
		return
	}
	if after := r.URL.Query().Get("after"); after != "" {
		if params.BeforeCreated, params.BeforeID, err = decodeCursor(after); err != nil {
			h.writeError(w, r, err)
			return
		}
	}
	rows, more, err := h.services.Runs.List(r.Context(), params, limit)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	var next *string
	if more {
		last := rows[len(rows)-1].Run
		cursor := encodeCursor(last.CreatedAt, last.ID)
		next = &cursor
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		item := runSummary(row.Run, row.WorkflowName, row.WorkflowVersion)
		item["error"] = httpx.Raw(row.Run.Error)
		items = append(items, item)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next": next})
}

// listRunNodes is GET /v1/runs/{id}/nodes (04 §3.4): every attempt, in order.
func (h *Handler) listRunNodes(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	id, err := runID(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	nodes, err := h.services.Runs.Nodes(r.Context(), p.TenantID, id)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	includeInput := strings.Contains(r.URL.Query().Get("include"), "input")
	items := make([]map[string]any, 0, len(nodes))
	for _, node := range nodes {
		item := map[string]any{
			"id": uid.String(node.ID), "node_id": node.NodeID, "node_type": node.NodeType,
			"branch_key": node.BranchKey, "attempt": node.Attempt, "status": node.Status,
			"output": httpx.Raw(node.Output), "error": httpx.Raw(node.Error), "metrics": httpx.Raw(node.Metrics),
			"run_after": httpx.Time(node.RunAfter), "created_at": httpx.Time(node.CreatedAt),
			"started_at": httpx.Time(node.StartedAt), "ended_at": httpx.Time(node.EndedAt),
		}
		if includeInput {
			// Secrets were redacted to *** before the input was stored.
			item["input"] = httpx.Raw(node.Input)
		}
		items = append(items, item)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// cancelRun is POST /v1/runs/{id}/cancel (04 §3.6): 202, because running nodes
// stop cooperatively at their next heartbeat.
func (h *Handler) cancelRun(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var req struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 {
		if err := httpx.DecodeJSON(r, &req); err != nil {
			h.writeError(w, r, err)
			return
		}
	}
	id, err := runID(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if err := h.services.Runs.Cancel(r.Context(), p.TenantID, id, req.Reason); err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{
		"id": uid.String(id), "status": "cancelled",
		"detail": "ready and waiting nodes were cancelled; a running node stops at its next heartbeat and its result is discarded",
	})
}

// retryRun is POST /v1/runs/{id}/retry (04 §3.7): a new run of the same version
// and input with parent_run_id set; deliberately not a resume.
func (h *Handler) retryRun(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	id, err := runID(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	result, err := h.services.Runs.Retry(r.Context(), p.TenantID, id, p.Actor(), r.Header.Get("Idempotency-Key"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, runSummary(result.Run, result.WorkflowName, result.Version))
}
