package api

import (
	"go.uber.org/zap"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/services/runs"
	"github.com/kartikey/workflow_service/pkg/auth"
	"github.com/kartikey/workflow_service/pkg/exception"
	"github.com/kartikey/workflow_service/pkg/httpx"
	"github.com/kartikey/workflow_service/pkg/uid"
)

func nodeExecutionID(r *http.Request) (pgtype.UUID, error) {
	id, err := uid.Parse(r.PathValue("id"))
	if err != nil {
		return pgtype.UUID{}, exception.NotFound("node execution")
	}
	return id, nil
}

func nodeAdminView(result runs.NodeAdminResult) map[string]any {
	return map[string]any{
		"run_id": uid.String(result.RunID), "node_id": result.NodeID, "branch_key": result.BranchKey,
		"attempt": result.Attempt, "status": result.Status,
	}
}

// adminRetryNode is POST /be/v1/admin/node-executions/{id}/retry (04 §5.1).
func (h *Handler) adminRetryNode(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	id, err := nodeExecutionID(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	result, err := h.services.Admin.RetryNode(r.Context(), id, p.Actor())
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, nodeAdminView(result))
}

// adminSkipNode is POST /be/v1/admin/node-executions/{id}/skip (04 §5.1).
func (h *Handler) adminSkipNode(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	id, err := nodeExecutionID(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	result, err := h.services.Admin.SkipNode(r.Context(), id, p.Actor())
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, nodeAdminView(result))
}

// adminReleaseLeases is POST /be/v1/admin/workers/{worker_id}/release-leases.
func (h *Handler) adminReleaseLeases(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	workerID := strings.TrimSpace(r.PathValue("worker_id"))
	if workerID == "" {
		h.writeError(w, r, exception.BadRequest("worker_id is required"))
		return
	}
	released, err := h.services.Admin.ReleaseWorkerLeases(r.Context(), workerID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	h.logger.Info("admin released worker leases", zap.String("worker_id", workerID),
		zap.Int("released", released), zap.String("actor", p.Actor()))
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"worker_id": workerID, "released": released})
}
