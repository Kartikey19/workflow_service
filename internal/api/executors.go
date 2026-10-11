package api

import (
	"github.com/kartikey/workflow_service/pkg/auth"
	"github.com/kartikey/workflow_service/pkg/httpx"
	"net/http"
)

// listExecutors is GET /be/v1/executors: the registered node types, so editors,
// docs and the workflow assistant stay in sync with the binary.
func (h *Handler) listExecutors(w http.ResponseWriter, _ *http.Request, _ auth.Principal) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": h.services.Executors.Descriptions()})
}
