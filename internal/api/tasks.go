package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/services/runs"
	"github.com/kartikey/workflow_service/internal/services/tasks"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/auth"
	"github.com/kartikey/workflow_service/pkg/exception"
	"github.com/kartikey/workflow_service/pkg/httpx"
	"github.com/kartikey/workflow_service/pkg/uid"
)

func taskView(t gen.TaskDetail) map[string]any {
	return map[string]any{
		"id": uid.String(t.ID), "status": t.Status, "queue": t.Queue, "form": httpx.Text(t.Form),
		"assignee": httpx.Text(t.Assignee), "claimed_by": httpx.Text(t.ClaimedBy), "completed_by": httpx.Text(t.CompletedBy),
		"run_id": uid.String(t.RunID), "workflow": t.WorkflowName, "node_id": t.NodeID,
		"payload": httpx.Raw(t.Payload), "due_at": httpx.Time(t.DueAt),
		"created_at": httpx.Time(t.CreatedAt), "claimed_at": httpx.Time(t.ClaimedAt),
	}
}

func taskID(r *http.Request) (pgtype.UUID, error) {
	id, err := uid.Parse(r.PathValue("id"))
	if err != nil {
		return pgtype.UUID{}, exception.NotFound("task")
	}
	return id, nil
}

func taskActor(p auth.Principal) tasks.Actor {
	return tasks.Actor{Name: p.Actor(), Admin: p.Can(auth.RoleAdmin)}
}

// listTasks is GET /v1/tasks (04 §4.2): the worklist, keyset paginated, with
// ?queue=, ?status=, ?assignee= and ?mine=true (tasks claimed by the caller).
func (h *Handler) listTasks(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	limit, err := queryLimit(r, 50, 200)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	filter := tasks.Filter{
		TenantID: p.TenantID, Queue: r.URL.Query().Get("queue"),
		Status: r.URL.Query().Get("status"), Assignee: r.URL.Query().Get("assignee"),
	}
	if r.URL.Query().Get("mine") == "true" {
		filter.ClaimedBy = p.Actor()
	}
	if after := r.URL.Query().Get("after"); after != "" {
		if filter.BeforeCreated, filter.BeforeID, err = decodeCursor(after); err != nil {
			h.writeError(w, r, err)
			return
		}
	}
	rows, more, err := h.services.Tasks.List(r.Context(), filter, limit)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	var next *string
	if more {
		last := rows[len(rows)-1]
		cursor := encodeCursor(last.CreatedAt, last.ID)
		next = &cursor
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, taskView(row))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next": next})
}

func (h *Handler) getTask(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	id, err := taskID(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	task, err := h.services.Tasks.Get(r.Context(), p.TenantID, id)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, taskView(task))
}

func (h *Handler) claimTask(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	h.taskTransition(w, r, p, h.services.Tasks.Claim)
}

func (h *Handler) releaseTask(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	h.taskTransition(w, r, p, h.services.Tasks.Release)
}

func (h *Handler) taskTransition(w http.ResponseWriter, r *http.Request, p auth.Principal,
	do func(ctx context.Context, tenantID, id pgtype.UUID, actor tasks.Actor) (gen.TaskDetail, error)) {
	id, err := taskID(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	task, err := do(r.Context(), p.TenantID, id, taskActor(p))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, taskView(task))
}

// completeTask is POST /v1/tasks/{id}/complete (04 §4.2): sugar over the signal
// that the task's wait is parked on. The body is {"payload": ...}; the payload
// becomes the node's output.
func (h *Handler) completeTask(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	id, err := taskID(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	var req struct {
		Payload json.RawMessage `json:"payload"`
	}
	if r.ContentLength != 0 {
		if err := httpx.DecodeJSON(r, &req); err != nil {
			h.writeError(w, r, err)
			return
		}
	}
	result, err := h.services.Tasks.Complete(r.Context(), p.TenantID, id, taskActor(p), req.Payload)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, completionView(id, result))
}

func completionView(id pgtype.UUID, result runs.SignalResult) map[string]any {
	return map[string]any{
		"id": uid.String(id), "status": "completed", "run_id": uid.String(result.RunID),
		"node_id": result.NodeID, "duplicate": result.Duplicate,
	}
}
