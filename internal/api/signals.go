package api

import (
	"encoding/json"
	"github.com/kartikey/workflow_service/pkg/httpx"
	"net/http"
	"strings"

	"github.com/kartikey/workflow_service/pkg/auth"
	"github.com/kartikey/workflow_service/pkg/exception"
	"github.com/kartikey/workflow_service/pkg/uid"
)

// signal is POST /v1/signals (04 §4.1). A duplicate signal for a fulfilled
// wait returns 200 with the original result; no live waiter is 404 no_waiter;
// a payload failing the wait's schema is 422.
func (h *Handler) signal(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var req struct {
		Key     string          `json:"key"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	if strings.TrimSpace(req.Key) == "" {
		h.writeError(w, r, exception.BadRequest("key is required"))
		return
	}
	result, err := h.services.Runs.Signal(r.Context(), p.TenantID, req.Key, req.Payload)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"run_id": uid.String(result.RunID), "node_id": result.NodeID, "duplicate": result.Duplicate,
	})
}
