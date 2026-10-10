package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/kartikey/workflow_service/internal/spec"
	"go.uber.org/zap"
)

type PublishWorkflowRequest struct {
	Workflow string `json:"workflow"`
}

func (s *Server) handleWorkflow(w http.ResponseWriter, r *http.Request) {
	var req PublishWorkflowRequest
	//  check the name in workflow , it is unique per tenant
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.logger.Error("failed to decode request", zap.Error(err))
		s.writeStatus(w, http.StatusBadRequest, "invalid request")
		return
	}

	_, diagnostics := spec.Compile([]byte(req.Workflow))
	if len(diagnostics) > 0 {
		s.logger.Error("failed to compile workflow", zap.Any("diagnostics", diagnostics))
		s.writeStatus(w, http.StatusBadRequest, fmt.Sprintf("invalid workflow: %v", diagnostics))
		return
	}

	// TODO: save the workflow to the database later, send the diagnostics for now

	s.writeStatus(w, http.StatusOK, "workflow published")
}
