package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/services/identity"
	"github.com/kartikey/workflow_service/pkg/auth"
	"github.com/kartikey/workflow_service/pkg/exception"
	"github.com/kartikey/workflow_service/pkg/httpx"
	"github.com/kartikey/workflow_service/pkg/uid"
)

func (h *Handler) registerIdentity(r Router) {
	r.HandlePublic("POST /v1/auth/login", h.login)
	r.HandlePublic("GET /v1/auth/invite", h.previewInvite)
	r.HandlePublic("POST /v1/auth/accept-invite", h.acceptInvite)
	r.Handle("POST /v1/auth/logout", auth.RoleViewer, h.logout)
	r.Handle("GET /v1/auth/me", auth.RoleViewer, h.me)
	r.Handle("POST /v1/auth/switch-org", auth.RoleViewer, h.switchOrg)

	r.Handle("GET /v1/members", auth.RoleViewer, h.listMembers)
	r.Handle("PATCH /v1/members/{user_id}", auth.RoleAdmin, h.updateMember)
	r.Handle("DELETE /v1/members/{user_id}", auth.RoleViewer, h.removeMember) // anyone may leave
	r.Handle("POST /v1/invites", auth.RoleAdmin, h.createInvite)
	r.Handle("GET /v1/invites", auth.RoleAdmin, h.listInvites)
	r.Handle("DELETE /v1/invites/{id}", auth.RoleAdmin, h.revokeInvite)
	r.Handle("POST /v1/api-keys", auth.RoleAdmin, h.createAPIKey)
	r.Handle("GET /v1/api-keys", auth.RoleAdmin, h.listAPIKeys)
	r.Handle("DELETE /v1/api-keys/{id}", auth.RoleAdmin, h.revokeAPIKey)
	r.Handle("GET /v1/usage", auth.RoleViewer, h.usage)
	r.Handle("GET /v1/plans", auth.RoleViewer, h.listPlans)

	r.HandleAdmin("POST /v1/admin/invites", h.platformInvite)
	r.HandleAdmin("GET /v1/admin/organizations", h.listOrganizations)
	r.HandleAdmin("PUT /v1/admin/organizations/{id}/plan", h.setOrganizationPlan)
}

func userView(u identity.User) map[string]any {
	return map[string]any{"id": uid.String(u.ID), "email": u.Email, "name": u.Name, "platform_admin": u.PlatformAdmin}
}

func membershipView(m identity.Membership) map[string]any {
	return map[string]any{"id": uid.String(m.OrgID), "name": m.OrgName, "slug": m.OrgSlug, "role": m.Role}
}

func membershipsView(ms []identity.Membership) []map[string]any {
	out := make([]map[string]any, 0, len(ms))
	for _, m := range ms {
		out = append(out, membershipView(m))
	}
	return out
}

func (h *Handler) sessionCookie(token string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name: identity.SessionCookie, Value: token, Path: "/", Expires: expires, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: h.services.CookieSecure,
	}
}

func (h *Handler) writeSession(w http.ResponseWriter, status int, session identity.Session) {
	http.SetCookie(w, h.sessionCookie(session.Token, session.ExpiresAt))
	httpx.WriteJSON(w, status, map[string]any{
		"user": userView(session.User), "organization": membershipView(session.Current),
		"organizations": membershipsView(session.Memberships),
	})
}

// login is POST /v1/auth/login: sets the session cookie.
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	session, err := h.services.Identity.Login(r.Context(), req.Email, req.Password)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	h.writeSession(w, http.StatusOK, session)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if p.SessionID.Valid {
		if err := h.services.Identity.Logout(r.Context(), p.SessionID); err != nil {
			h.writeError(w, r, err)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: identity.SessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: h.services.CookieSecure,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	user, current, all, err := h.services.Identity.Me(r.Context(), p)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"user": userView(user), "organization": membershipView(current), "organizations": membershipsView(all),
	})
}

func (h *Handler) switchOrg(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var req struct {
		OrganizationID string `json:"organization_id"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	id, err := uid.Parse(req.OrganizationID)
	if err != nil {
		h.writeError(w, r, exception.BadRequest("organization_id must be an organization ID"))
		return
	}
	current, err := h.services.Identity.SwitchOrg(r.Context(), p, id)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"organization": membershipView(current)})
}

func (h *Handler) inviteView(i identity.Invite, token string) map[string]any {
	view := map[string]any{
		"id": uid.String(i.ID), "email": i.Email, "role": i.Role,
		"expires_at": i.ExpiresAt.UTC().Format(time.RFC3339), "created_at": i.CreatedAt.UTC().Format(time.RFC3339),
	}
	if i.OrgName != "" {
		view["organization_name"] = i.OrgName
	}
	if token != "" {
		// Shown once: the token is not stored, only its hash.
		view["token"] = token
		if h.services.PublicURL != "" {
			view["accept_url"] = strings.TrimRight(h.services.PublicURL, "/") + "/accept-invite?token=" + token
		}
	}
	return view
}

// previewInvite is GET /v1/auth/invite?token=: what the accept page shows.
func (h *Handler) previewInvite(w http.ResponseWriter, r *http.Request) {
	invite, hasAccount, err := h.services.Identity.PreviewInvite(r.Context(), r.URL.Query().Get("token"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	view := h.inviteView(invite, "")
	view["has_account"] = hasAccount
	httpx.WriteJSON(w, http.StatusOK, view)
}

// acceptInvite is POST /v1/auth/accept-invite: creates the account (or checks
// the existing account's password), joins the organization and signs in.
func (h *Handler) acceptInvite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token    string `json:"token"`
		Password string `json:"password"`
		Name     string `json:"name"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	session, err := h.services.Identity.AcceptInvite(r.Context(), req.Token, req.Password, req.Name)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	h.writeSession(w, http.StatusCreated, session)
}

func (h *Handler) listMembers(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	members, err := h.services.Identity.ListMembers(r.Context(), p.TenantID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(members))
	for _, m := range members {
		items = append(items, map[string]any{
			"user_id": uid.String(m.UserID), "email": m.Email, "name": m.Name, "role": m.Role,
			"joined_at": m.JoinedAt.UTC().Format(time.RFC3339),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func pathUUID(r *http.Request, name, what string) (pgtype.UUID, error) {
	id, err := uid.Parse(r.PathValue(name))
	if err != nil {
		return pgtype.UUID{}, exception.NotFound(what)
	}
	return id, nil
}

func (h *Handler) updateMember(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	id, err := pathUUID(r, "user_id", "member")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	var req struct {
		Role string `json:"role"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	if err := h.services.Identity.UpdateMemberRole(r.Context(), p, id, req.Role); err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"user_id": uid.String(id), "role": req.Role})
}

func (h *Handler) removeMember(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	id, err := pathUUID(r, "user_id", "member")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if err := h.services.Identity.RemoveMember(r.Context(), p, id); err != nil {
		h.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) createInvite(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var req struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	invite, token, err := h.services.Identity.CreateInvite(r.Context(), p, req.Email, req.Role)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, h.inviteView(invite, token))
}

func (h *Handler) listInvites(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	invites, err := h.services.Identity.ListInvites(r.Context(), p.TenantID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(invites))
	for _, i := range invites {
		items = append(items, h.inviteView(i, ""))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) revokeInvite(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	id, err := pathUUID(r, "id", "invite")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if err := h.services.Identity.RevokeInvite(r.Context(), p.TenantID, id); err != nil {
		h.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func apiKeyView(k identity.APIKey) map[string]any {
	view := map[string]any{
		"id": uid.String(k.ID), "name": k.Name, "prefix": k.Prefix, "role": k.Role,
		"created_at": k.CreatedAt.UTC().Format(time.RFC3339), "last_used_at": nil,
	}
	if k.LastUsedAt != nil {
		view["last_used_at"] = k.LastUsedAt.UTC().Format(time.RFC3339)
	}
	return view
}

func (h *Handler) createAPIKey(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var req struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	key, token, err := h.services.Identity.CreateAPIKey(r.Context(), p, strings.TrimSpace(req.Name), req.Role)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	view := apiKeyView(key)
	view["token"] = token // shown once
	httpx.WriteJSON(w, http.StatusCreated, view)
}

func (h *Handler) listAPIKeys(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	keys, err := h.services.Identity.ListAPIKeys(r.Context(), p.TenantID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		items = append(items, apiKeyView(k))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) revokeAPIKey(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	id, err := pathUUID(r, "id", "api key")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if err := h.services.Identity.RevokeAPIKey(r.Context(), p.TenantID, id); err != nil {
		h.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func planView(p identity.PlanInfo) map[string]any {
	return map[string]any{
		"id": p.ID, "name": p.Name,
		"limits": map[string]any{"workflows": p.MaxWorkflows, "steps_per_workflow": p.MaxSteps, "runs_per_month": p.MaxRunsPerMonth},
	}
}

func (h *Handler) usage(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	usage, err := h.services.Identity.Usage(r.Context(), p.TenantID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"plan": planView(usage.Plan), "usage": map[string]any{"workflows": usage.Workflows, "runs_this_month": usage.RunsThisMonth},
	})
}

func (h *Handler) listPlans(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	plans, err := h.services.Identity.ListPlans(r.Context())
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(plans))
	for _, plan := range plans {
		items = append(items, planView(plan))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) platformInvite(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var req struct {
		Email            string `json:"email"`
		OrganizationName string `json:"organization_name"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	invite, token, err := h.services.Identity.CreatePlatformInvite(r.Context(), p, req.Email, req.OrganizationName)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, h.inviteView(invite, token))
}

func (h *Handler) listOrganizations(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	limit, err := queryLimit(r, 100, 500)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	orgs, err := h.services.Identity.ListOrganizations(r.Context(), limit)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(orgs))
	for _, o := range orgs {
		items = append(items, map[string]any{
			"id": uid.String(o.ID), "name": o.Name, "slug": o.Slug, "plan": o.PlanID, "plan_name": o.PlanName,
			"members": o.Members, "runs_this_month": o.RunsThisMonth,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) setOrganizationPlan(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	id, err := pathUUID(r, "id", "organization")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	var req struct {
		Plan string `json:"plan"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	if err := h.services.Identity.SetOrganizationPlan(r.Context(), id, req.Plan); err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": uid.String(id), "plan": req.Plan})
}
