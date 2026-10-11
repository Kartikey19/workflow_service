package api

import (
	"net/http"
	"time"

	"github.com/kartikey/workflow_service/internal/services/connections"
	"github.com/kartikey/workflow_service/pkg/auth"
	"github.com/kartikey/workflow_service/pkg/httpx"
	"github.com/kartikey/workflow_service/pkg/uid"
)

func connectionInfoView(c connections.Connection) map[string]any {
	view := map[string]any{
		"id": uid.String(c.ID), "name": c.Name, "kind": c.Kind, "config": c.Config, "secret_hint": nil,
		"account": nil, "expires_at": nil, "created_at": c.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at": c.UpdatedAt.UTC().Format(time.RFC3339), "created_by": nil,
	}
	if c.Hint != "" {
		view["secret_hint"] = c.Hint
	}
	if c.Account != "" {
		view["account"] = c.Account
	}
	if c.ExpiresAt != nil {
		view["expires_at"] = c.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if c.CreatedBy != "" {
		view["created_by"] = c.CreatedBy
	}
	return view
}

// connectionRequest is the body of the connection endpoints. secret is
// write-only: nothing ever returns it.
type connectionRequest struct {
	Name     string            `json:"name"`
	Kind     string            `json:"kind"`
	Config   map[string]any    `json:"config"`
	Secret   map[string]string `json:"secret"`
	SkipTest bool              `json:"skip_test"`
}

func (c connectionRequest) input() connections.Input {
	return connections.Input{Name: c.Name, Kind: c.Kind, Config: c.Config, Secret: c.Secret, SkipTest: c.SkipTest}
}

// listIntegrations is GET /v1/integrations: the kinds of connection this
// installation offers and the fields each takes, for the editor.
func (h *Handler) listIntegrations(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	kinds := h.services.Connections.Kinds()
	items := make([]map[string]any, 0, len(kinds))
	for _, kind := range kinds {
		fields := make([]map[string]any, 0, len(kind.Fields))
		for _, f := range kind.Fields {
			fields = append(fields, map[string]any{
				"name": f.Name, "title": f.Title, "type": f.Type, "required": f.Required, "secret": f.Secret,
				"enum": f.Enum, "default": f.Default, "help": f.Help,
			})
		}
		items = append(items, map[string]any{"kind": kind.Kind, "title": kind.Title, "oauth": kind.OAuth, "fields": fields})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// testConnectionInput is POST /v1/connections/test: proves settings and
// credentials work before they are saved. It saves nothing.
func (h *Handler) testConnectionInput(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	var req connectionRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	if err := h.services.Connections.TestInput(r.Context(), req.input()); err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// createConnection is POST /v1/connections: tests (unless skip_test) and saves
// only if the test passes.
func (h *Handler) createConnection(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var req connectionRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	created, err := h.services.Connections.Create(r.Context(), p.TenantID, p.Actor(), req.input())
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, connectionInfoView(created))
}

func (h *Handler) listConnections(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	list, err := h.services.Connections.List(r.Context(), p.TenantID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(list))
	for _, c := range list {
		items = append(items, connectionInfoView(c))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) getConnection(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	c, err := h.services.Connections.Get(r.Context(), p.TenantID, r.PathValue("name"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, connectionInfoView(c))
}

// updateConnection is PUT /v1/connections/{name}: replaces the settings, and the
// credentials when a secret is sent. The kind cannot change.
func (h *Handler) updateConnection(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var req connectionRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	updated, err := h.services.Connections.Update(r.Context(), p.TenantID, r.PathValue("name"), req.input())
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, connectionInfoView(updated))
}

func (h *Handler) deleteConnection(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if err := h.services.Connections.Delete(r.Context(), p.TenantID, r.PathValue("name")); err != nil {
		h.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) testConnection(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if err := h.services.Connections.Test(r.Context(), p.TenantID, r.PathValue("name")); err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}
