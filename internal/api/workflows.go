package api

import (
	"encoding/json"
	"github.com/kartikey/workflow_service/pkg/httpx"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/kartikey/workflow_service/internal/services/workflows"
	"github.com/kartikey/workflow_service/internal/spec"
	"github.com/kartikey/workflow_service/pkg/auth"
	"github.com/kartikey/workflow_service/pkg/exception"
	"github.com/kartikey/workflow_service/pkg/uid"
)

// publishWorkflow is POST /v1/workflows (04 §2.1). The body is the YAML
// document; JSON is accepted too, either as the document itself (JSON is YAML)
// or as {"yaml": "..."} for clients that cannot send a raw body.
func (h *Handler) publishWorkflow(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	source, err := io.ReadAll(r.Body)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType == "application/json" {
		var wrapped struct {
			YAML     *string `json:"yaml"`
			Workflow *string `json:"workflow"`
		}
		if json.Unmarshal(source, &wrapped) == nil {
			switch {
			case wrapped.YAML != nil:
				source = []byte(*wrapped.YAML)
			case wrapped.Workflow != nil:
				source = []byte(*wrapped.Workflow)
			}
		}
	}
	if len(strings.TrimSpace(string(source))) == 0 {
		h.writeError(w, r, exception.BadRequest("request body must be a workflow YAML document"))
		return
	}
	params := workflows.PublishParams{
		TenantID: p.TenantID, Source: source, CreatedBy: p.Actor(),
		DryRun: r.URL.Query().Get("dry_run") == "true",
	}
	result, err := h.services.Workflows.Publish(r.Context(), params)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	warnings := result.Diagnostics
	if warnings == nil {
		warnings = []spec.Diagnostic{}
	}
	if params.DryRun {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"dry_run": true, "valid": true, "workflow": map[string]any{"name": result.Spec.Name},
			"spec": result.Spec, "warnings": warnings,
		})
		return
	}
	status := http.StatusCreated
	if result.Unchanged {
		status = http.StatusOK
	}
	httpx.WriteJSON(w, status, map[string]any{
		"workflow":   map[string]any{"id": uid.String(result.Workflow.ID), "name": result.Workflow.Name},
		"version":    result.Version.Version,
		"version_id": uid.String(result.Version.ID),
		"spec_hash":  result.Version.SpecHash,
		"unchanged":  result.Unchanged,
		"warnings":   warnings,
	})
}

func (h *Handler) listWorkflows(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	rows, err := h.services.Workflows.List(r.Context(), p.TenantID, r.URL.Query().Get("include_archived") == "true")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		view := workflowView(row.Workflow)
		view["latest_version"] = row.LatestVersion
		view["latest_published_at"] = httpx.Time(row.LatestPublishedAt)
		view["run_count"] = row.RunCount
		view["last_run_at"] = httpx.Time(row.LastRunAt)
		items = append(items, view)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// getWorkflow returns the workflow, its latest compiled spec (enough to render
// a graph) and the version list (04 §2.2).
func (h *Handler) getWorkflow(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	detail, err := h.services.Workflows.Get(r.Context(), p.TenantID, r.PathValue("name"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	view := workflowView(detail.Workflow)
	if detail.Latest != nil {
		view["latest"] = versionView(*detail.Latest)
	} else {
		view["latest"] = nil
	}
	summaries := make([]map[string]any, 0, len(detail.Versions))
	for _, v := range detail.Versions {
		summaries = append(summaries, map[string]any{
			"version": v.Version, "version_id": uid.String(v.ID), "spec_hash": v.SpecHash,
			"created_at": httpx.Time(v.CreatedAt), "created_by": httpx.Text(v.CreatedBy),
		})
	}
	view["versions"] = summaries
	httpx.WriteJSON(w, http.StatusOK, view)
}

// getWorkflowVersion is 04 §2.3; ?format=yaml round-trips the deployed source.
func (h *Handler) getWorkflowVersion(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	number, err := strconv.ParseInt(r.PathValue("version"), 10, 32)
	if err != nil || number < 1 {
		h.writeError(w, r, exception.BadRequest("version must be a positive integer"))
		return
	}
	detail, err := h.services.Workflows.GetVersion(r.Context(), p.TenantID, r.PathValue("name"), int32(number))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if r.URL.Query().Get("format") == "yaml" {
		w.Header().Set("Content-Type", "application/x-yaml")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, detail.Version.YamlSource)
		return
	}
	view := versionView(detail.Version)
	view["workflow"] = map[string]any{"id": uid.String(detail.Workflow.ID), "name": detail.Workflow.Name}
	httpx.WriteJSON(w, http.StatusOK, view)
}

// archiveWorkflow is DELETE /v1/workflows/{name} (04 §2.4): new runs are
// rejected with 409, existing runs continue, versions are never deleted.
func (h *Handler) archiveWorkflow(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	h.setArchived(w, r, p, true)
}

func (h *Handler) restoreWorkflow(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	h.setArchived(w, r, p, false)
}

func (h *Handler) setArchived(w http.ResponseWriter, r *http.Request, p auth.Principal, archived bool) {
	name := r.PathValue("name")
	if err := h.services.Workflows.SetArchived(r.Context(), p.TenantID, name, archived); err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"name": name, "archived": archived})
}
