package flow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/ids"
	"github.com/kartikey/workflow_service/internal/jsonschema"
	"github.com/kartikey/workflow_service/internal/obs"
	"github.com/kartikey/workflow_service/internal/store"
	"github.com/kartikey/workflow_service/internal/store/gen"
)

// Cancel ends a run (03 §10). Ready and waiting nodes are cancelled now; running
// nodes stop at their next heartbeat, whose fenced extension fails once the run
// is terminal, and their results are discarded.
func (f *Flow) Cancel(ctx context.Context, tenantID, runID pgtype.UUID, reason string) error {
	return store.InTx(ctx, f.Pool, func(tx pgx.Tx) error {
		q := f.Q.WithTx(tx)
		run, err := q.LockRun(ctx, runID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && run.TenantID != tenantID) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock run: %w", err)
		}
		if IsTerminalRun(run.Status) {
			return ErrRunTerminal
		}
		if reason == "" {
			reason = "cancelled by request"
		}
		if _, err := q.CancelRun(ctx, gen.CancelRunParams{ID: runID, TenantID: tenantID, Reason: reason}); err != nil {
			return fmt.Errorf("cancel run: %w", err)
		}
		if _, err := q.CancelRunNodes(ctx, runID); err != nil {
			return fmt.Errorf("cancel run nodes: %w", err)
		}
		if err := q.CancelRunWaits(ctx, runID); err != nil {
			return fmt.Errorf("cancel run waits: %w", err)
		}
		if err := appendEvent(ctx, q, run, "run.cancelled", "", 0, map[string]any{"reason": reason}); err != nil {
			return err
		}
		obs.RunsFinished.WithLabelValues("cancelled").Inc()
		return q.NotifyRunEvents(ctx, ids.String(runID))
	})
}

// PayloadError is a signal payload that fails the wait's schema (422). The
// wait stays live so a corrected signal can still be delivered.
type PayloadError struct{ Fields []jsonschema.FieldError }

func (e *PayloadError) Error() string {
	return fmt.Sprintf("%d payload field(s) invalid", len(e.Fields))
}

// SignalResult names the node a signal completed.
type SignalResult struct {
	RunID     pgtype.UUID
	NodeID    string
	Duplicate bool
}

// Signal delivers payload to the single live waiter for key and advances the
// DAG in the same transaction (01 §6.7). With no waiter, a key that was already
// fulfilled returns the original result; otherwise ErrNoWaiter (v1 rejects early
// signals, 03 §7.3).
func (f *Flow) Signal(ctx context.Context, tenantID pgtype.UUID, key string, payload json.RawMessage) (SignalResult, error) {
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	runID, err := f.Q.PeekLiveWait(ctx, gen.PeekLiveWaitParams{TenantID: tenantID, WaitKey: key})
	if errors.Is(err, pgx.ErrNoRows) {
		previous, err := f.Q.LatestFulfilledWait(ctx, gen.LatestFulfilledWaitParams{TenantID: tenantID, WaitKey: key})
		if err == nil {
			node, err := f.Q.GetNodeExecution(ctx, previous.NodeExecutionID)
			if err == nil {
				obs.Signals.WithLabelValues("duplicate").Inc()
				return SignalResult{RunID: previous.RunID, NodeID: node.NodeID, Duplicate: true}, nil
			}
		}
		obs.Signals.WithLabelValues("no_waiter").Inc()
		return SignalResult{}, ErrNoWaiter
	}
	if err != nil {
		return SignalResult{}, fmt.Errorf("find waiter: %w", err)
	}

	var result SignalResult
	err = store.InTx(ctx, f.Pool, func(tx pgx.Tx) error {
		q := f.Q.WithTx(tx)
		run, err := LockRun(ctx, q, runID)
		if err != nil {
			return err
		}
		wait, err := q.LockLiveWait(ctx, gen.LockLiveWaitParams{TenantID: tenantID, WaitKey: key})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoWaiter // fulfilled or expired between peek and lock
		}
		if err != nil {
			return fmt.Errorf("lock waiter: %w", err)
		}
		if len(wait.Schema) > 0 {
			var schema any
			if err := json.Unmarshal(wait.Schema, &schema); err != nil {
				return fmt.Errorf("decode wait schema: %w", err)
			}
			if fields := jsonschema.ValidateJSON(schema, payload); len(fields) > 0 {
				return &PayloadError{Fields: fields}
			}
		}
		node, err := q.GetNodeExecution(ctx, wait.NodeExecutionID)
		if err != nil {
			return fmt.Errorf("load waiting node: %w", err)
		}
		if err := q.FulfillWait(ctx, gen.FulfillWaitParams{ID: wait.ID, Payload: payload}); err != nil {
			return fmt.Errorf("fulfil wait: %w", err)
		}
		rows, err := q.CompleteWaitingNode(ctx, gen.CompleteWaitingNodeParams{ID: node.ID, Status: "succeeded", Output: payload})
		if err != nil {
			return fmt.Errorf("complete waiting node: %w", err)
		}
		if rows == 0 {
			return ErrNoWaiter
		}
		if err := appendEvent(ctx, q, run, "node.signalled", node.NodeID, node.Attempt, map[string]any{"key": key}); err != nil {
			return err
		}
		if err := appendEvent(ctx, q, run, "node.succeeded", node.NodeID, node.Attempt, map[string]any{"output_bytes": len(payload)}); err != nil {
			return err
		}
		compiled, err := f.Specs.Get(ctx, q, run.WorkflowVersionID, run.TenantID)
		if err != nil {
			return err
		}
		if err := f.Advance(ctx, q, run, compiled, Completed{NodeID: node.NodeID, BranchKey: node.BranchKey, Status: "succeeded"}); err != nil {
			return err
		}
		result = SignalResult{RunID: run.ID, NodeID: node.NodeID}
		return q.NotifyRunEvents(ctx, ids.String(run.ID))
	})
	if err != nil {
		var payloadErr *PayloadError
		switch {
		case errors.Is(err, ErrNoWaiter):
			obs.Signals.WithLabelValues("no_waiter").Inc()
		case errors.As(err, &payloadErr):
			obs.Signals.WithLabelValues("invalid_payload").Inc()
		}
		return SignalResult{}, err
	}
	obs.Signals.WithLabelValues("delivered").Inc()
	return result, nil
}

// WakeDue completes elapsed sleeps and times out expired waits (03 §8.2). Each
// node is handled in its own transaction under its run lock.
func (f *Flow) WakeDue(ctx context.Context) (int, error) {
	due, err := f.Q.DueWaitingNodes(ctx)
	if err != nil {
		return 0, fmt.Errorf("load due waiting nodes: %w", err)
	}
	handled := 0
	for _, node := range due {
		err := store.InTx(ctx, f.Pool, func(tx pgx.Tx) error {
			q := f.Q.WithTx(tx)
			run, err := LockRun(ctx, q, node.RunID)
			if err != nil {
				return err
			}
			if IsTerminalRun(run.Status) {
				return nil
			}
			status, eventType := "succeeded", "node.succeeded"
			var errorJSON []byte
			if node.NodeType != "sleep" {
				status, eventType = "failed", "node.timeout"
				errorJSON, _ = json.Marshal(map[string]any{"code": "wait_timeout", "message": "wait timed out", "retryable": false})
				if err := q.ExpireNodeWaits(ctx, node.ID); err != nil {
					return fmt.Errorf("expire wait: %w", err)
				}
			}
			rows, err := q.CompleteWaitingNode(ctx, gen.CompleteWaitingNodeParams{ID: node.ID, Status: status, Error: errorJSON})
			if err != nil {
				return fmt.Errorf("complete waiting node: %w", err)
			}
			if rows == 0 {
				return nil // another sweeper or a signal got there first
			}
			if err := appendEvent(ctx, q, run, eventType, node.NodeID, node.Attempt, nil); err != nil {
				return err
			}
			compiled, err := f.Specs.Get(ctx, q, run.WorkflowVersionID, run.TenantID)
			if err != nil {
				return err
			}
			if err := f.Advance(ctx, q, run, compiled, Completed{NodeID: node.NodeID, BranchKey: node.BranchKey, Status: status}); err != nil {
				return err
			}
			handled++
			return q.NotifyRunEvents(ctx, ids.String(run.ID))
		})
		if err != nil {
			return handled, err
		}
	}
	if handled > 0 {
		obs.SweeperActions.WithLabelValues("wake_waiting").Add(float64(handled))
	}
	return handled, nil
}

// FailPastDeadline fails runs whose deadline_at passed (03 §8.3).
func (f *Flow) FailPastDeadline(ctx context.Context) (int, error) {
	runs, err := f.Q.RunsPastDeadline(ctx)
	if err != nil {
		return 0, fmt.Errorf("load runs past deadline: %w", err)
	}
	failed := 0
	for _, candidate := range runs {
		err := store.InTx(ctx, f.Pool, func(tx pgx.Tx) error {
			q := f.Q.WithTx(tx)
			run, err := LockRun(ctx, q, candidate.ID)
			if err != nil {
				return err
			}
			if IsTerminalRun(run.Status) {
				return nil
			}
			failed++
			if err := f.finishRun(ctx, q, run, "failed", nil, map[string]any{
				"code": "run_deadline_exceeded", "message": "run exceeded its timeout",
			}); err != nil {
				return err
			}
			return q.NotifyRunEvents(ctx, ids.String(run.ID))
		})
		if err != nil {
			return failed, err
		}
	}
	if failed > 0 {
		obs.SweeperActions.WithLabelValues("run_deadline").Add(float64(failed))
	}
	return failed, nil
}
