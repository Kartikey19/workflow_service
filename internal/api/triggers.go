package api

import (
	"context"
	"io"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/services/runs"
	"github.com/kartikey/workflow_service/internal/services/triggers"
	"github.com/kartikey/workflow_service/pkg/auth"
	"github.com/kartikey/workflow_service/pkg/httpx"
)

type TriggerService interface {
	Create(context.Context, pgtype.UUID, triggers.Input) (triggers.Created, error)
	List(context.Context, pgtype.UUID) ([]triggers.Info, error)
	Update(context.Context, pgtype.UUID, pgtype.UUID, *bool, string) (triggers.Info, error)
	Delete(context.Context, pgtype.UUID, pgtype.UUID) error
	RotateToken(context.Context, pgtype.UUID, pgtype.UUID) (string, error)
	Fire(context.Context, string, string, string, []byte, http.Header, map[string][]string) (runs.StartResult, error)
}

func (h *Handler) createTrigger(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var in triggers.Input
	if err := httpx.DecodeJSON(r, &in); err != nil {
		h.writeError(w, r, err)
		return
	}
	out, err := h.services.Triggers.Create(r.Context(), p.TenantID, in)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, 201, out)
}
func (h *Handler) listTriggers(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	out, err := h.services.Triggers.List(r.Context(), p.TenantID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, 200, map[string]any{"items": out})
}
func (h *Handler) updateTrigger(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	id, err := runID(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	var in struct {
		Enabled   *bool  `json:"enabled"`
		InputMode string `json:"input_mode"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		h.writeError(w, r, err)
		return
	}
	out, err := h.services.Triggers.Update(r.Context(), p.TenantID, id, in.Enabled, in.InputMode)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, 200, out)
}
func (h *Handler) deleteTrigger(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	id, err := runID(r)
	if err == nil {
		err = h.services.Triggers.Delete(r.Context(), p.TenantID, id)
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	w.WriteHeader(204)
}
func (h *Handler) rotateTrigger(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	id, err := runID(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	token, err := h.services.Triggers.RotateToken(r.Context(), p.TenantID, id)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, 200, map[string]any{"token": token})
}
func (h *Handler) webhook(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	out, err := h.services.Triggers.Fire(r.Context(), r.PathValue("token"), r.Header.Get("X-Workflow-Signature"), r.Header.Get("Idempotency-Key"), raw, r.Header, r.URL.Query())
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	status := 201
	if !out.Created {
		status = 200
	}
	httpx.WriteJSON(w, status, runSummary(out.Run, out.WorkflowName, out.Version))
}
