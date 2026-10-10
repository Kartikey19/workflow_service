package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/kartikey/workflow_service/internal/api/problem"
	"github.com/kartikey/workflow_service/internal/auth"
	"github.com/kartikey/workflow_service/internal/ids"
)

// signal is POST /v1/signals (04 §4.1). A duplicate signal for a fulfilled
// wait returns 200 with the original result; no live waiter is 404 no_waiter;
// a payload failing the wait's schema is 422.
func (s *Server) signal(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var req struct {
		Key     string          `json:"key"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if strings.TrimSpace(req.Key) == "" {
		s.writeError(w, r, problem.BadRequest("key is required"))
		return
	}
	result, err := s.services.Flow.Signal(r.Context(), p.TenantID, req.Key, req.Payload)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run_id": ids.String(result.RunID), "node_id": result.NodeID, "duplicate": result.Duplicate,
	})
}

// listExecutors is GET /v1/executors: the registered node types, so editors,
// docs and the workflow assistant stay in sync with the binary.
func (s *Server) listExecutors(w http.ResponseWriter, _ *http.Request, _ auth.Principal) {
	writeJSON(w, http.StatusOK, map[string]any{"items": s.services.Registry.Descriptions()})
}
