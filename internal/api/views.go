package api

import (
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/httpx"
	"github.com/kartikey/workflow_service/pkg/uid"
)

func runSummary(run gen.Run, workflow string, version int32) map[string]any {
	return map[string]any{
		"id": uid.String(run.ID), "status": run.Status, "workflow": workflow, "version": version,
		"correlation_id": httpx.Text(run.CorrelationID), "trigger": run.Trigger,
		"parent_run_id": nullableID(run.ParentRunID), "created_by": httpx.Text(run.CreatedBy),
		"created_at": httpx.Time(run.CreatedAt), "started_at": httpx.Time(run.StartedAt),
		"ended_at": httpx.Time(run.EndedAt), "deadline_at": httpx.Time(run.DeadlineAt),
	}
}

func nullableID(id pgtype.UUID) *string {
	if !id.Valid {
		return nil
	}
	text := uid.String(id)
	return &text
}

func eventView(e gen.RunEvent) map[string]any {
	return map[string]any{
		"seq": e.Seq, "type": e.Type, "node_id": httpx.Text(e.NodeID), "attempt": attemptJSON(e),
		"payload": httpx.Raw(e.Payload), "created_at": httpx.Time(e.CreatedAt),
	}
}

func attemptJSON(e gen.RunEvent) *int32 {
	if !e.Attempt.Valid {
		return nil
	}
	return &e.Attempt.Int32
}

func workflowView(w gen.Workflow) map[string]any {
	return map[string]any{
		"id": uid.String(w.ID), "name": w.Name, "description": httpx.Text(w.Description),
		"created_at": httpx.Time(w.CreatedAt), "updated_at": httpx.Time(w.UpdatedAt),
		"archived_at": httpx.Time(w.ArchivedAt),
	}
}

func versionView(v gen.WorkflowVersion) map[string]any {
	return map[string]any{
		"version": v.Version, "version_id": uid.String(v.ID), "spec_hash": v.SpecHash,
		"spec": httpx.Raw(v.Spec), "yaml_source": v.YamlSource,
		"created_at": httpx.Time(v.CreatedAt), "created_by": httpx.Text(v.CreatedBy),
	}
}
