package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/kartikey/workflow_service/internal/api/problem"
	"github.com/kartikey/workflow_service/internal/auth"
	"github.com/kartikey/workflow_service/internal/ids"
	"github.com/kartikey/workflow_service/internal/spec"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/internal/workflows"
)

// publishWorkflow is POST /v1/workflows (04 §2.1). The body is the YAML
// document; JSON is accepted too, either as the document itself (JSON is YAML)
// or as {"yaml": "..."} for clients that cannot send a raw body.
func (s *Server) publishWorkflow(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	source, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeError(w, r, err)
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
		s.writeError(w, r, problem.BadRequest("request body must be a workflow YAML document"))
		return
	}
	params := workflows.PublishParams{
		TenantID: p.TenantID, Source: source, CreatedBy: p.Actor(),
		DryRun: r.URL.Query().Get("dry_run") == "true",
	}
	if s.services.Plans != nil {
		if params.Limits, err = s.services.Plans.PublishLimits(r.Context(), p.TenantID); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	result, err := s.services.Workflows.Publish(r.Context(), params)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	warnings := result.Diagnostics
	if warnings == nil {
		warnings = []spec.Diagnostic{}
	}
	if params.DryRun {
		writeJSON(w, http.StatusOK, map[string]any{
			"dry_run": true, "valid": true, "workflow": map[string]any{"name": result.Spec.Name},
			"spec": result.Spec, "warnings": warnings,
		})
		return
	}
	status := http.StatusCreated
	if result.Unchanged {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{
		"workflow":   map[string]any{"id": ids.String(result.Workflow.ID), "name": result.Workflow.Name},
		"version":    result.Version.Version,
		"version_id": ids.String(result.Version.ID),
		"spec_hash":  result.Version.SpecHash,
		"unchanged":  result.Unchanged,
		"warnings":   warnings,
	})
}

func workflowView(w gen.Workflow) map[string]any {
	return map[string]any{
		"id": ids.String(w.ID), "name": w.Name, "description": textJSON(w.Description),
		"created_at": timeJSON(w.CreatedAt), "updated_at": timeJSON(w.UpdatedAt),
		"archived_at": timeJSON(w.ArchivedAt),
	}
}

func (s *Server) listWorkflows(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	rows, err := s.services.Flow.Q.ListWorkflows(r.Context(), gen.ListWorkflowsParams{
		TenantID: p.TenantID, IncludeArchived: r.URL.Query().Get("include_archived") == "true",
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		view := workflowView(row.Workflow)
		view["latest_version"] = row.LatestVersion
		view["latest_published_at"] = timeJSON(row.LatestPublishedAt)
		view["run_count"] = row.RunCount
		view["last_run_at"] = timeJSON(row.LastRunAt)
		items = append(items, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) loadWorkflow(r *http.Request, p auth.Principal) (gen.Workflow, error) {
	workflow, err := s.services.Flow.Q.GetWorkflowByName(r.Context(), gen.GetWorkflowByNameParams{TenantID: p.TenantID, Name: r.PathValue("name")})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.Workflow{}, problem.NotFound("workflow " + strconv.Quote(r.PathValue("name")))
	}
	return workflow, err
}

func versionView(v gen.WorkflowVersion) map[string]any {
	return map[string]any{
		"version": v.Version, "version_id": ids.String(v.ID), "spec_hash": v.SpecHash,
		"spec": rawJSON(v.Spec), "yaml_source": v.YamlSource,
		"created_at": timeJSON(v.CreatedAt), "created_by": textJSON(v.CreatedBy),
	}
}

// getWorkflow returns the workflow, its latest compiled spec (enough to render
// a graph) and the version list (04 §2.2).
func (s *Server) getWorkflow(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	workflow, err := s.loadWorkflow(r, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	view := workflowView(workflow)
	latest, err := s.services.Flow.Q.LatestWorkflowVersion(r.Context(), workflow.ID)
	switch {
	case err == nil:
		view["latest"] = versionView(latest)
	case errors.Is(err, pgx.ErrNoRows):
		view["latest"] = nil
	default:
		s.writeError(w, r, err)
		return
	}
	versions, err := s.services.Flow.Q.ListWorkflowVersions(r.Context(), workflow.ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	summaries := make([]map[string]any, 0, len(versions))
	for _, v := range versions {
		summaries = append(summaries, map[string]any{
			"version": v.Version, "version_id": ids.String(v.ID), "spec_hash": v.SpecHash,
			"created_at": timeJSON(v.CreatedAt), "created_by": textJSON(v.CreatedBy),
		})
	}
	view["versions"] = summaries
	writeJSON(w, http.StatusOK, view)
}

// getWorkflowVersion is 04 §2.3; ?format=yaml round-trips the deployed source.
func (s *Server) getWorkflowVersion(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	workflow, err := s.loadWorkflow(r, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	number, err := strconv.ParseInt(r.PathValue("version"), 10, 32)
	if err != nil || number < 1 {
		s.writeError(w, r, problem.BadRequest("version must be a positive integer"))
		return
	}
	version, err := s.services.Flow.Q.GetWorkflowVersionByNumber(r.Context(), gen.GetWorkflowVersionByNumberParams{WorkflowID: workflow.ID, Version: int32(number)})
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, r, problem.NotFound("version "+strconv.FormatInt(number, 10)))
		return
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if r.URL.Query().Get("format") == "yaml" {
		w.Header().Set("Content-Type", "application/x-yaml")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, version.YamlSource)
		return
	}
	view := versionView(version)
	view["workflow"] = map[string]any{"id": ids.String(workflow.ID), "name": workflow.Name}
	writeJSON(w, http.StatusOK, view)
}

// archiveWorkflow is DELETE /v1/workflows/{name} (04 §2.4): new runs are
// rejected with 409, existing runs continue, versions are never deleted.
func (s *Server) archiveWorkflow(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	s.setArchived(w, r, p, true)
}

func (s *Server) restoreWorkflow(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	s.setArchived(w, r, p, false)
}

func (s *Server) setArchived(w http.ResponseWriter, r *http.Request, p auth.Principal, archived bool) {
	rows, err := s.services.Flow.Q.SetWorkflowArchived(r.Context(), gen.SetWorkflowArchivedParams{
		TenantID: p.TenantID, Name: r.PathValue("name"), Archived: archived,
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if rows == 0 {
		s.writeError(w, r, problem.NotFound("workflow "+strconv.Quote(r.PathValue("name"))))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": r.PathValue("name"), "archived": archived})
}
