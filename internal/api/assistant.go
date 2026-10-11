package api

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/services/assistant"
	"github.com/kartikey/workflow_service/pkg/auth"
	"github.com/kartikey/workflow_service/pkg/httpx"
)

type AssistantService interface {
	Generate(context.Context, pgtype.UUID, string, assistant.Input) (assistant.Draft, error)
}

func (h *Handler) assistantWorkflow(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var in assistant.Input
	if err := httpx.DecodeJSON(r, &in); err != nil {
		h.writeError(w, r, err)
		return
	}
	draft, err := h.services.Assistant.Generate(r.Context(), p.TenantID, p.Actor(), in)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	status := 200
	if !draft.Valid {
		status = 422
	}
	httpx.WriteJSON(w, status, draft)
}
