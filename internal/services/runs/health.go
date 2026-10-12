package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/uid"
)

// Alert events written to the alert outbox. Delivery lives in the alerts
// service; this package only records what happened, in the transaction that
// finished the run, so an alert is never lost or sent for a rolled-back end.
const (
	AlertRunFailed      = "run.failed"
	AlertRunRecovered   = "run.recovered"
	AlertConnectionAuth = "connection.reauth"
	AlertScheduleFailed = "schedule.failed"
)

// quietTriggers end runs that never alert: draft tests are expected to fail
// while someone edits, and a sub-workflow's failure surfaces on its parent.
var quietTriggers = map[string]bool{"test": true, "subworkflow": true}

// recordHealth tracks one open incident per workflow. The first failure after
// a success opens it and alerts; later failures only count; the next success
// closes it with a recovery. A lost account (E_CONNECTION_REAUTH) alerts at
// most daily per step, even inside an open incident, because it needs a person.
func recordHealth(ctx context.Context, q *gen.Queries, run gen.Run, workflow, status string, runError map[string]any) error {
	if quietTriggers[run.Trigger] || (status != "failed" && status != "succeeded") {
		return nil
	}
	// Retrying or resuming a draft changes its trigger but still executes an
	// unpublished version. It must not affect published workflow health.
	version, err := q.GetWorkflowVersion(ctx, gen.GetWorkflowVersionParams{ID: run.WorkflowVersionID, TenantID: run.TenantID})
	if err != nil {
		return fmt.Errorf("load alert workflow version: %w", err)
	}
	if version.IsTest {
		return nil
	}
	runID := uid.String(run.ID)
	base := map[string]any{"workflow": workflow, "run_id": runID, "trigger": run.Trigger}
	switch status {
	case "failed":
		failures, err := q.MarkWorkflowFailing(ctx, gen.MarkWorkflowFailingParams{WorkflowID: run.WorkflowID, OrgID: run.TenantID, RunID: run.ID})
		if err != nil {
			return fmt.Errorf("mark workflow failing: %w", err)
		}
		for _, key := range []string{"code", "node_id", "node_code", "message"} {
			if value, ok := runError[key]; ok {
				base[key] = value
			}
		}
		if failures == 1 {
			if err := enqueueAlert(ctx, q, run, workflow, AlertRunFailed, AlertRunFailed+":"+runID, base); err != nil {
				return err
			}
		}
		if runError["node_code"] == "E_CONNECTION_REAUTH" {
			day := time.Now().UTC().Format(time.DateOnly)
			key := fmt.Sprintf("%s:%s:%v:%s", AlertConnectionAuth, workflow, runError["node_id"], day)
			if err := enqueueAlert(ctx, q, run, workflow, AlertConnectionAuth, key, base); err != nil {
				return err
			}
		}
	case "succeeded":
		closed, err := q.MarkWorkflowHealthy(ctx, gen.MarkWorkflowHealthyParams{WorkflowID: run.WorkflowID, RunID: run.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // was not failing
		}
		if err != nil {
			return fmt.Errorf("mark workflow healthy: %w", err)
		}
		base["failures"] = closed.Failures
		if closed.FailingSince.Valid {
			base["failing_since"] = closed.FailingSince.Time.UTC().Format(time.RFC3339)
		}
		return enqueueAlert(ctx, q, run, workflow, AlertRunRecovered, AlertRunRecovered+":"+runID, base)
	}
	return nil
}

func enqueueAlert(ctx context.Context, q *gen.Queries, run gen.Run, workflow, event, key string, payload map[string]any) error {
	return EnqueueAlert(ctx, q, run.TenantID, workflow, event, key, payload)
}

// EnqueueAlert fans an event out to the organization's subscribed channels.
// Callers pass their transaction's queries.
func EnqueueAlert(ctx context.Context, q *gen.Queries, orgID pgtype.UUID, workflow, event, key string, payload map[string]any) error {
	payload["event"] = event
	payload["occurred_at"] = time.Now().UTC().Format(time.RFC3339)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s alert: %w", event, err)
	}
	if _, err := q.EnqueueAlert(ctx, gen.EnqueueAlertParams{
		OrgID: orgID, Event: event, DedupeKey: key, Payload: encoded, Workflow: workflow,
	}); err != nil {
		return fmt.Errorf("enqueue %s alert: %w", event, err)
	}
	return nil
}
