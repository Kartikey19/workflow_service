package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/api/problem"
	"github.com/kartikey/workflow_service/internal/auth"
	"github.com/kartikey/workflow_service/internal/flow"
	"github.com/kartikey/workflow_service/internal/ids"
	"github.com/kartikey/workflow_service/internal/store/gen"
)

type createRunRequest struct {
	Workflow      string         `json:"workflow"`
	Version       int32          `json:"version"`
	Input         map[string]any `json:"input"`
	CorrelationID string         `json:"correlation_id"`
}

// createRun is POST /v1/runs (04 §3.1): 201 for a new run, 200 when the
// Idempotency-Key already exists, 409 when it was used with another body.
func (s *Server) createRun(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var req createRunRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if req.Workflow == "" {
		s.writeError(w, r, problem.BadRequest("workflow is required"))
		return
	}
	if req.Version < 0 {
		s.writeError(w, r, problem.BadRequest("version must be positive"))
		return
	}
	trigger := "api"
	if p.Via == "session" {
		trigger = "ui"
	}
	result, err := s.StartRun(r, p, flow.StartParams{
		TenantID: p.TenantID, Workflow: req.Workflow, Version: req.Version, Input: req.Input,
		CorrelationID: req.CorrelationID, IdempotencyKey: r.Header.Get("Idempotency-Key"),
		Trigger: trigger, CreatedBy: p.Actor(),
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	status := http.StatusCreated
	if !result.Created {
		status = http.StatusOK
	}
	writeJSON(w, status, runSummary(result.Run, result.WorkflowName, result.Version))
}

// StartRun applies the monthly run allowance, then starts the run. An
// idempotent replay of an existing run is not charged again. Webhook triggers
// (M16) share it.
func (s *Server) StartRun(r *http.Request, p auth.Principal, params flow.StartParams) (flow.StartResult, error) {
	if s.services.Plans != nil && !s.isReplay(r, params) {
		if err := s.services.Plans.CheckRun(r.Context(), p.TenantID); err != nil {
			return flow.StartResult{}, err
		}
	}
	return s.services.Flow.Start(r.Context(), params)
}

func (s *Server) isReplay(r *http.Request, params flow.StartParams) bool {
	if params.IdempotencyKey == "" {
		return false
	}
	_, err := s.services.Flow.Q.GetRunByIdempotencyKey(r.Context(), gen.GetRunByIdempotencyKeyParams{
		TenantID: params.TenantID, IdempotencyKey: pgtype.Text{String: params.IdempotencyKey, Valid: true},
	})
	return err == nil
}

func runSummary(run gen.Run, workflow string, version int32) map[string]any {
	return map[string]any{
		"id": ids.String(run.ID), "status": run.Status, "workflow": workflow, "version": version,
		"correlation_id": textJSON(run.CorrelationID), "trigger": run.Trigger,
		"parent_run_id": nullableID(run.ParentRunID), "created_by": textJSON(run.CreatedBy),
		"created_at": timeJSON(run.CreatedAt), "started_at": timeJSON(run.StartedAt),
		"ended_at": timeJSON(run.EndedAt), "deadline_at": timeJSON(run.DeadlineAt),
	}
}

func nullableID(id pgtype.UUID) *string {
	if !id.Valid {
		return nil
	}
	text := ids.String(id)
	return &text
}

func (s *Server) loadRun(r *http.Request, p auth.Principal) (gen.GetRunDetailRow, error) {
	id, err := ids.Parse(r.PathValue("id"))
	if err != nil {
		return gen.GetRunDetailRow{}, problem.NotFound("run")
	}
	// Every lookup is tenant-scoped: another organization's run is a 404.
	row, err := s.services.Flow.Q.GetRunDetail(r.Context(), gen.GetRunDetailParams{ID: id, TenantID: p.TenantID})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.GetRunDetailRow{}, problem.NotFound("run")
	}
	return row, err
}

// getRun is GET /v1/runs/{id} (04 §3.2) with a progress summary over the latest
// attempt of every node; total is the spec's node count, an upper bound.
func (s *Server) getRun(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	row, err := s.loadRun(r, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	view := runSummary(row.Run, row.WorkflowName, row.WorkflowVersion)
	view["input"] = rawJSON(row.Run.Input)
	view["output"] = rawJSON(row.Run.Output)
	view["error"] = rawJSON(row.Run.Error)
	progress := map[string]int{"total": 0, "succeeded": 0, "failed": 0, "skipped": 0, "running": 0, "waiting": 0, "ready": 0, "cancelled": 0}
	if compiled, err := s.services.Flow.Specs.Get(r.Context(), s.services.Flow.Q, row.Run.WorkflowVersionID, p.TenantID); err == nil {
		progress["total"] = len(compiled.Spec.Nodes)
	}
	states, err := s.services.Flow.Q.LatestNodeStates(r.Context(), row.Run.ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	for _, state := range states {
		progress[state.Status]++
	}
	view["progress"] = progress
	writeJSON(w, http.StatusOK, view)
}

// runCursor is the opaque keyset cursor over (created_at, id) descending.
type runCursor struct {
	CreatedAt time.Time `json:"t"`
	ID        string    `json:"id"`
}

func encodeCursor(run gen.Run) string {
	data, _ := json.Marshal(runCursor{CreatedAt: run.CreatedAt.Time, ID: ids.String(run.ID)})
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeCursor(text string) (pgtype.Timestamptz, pgtype.UUID, error) {
	data, err := base64.RawURLEncoding.DecodeString(text)
	var cursor runCursor
	if err == nil {
		err = json.Unmarshal(data, &cursor)
	}
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, problem.BadRequest("after is not a valid cursor")
	}
	id, err := ids.Parse(cursor.ID)
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, problem.BadRequest("after is not a valid cursor")
	}
	return pgtype.Timestamptz{Time: cursor.CreatedAt, Valid: true}, id, nil
}

func queryLimit(r *http.Request, fallback, maximum int) (int, error) {
	text := r.URL.Query().Get("limit")
	if text == "" {
		return fallback, nil
	}
	limit, err := strconv.Atoi(text)
	if err != nil || limit < 1 || limit > maximum {
		return 0, problem.BadRequest(fmt.Sprintf("limit must be between 1 and %d", maximum))
	}
	return limit, nil
}

func optionalQueryTime(r *http.Request, name string) (pgtype.Timestamptz, error) {
	text := r.URL.Query().Get(name)
	if text == "" {
		return pgtype.Timestamptz{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return pgtype.Timestamptz{}, problem.BadRequest(name + " must be an RFC 3339 timestamp")
	}
	return pgtype.Timestamptz{Time: parsed, Valid: true}, nil
}

func optionalQueryText(r *http.Request, name string) pgtype.Text {
	text := r.URL.Query().Get(name)
	return pgtype.Text{String: text, Valid: text != ""}
}

// listRuns is GET /v1/runs (04 §3.3), keyset paginated, never offset.
func (s *Server) listRuns(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	limit, err := queryLimit(r, 50, 200)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	params := gen.ListRunsParams{
		TenantID: p.TenantID, WorkflowName: optionalQueryText(r, "workflow"),
		Status: optionalQueryText(r, "status"), CorrelationID: optionalQueryText(r, "correlation_id"),
		Lim: int32(limit + 1),
	}
	if params.CreatedAfter, err = optionalQueryTime(r, "created_after"); err != nil {
		s.writeError(w, r, err)
		return
	}
	if params.CreatedBefore, err = optionalQueryTime(r, "created_before"); err != nil {
		s.writeError(w, r, err)
		return
	}
	if after := r.URL.Query().Get("after"); after != "" {
		if params.BeforeCreated, params.BeforeID, err = decodeCursor(after); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	rows, err := s.services.Flow.Q.ListRuns(r.Context(), params)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var next *string
	if len(rows) > limit {
		rows = rows[:limit]
		cursor := encodeCursor(rows[limit-1].Run)
		next = &cursor
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		item := runSummary(row.Run, row.WorkflowName, row.WorkflowVersion)
		item["error"] = rawJSON(row.Run.Error)
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next": next})
}

// listRunNodes is GET /v1/runs/{id}/nodes (04 §3.4): every attempt, in order.
func (s *Server) listRunNodes(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	row, err := s.loadRun(r, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	nodes, err := s.services.Flow.Q.ListRunNodes(r.Context(), gen.ListRunNodesParams{RunID: row.Run.ID, TenantID: p.TenantID})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	includeInput := strings.Contains(r.URL.Query().Get("include"), "input")
	items := make([]map[string]any, 0, len(nodes))
	for _, node := range nodes {
		item := map[string]any{
			"id": ids.String(node.ID), "node_id": node.NodeID, "node_type": node.NodeType,
			"branch_key": node.BranchKey, "attempt": node.Attempt, "status": node.Status,
			"output": rawJSON(node.Output), "error": rawJSON(node.Error), "metrics": rawJSON(node.Metrics),
			"run_after": timeJSON(node.RunAfter), "created_at": timeJSON(node.CreatedAt),
			"started_at": timeJSON(node.StartedAt), "ended_at": timeJSON(node.EndedAt),
		}
		if includeInput {
			// Secrets were redacted to *** before the input was stored.
			item["input"] = rawJSON(node.Input)
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// cancelRun is POST /v1/runs/{id}/cancel (04 §3.6): 202, because running nodes
// stop cooperatively at their next heartbeat.
func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var req struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &req); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	id, err := ids.Parse(r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, problem.NotFound("run"))
		return
	}
	if err := s.services.Flow.Cancel(r.Context(), p.TenantID, id, req.Reason); err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"id": ids.String(id), "status": "cancelled",
		"detail": "ready and waiting nodes were cancelled; a running node stops at its next heartbeat and its result is discarded",
	})
}

// retryRun is POST /v1/runs/{id}/retry (04 §3.7): a new run of the same version
// and input with parent_run_id set; deliberately not a resume.
func (s *Server) retryRun(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	row, err := s.loadRun(r, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var input map[string]any
	if err := json.Unmarshal(row.Run.Input, &input); err != nil {
		s.writeError(w, r, fmt.Errorf("decode run input: %w", err))
		return
	}
	result, err := s.StartRun(r, p, flow.StartParams{
		TenantID: p.TenantID, Workflow: row.WorkflowName, Version: row.WorkflowVersion, Input: input,
		CorrelationID: row.Run.CorrelationID.String, IdempotencyKey: r.Header.Get("Idempotency-Key"),
		Trigger: "retry", CreatedBy: p.Actor(), ParentRunID: row.Run.ID,
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, runSummary(result.Run, result.WorkflowName, result.Version))
}
