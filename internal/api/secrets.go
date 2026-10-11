package api

import (
	"net/http"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/kartikey/workflow_service/pkg/auth"
	"github.com/kartikey/workflow_service/pkg/httpx"
)

// listSecrets is GET /v1/secrets: names and metadata only. Values are
// write-only, so there is no way to read one back.
func (h *Handler) listSecrets(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	secrets, err := h.services.Vault.List(r.Context(), p.TenantID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(secrets))
	for _, s := range secrets {
		item := map[string]any{
			"name": s.Name, "created_at": s.CreatedAt.UTC().Format(time.RFC3339), "updated_at": s.UpdatedAt.UTC().Format(time.RFC3339),
			"updated_by": nil, "reference": "${secrets." + s.Name + "}",
		}
		if s.UpdatedBy != "" {
			item["updated_by"] = s.UpdatedBy
		}
		items = append(items, item)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// putSecret is PUT /v1/secrets/{name} with {"value": "..."}: creates or replaces.
func (h *Handler) putSecret(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var req struct {
		Value string `json:"value"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	name := r.PathValue("name")
	if err := h.services.Vault.Put(r.Context(), p.TenantID, name, req.Value, p.Actor()); err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"name": name, "reference": "${secrets." + name + "}"})
}

func (h *Handler) deleteSecret(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if err := h.services.Vault.Delete(r.Context(), p.TenantID, r.PathValue("name")); err != nil {
		h.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// rotateVault is POST /v1/admin/vault/rotate: re-seal every secret under the
// current master key. Safe to run while serving; run it again until failed is 0.
func (h *Handler) rotateVault(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	result, err := h.services.Vault.Rotate(r.Context())
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	h.logger.Info("vault rotation", zap.Int("rotated", result.Rotated), zap.Int("skipped", result.Skipped),
		zap.Int("failed", result.Failed), zap.String("actor", p.Actor()))
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"current_key_version": h.services.Vault.Current(),
		"rotated":             result.Rotated, "skipped": result.Skipped, "failed": result.Failed,
	})
}

// vaultStatus is GET /v1/admin/vault: how many secrets use each key version,
// which says when an old key can be removed.
func (h *Handler) vaultStatus(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	versions, err := h.services.Vault.KeyVersions(r.Context())
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	byVersion := make(map[string]int64, len(versions))
	for version, count := range versions {
		byVersion[strconv.Itoa(version)] = count
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"current_key_version": h.services.Vault.Current(), "secrets_by_key_version": byVersion})
}
