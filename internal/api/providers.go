package api

import (
	"net/http"
	"time"

	"github.com/kartikey/workflow_service/internal/services/providers"
	"github.com/kartikey/workflow_service/pkg/auth"
	"github.com/kartikey/workflow_service/pkg/httpx"
	"github.com/kartikey/workflow_service/pkg/uid"
)

func connectionView(c providers.Connection) map[string]any {
	view := map[string]any{
		"id": uid.String(c.ID), "name": c.Name, "provider": c.Provider, "base_url": nil, "key_hint": nil,
		"agents": c.Agents, "created_at": c.CreatedAt.UTC().Format(time.RFC3339), "created_by": nil,
	}
	if c.BaseURL != "" {
		view["base_url"] = c.BaseURL
	}
	if c.KeyHint != "" {
		view["key_hint"] = c.KeyHint
	}
	if c.CreatedBy != "" {
		view["created_by"] = c.CreatedBy
	}
	return view
}

// testProvider is POST /v1/providers/test: proves a key and endpoint work by
// listing the models, before anything is saved. The key is never stored or logged.
func (h *Handler) testProvider(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	var req struct {
		Provider string `json:"provider"`
		BaseURL  string `json:"base_url"`
		APIKey   string `json:"api_key"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	result, err := h.services.Providers.Test(r.Context(), providers.TestInput{Provider: req.Provider, BaseURL: req.BaseURL, APIKey: req.APIKey})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "models": result.Models})
}

// createProvider is POST /v1/providers: tests the connection (unless skip_test
// is set) and saves it only if the test passes.
func (h *Handler) createProvider(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var req struct {
		Name     string `json:"name"`
		Provider string `json:"provider"`
		BaseURL  string `json:"base_url"`
		APIKey   string `json:"api_key"`
		SkipTest bool   `json:"skip_test"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	connection, tested, err := h.services.Providers.Create(r.Context(), p.TenantID, p.Actor(), providers.CreateInput{
		Name: req.Name, Provider: req.Provider, BaseURL: req.BaseURL, APIKey: req.APIKey, SkipTest: req.SkipTest,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	view := connectionView(connection)
	view["models"] = tested.Models
	httpx.WriteJSON(w, http.StatusCreated, view)
}

func (h *Handler) listProviders(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	connections, err := h.services.Providers.List(r.Context(), p.TenantID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(connections))
	for _, c := range connections {
		items = append(items, connectionView(c))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) testStoredProvider(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	id, err := pathUUID(r, "id", "provider connection")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	result, err := h.services.Providers.TestStored(r.Context(), p.TenantID, id)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "models": result.Models})
}

func (h *Handler) deleteProvider(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	id, err := pathUUID(r, "id", "provider connection")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if err := h.services.Providers.Delete(r.Context(), p.TenantID, id); err != nil {
		h.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
