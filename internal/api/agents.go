package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/kartikey/workflow_service/internal/services/agents"
	"github.com/kartikey/workflow_service/pkg/auth"
	"github.com/kartikey/workflow_service/pkg/httpx"
	"github.com/kartikey/workflow_service/pkg/uid"
)

// agentRequest is the body of POST /v1/agents and PUT /v1/agents/{name}.
type agentRequest struct {
	Name               string          `json:"name"`
	Description        string          `json:"description"`
	Connection         string          `json:"connection"`
	Model              string          `json:"model"`
	System             string          `json:"system_prompt"`
	InputSchema        json.RawMessage `json:"input_schema"`
	OutputSchema       json.RawMessage `json:"output_schema"`
	Tools              []string        `json:"tools"`
	DatabaseConnection string          `json:"database_connection"`
	AllowedHosts       []string        `json:"allowed_hosts"`
	MaxIterations      int             `json:"max_iterations"`
	MaxTokens          *int            `json:"max_tokens"`
	Temperature        *float64        `json:"temperature"`
}

func (a agentRequest) input() agents.Input {
	return agents.Input{
		Name: a.Name, Description: a.Description, Connection: a.Connection, Model: a.Model, System: a.System,
		InputSchema: a.InputSchema, OutputSchema: a.OutputSchema, Tools: a.Tools, AllowedHosts: a.AllowedHosts, DatabaseConnection: a.DatabaseConnection,
		MaxIterations: a.MaxIterations, MaxTokens: a.MaxTokens, Temperature: a.Temperature,
	}
}

func agentView(a agents.Agent) map[string]any {
	view := map[string]any{
		"id": uid.String(a.ID), "name": a.Name, "description": a.Description, "connection": nil, "model": a.Model,
		"system_prompt": a.System, "input_schema": httpx.Raw(a.InputSchema), "output_schema": httpx.Raw(a.OutputSchema),
		"database_connection": a.DatabaseConnection, "tools": a.Tools, "allowed_hosts": a.AllowedHosts, "max_iterations": a.MaxIterations,
		"max_tokens": a.MaxTokens, "temperature": a.Temperature, "revision": a.Revision,
		"created_at": a.CreatedAt.UTC().Format(time.RFC3339), "updated_at": a.UpdatedAt.UTC().Format(time.RFC3339),
		"updated_by": nil,
	}
	if a.Connection != "" {
		view["connection"] = a.Connection
	}
	if a.UpdatedBy != "" {
		view["updated_by"] = a.UpdatedBy
	}
	return view
}

func (h *Handler) listAgentTools(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	tools := h.services.Agents.Tools()
	items := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		items = append(items, map[string]any{"name": tool.Name, "description": tool.Description, "input_schema": httpx.Raw(tool.InputSchema)})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) listAgents(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	list, err := h.services.Agents.List(r.Context(), p.TenantID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(list))
	for _, a := range list {
		items = append(items, agentView(a))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) createAgent(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var req agentRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	created, err := h.services.Agents.Create(r.Context(), p.TenantID, p.Actor(), req.input())
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, agentView(created))
}

func (h *Handler) getAgent(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	found, err := h.services.Agents.Get(r.Context(), p.TenantID, r.PathValue("name"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, agentView(found))
}

// updateAgent is PUT /v1/agents/{name}: replaces the definition and bumps its
// revision. The name in the path is the identity; a name in the body is ignored.
func (h *Handler) updateAgent(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var req agentRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	updated, err := h.services.Agents.Update(r.Context(), p.TenantID, p.Actor(), r.PathValue("name"), req.input())
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, agentView(updated))
}

func (h *Handler) deleteAgent(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if err := h.services.Agents.Delete(r.Context(), p.TenantID, r.PathValue("name")); err != nil {
		h.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// testAgent is POST /v1/agents/{name}/test: runs the agent once with the given
// input, as a workflow node would. It spends real tokens and reports the cost,
// but creates no run. A failing agent is a 200 with ok:false so the editor can
// show why.
func (h *Handler) testAgent(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var req struct {
		Input  json.RawMessage `json:"input"`
		Prompt json.RawMessage `json:"prompt"`
	}
	if r.ContentLength != 0 {
		if err := httpx.DecodeJSON(r, &req); err != nil {
			h.writeError(w, r, err)
			return
		}
	}
	result, err := h.services.Agents.Test(r.Context(), p.TenantID, r.PathValue("name"), req.Input, req.Prompt)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	view := map[string]any{"ok": result.OK, "output": httpx.Raw(result.Output), "metrics": result.Metrics}
	if !result.OK {
		view["error"] = map[string]any{"code": result.Code, "message": result.Message}
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}
