package api

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kartikey/workflow_service/internal/services/connections"
	"github.com/kartikey/workflow_service/pkg/auth"
	"github.com/kartikey/workflow_service/pkg/exception"
	"github.com/kartikey/workflow_service/pkg/httpx"
	"github.com/kartikey/workflow_service/pkg/uid"
	"go.uber.org/zap"
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

// startOAuth is POST /v1/connections/oauth/start: {kind, name} → the provider's
// authorization URL. The caller sends the user there; the provider redirects to
// /v1/oauth/callback. Naming an existing connection of the same kind reconnects it.
func (h *Handler) startOAuth(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var req struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	target, err := h.services.Connections.StartOAuth(r.Context(), p.TenantID, p.UserID, req.Kind, req.Name)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"authorize_url": target})
}

// oauthCallback is GET /v1/oauth/callback, where the provider redirects the
// user's browser. It needs no session: the one-time state identifies the
// organization that started the sign-in. The browser is sent on to the UI's
// connections page with the outcome in the query string.
func (h *Handler) oauthCallback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	providerError := query.Get("error")
	if providerError != "" && query.Get("error_description") != "" {
		providerError += ": " + query.Get("error_description")
	}
	name, err := h.services.Connections.CompleteOAuth(r.Context(), query.Get("state"), query.Get("code"), providerError)
	back := strings.TrimRight(h.services.PublicURL, "/") + "/connections"
	if err != nil {
		var ex *exception.Exception
		message := "sign-in failed"
		if errors.As(err, &ex) {
			message = ex.Detail
		} else {
			h.logger.Error("oauth callback", zap.Error(err))
		}
		http.Redirect(w, r, back+"?"+url.Values{"error": {message}, "name": {name}}.Encode(), http.StatusFound)
		return
	}
	http.Redirect(w, r, back+"?"+url.Values{"connected": {name}}.Encode(), http.StatusFound)
}
