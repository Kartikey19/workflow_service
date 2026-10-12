package api

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/services/alerts"
	"github.com/kartikey/workflow_service/internal/services/runs"
	"github.com/kartikey/workflow_service/internal/services/schedules"
	"github.com/kartikey/workflow_service/pkg/auth"
	"github.com/kartikey/workflow_service/pkg/exception"
	"github.com/kartikey/workflow_service/pkg/httpx"
	"github.com/kartikey/workflow_service/pkg/uid"
)

// ScheduleService manages recurring workflow starts.
type ScheduleService interface {
	Create(ctx context.Context, org pgtype.UUID, actor string, in schedules.Input) (schedules.Info, error)
	List(ctx context.Context, org pgtype.UUID, workflow string) ([]schedules.Info, error)
	Update(ctx context.Context, org, id pgtype.UUID, in schedules.Input) (schedules.Info, error)
	Delete(ctx context.Context, org, id pgtype.UUID) error
	RunNow(ctx context.Context, org, id pgtype.UUID, actor string) (runs.StartResult, error)
	Preview(expression, zone string, n int) ([]time.Time, error)
}

// AlertService manages alert channels and their delivery log.
type AlertService interface {
	Create(ctx context.Context, org pgtype.UUID, in alerts.Input) (alerts.Created, error)
	List(ctx context.Context, org pgtype.UUID) ([]alerts.Info, error)
	Update(ctx context.Context, org, id pgtype.UUID, in alerts.Input) (alerts.Info, error)
	Delete(ctx context.Context, org, id pgtype.UUID) error
	Test(ctx context.Context, org, id pgtype.UUID) error
	Deliveries(ctx context.Context, org pgtype.UUID, limit int32) ([]alerts.Delivery, error)
}

func (h *Handler) registerOperate(r Router) {
	r.Handle("POST /be/v1/test-runs", auth.RoleEditor, h.createTestRun)
	r.Handle("POST /be/v1/runs/{id}/resume", auth.RoleEditor, h.resumeRun)
	if h.services.Schedules != nil {
		r.Handle("GET /be/v1/schedules", auth.RoleViewer, h.listSchedules)
		r.Handle("POST /be/v1/schedules", auth.RoleEditor, h.createSchedule)
		r.Handle("POST /be/v1/schedules/preview", auth.RoleViewer, h.previewSchedule)
		r.Handle("PATCH /be/v1/schedules/{id}", auth.RoleEditor, h.updateSchedule)
		r.Handle("DELETE /be/v1/schedules/{id}", auth.RoleEditor, h.deleteSchedule)
		r.Handle("POST /be/v1/schedules/{id}/run", auth.RoleEditor, h.runSchedule)
	}
	if h.services.Alerts != nil {
		r.Handle("GET /be/v1/alert-channels", auth.RoleEditor, h.listAlertChannels)
		r.Handle("POST /be/v1/alert-channels", auth.RoleAdmin, h.createAlertChannel)
		r.Handle("PATCH /be/v1/alert-channels/{id}", auth.RoleAdmin, h.updateAlertChannel)
		r.Handle("DELETE /be/v1/alert-channels/{id}", auth.RoleAdmin, h.deleteAlertChannel)
		r.Handle("POST /be/v1/alert-channels/{id}/test", auth.RoleAdmin, h.testAlertChannel)
		r.Handle("GET /be/v1/alert-deliveries", auth.RoleEditor, h.listAlertDeliveries)
	}
}

// createTestRun is POST /be/v1/test-runs: run an unpublished definition with
// real executors. The draft is stored as a test version; nothing is published.
func (h *Handler) createTestRun(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var req struct {
		YAML  string         `json:"yaml"`
		Input map[string]any `json:"input"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	if req.YAML == "" {
		h.writeError(w, r, exception.BadRequest("yaml is required"))
		return
	}
	version, err := h.services.Workflows.PrepareTest(r.Context(), p.TenantID, []byte(req.YAML), p.Actor())
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	result, err := h.services.Runs.Start(r.Context(), runs.StartParams{
		TenantID: p.TenantID, Workflow: version.Workflow.Name, VersionID: version.Version.ID, Input: req.Input,
		IdempotencyKey: r.Header.Get("Idempotency-Key"), Trigger: "test", CreatedBy: p.Actor(),
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	view := runSummary(result.Run, result.WorkflowName, result.Version)
	view["spec"] = version.Spec
	httpx.WriteJSON(w, http.StatusCreated, view)
}

func (h *Handler) resumeRun(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	id, err := runID(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	result, err := h.services.Runs.Resume(r.Context(), p.TenantID, id, p.Actor(), r.Header.Get("Idempotency-Key"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	status := http.StatusCreated
	if !result.Created {
		status = http.StatusOK
	}
	httpx.WriteJSON(w, status, runSummary(result.Run, result.WorkflowName, result.Version))
}

// pathID parses {id}; a malformed ID is a 404 for what, like an unknown one.
func pathID(r *http.Request, what string) (pgtype.UUID, error) {
	id, err := uid.Parse(r.PathValue("id"))
	if err != nil {
		return pgtype.UUID{}, exception.NotFound(what)
	}
	return id, nil
}

func (h *Handler) listSchedules(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	out, err := h.services.Schedules.List(r.Context(), p.TenantID, r.URL.Query().Get("workflow"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *Handler) createSchedule(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var in schedules.Input
	if err := httpx.DecodeJSON(r, &in); err != nil {
		h.writeError(w, r, err)
		return
	}
	out, err := h.services.Schedules.Create(r.Context(), p.TenantID, p.Actor(), in)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, out)
}

func (h *Handler) previewSchedule(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var in struct {
		Cron     string `json:"cron"`
		Timezone string `json:"timezone"`
		Count    int    `json:"count"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		h.writeError(w, r, err)
		return
	}
	times, err := h.services.Schedules.Preview(in.Cron, in.Timezone, in.Count)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"next": times})
}

func (h *Handler) updateSchedule(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	id, err := pathID(r, "schedule")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	var in schedules.Input
	if err := httpx.DecodeJSON(r, &in); err != nil {
		h.writeError(w, r, err)
		return
	}
	out, err := h.services.Schedules.Update(r.Context(), p.TenantID, id, in)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) deleteSchedule(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	id, err := pathID(r, "schedule")
	if err == nil {
		err = h.services.Schedules.Delete(r.Context(), p.TenantID, id)
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) runSchedule(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	id, err := pathID(r, "schedule")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	result, err := h.services.Schedules.RunNow(r.Context(), p.TenantID, id, p.Actor())
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, runSummary(result.Run, result.WorkflowName, result.Version))
}

func (h *Handler) listAlertChannels(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	out, err := h.services.Alerts.List(r.Context(), p.TenantID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out, "events": alerts.Events})
}

func (h *Handler) createAlertChannel(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var in alerts.Input
	if err := httpx.DecodeJSON(r, &in); err != nil {
		h.writeError(w, r, err)
		return
	}
	out, err := h.services.Alerts.Create(r.Context(), p.TenantID, in)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, out)
}

func (h *Handler) updateAlertChannel(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	id, err := pathID(r, "alert channel")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	var in alerts.Input
	if err := httpx.DecodeJSON(r, &in); err != nil {
		h.writeError(w, r, err)
		return
	}
	out, err := h.services.Alerts.Update(r.Context(), p.TenantID, id, in)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) deleteAlertChannel(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	id, err := pathID(r, "alert channel")
	if err == nil {
		err = h.services.Alerts.Delete(r.Context(), p.TenantID, id)
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) testAlertChannel(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	id, err := pathID(r, "alert channel")
	if err == nil {
		err = h.services.Alerts.Test(r.Context(), p.TenantID, id)
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"sent": true})
}

func (h *Handler) listAlertDeliveries(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	limit, err := queryLimit(r, 50, 200)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	out, err := h.services.Alerts.Deliveries(r.Context(), p.TenantID, int32(limit))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}
