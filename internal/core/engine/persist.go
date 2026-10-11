package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/internal/services/runs"
	"github.com/kartikey/workflow_service/internal/spec"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/db"
	"github.com/kartikey/workflow_service/pkg/obs"
	"github.com/kartikey/workflow_service/pkg/uid"
	"go.uber.org/zap"
	"maps"
	"time"
)

// outcome is one attempt's terminal or parked write.
type outcome struct {
	status  string // succeeded | failed | skipped | waiting
	code    string // error code for metrics; "" on success
	output  []byte
	err     []byte
	metrics []byte
	retryAt time.Time // non-zero: keep the failed row and enqueue attempt+1
	gotoID  *string   // branch: the chosen successor
	park    *park     // sleep / wait_event
	fanout  [][]byte  // foreach controller: the encoded elements, one branch each
}

type park struct {
	until time.Time
	wait  *executor.WaitSpec
}

// markStarted records the resolved input and the node.started event.
func (d *Dispatcher) markStarted(ctx context.Context, ne gen.NodeExecution, input []byte) error {
	return db.InTx(ctx, d.runs.Pool, func(tx pgx.Tx) error {
		q := d.store.WithTx(tx)
		run, err := runs.LockRun(ctx, q, ne.RunID)
		if err != nil {
			return err
		}
		rows, err := q.SetNodeInput(ctx, gen.SetNodeInputParams{ID: ne.ID, LeaseToken: ne.LeaseToken, WorkerID: d.leaseOwner(), Input: input})
		if err != nil {
			return fmt.Errorf("set node input: %w", err)
		}
		if rows == 0 {
			return errLeaseLost
		}
		if err := runs.AppendEvent(ctx, q, run, "node.started", ne.NodeID, ne.Attempt, branchPayload(ne.BranchKey, map[string]any{"worker_id": d.cfg.ID})); err != nil {
			return err
		}
		return q.NotifyRunEvents(ctx, uid.String(ne.RunID))
	})
}

// persist writes an attempt's outcome and advances the run in one transaction
// (03 §4.4). parent outlives the heartbeat; ctx is cancelled on lease loss.
// A zero-row fenced write means another worker owns the node: the result is
// discarded and logged, never forced or retried.
func (d *Dispatcher) persist(parent, ctx context.Context, ne gen.NodeExecution, compiled *spec.Compiled, o outcome) {
	if err := ctx.Err(); err != nil {
		// The heartbeat stopped: the lease was lost, the run ended, or the worker
		// is shutting down. A cancelled run's node is marked cancelled now.
		obs.LeaseLost.Inc()
		d.logger.Warn("discard node result", zap.Error(err),
			zap.String("node_execution_id", uid.String(ne.ID)), zap.String("status", o.status))
		d.cancelStale(parent, ne, o.metrics)
		return
	}

	err := db.InTx(ctx, d.runs.Pool, func(tx pgx.Tx) error {
		q := d.store.WithTx(tx)
		run, err := runs.LockRun(ctx, q, ne.RunID)
		if err != nil {
			return err
		}
		if runs.IsTerminalRun(run.Status) {
			rows, err := q.CancelRunningNode(ctx, gen.CancelRunningNodeParams{ID: ne.ID, LeaseToken: ne.LeaseToken, WorkerID: d.leaseOwner(), Metrics: o.metrics})
			if err == nil && rows == 0 {
				return errLeaseLost // another worker already finished this node
			}
			return err
		}
		if o.park != nil && o.park.wait != nil {
			_, err := q.PeekLiveWait(ctx, gen.PeekLiveWaitParams{TenantID: ne.TenantID, WaitKey: o.park.wait.Key})
			if err == nil {
				// Another live run waits on this key: fail loudly now rather than
				// misroute a signal later (03 §7.3).
				o = permanentOutcome("E_WAIT_KEY_CONFLICT", fmt.Errorf("another run is already waiting on key %q", o.park.wait.Key), o.metrics)
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("check wait key: %w", err)
			}
		}

		if o.fanout != nil {
			// The controller parks without a wake-up time: the last branch to
			// finish completes it, not the clock (runs.Service.Advance).
			rows, err := q.ParkNode(ctx, gen.ParkNodeParams{
				ID: ne.ID, LeaseToken: ne.LeaseToken, WorkerID: d.leaseOwner(), Output: o.output, Metrics: o.metrics,
				RunAfter: pgtype.Timestamptz{InfinityModifier: pgtype.Infinity, Valid: true},
			})
			if err != nil {
				return fmt.Errorf("park foreach controller: %w", err)
			}
			if rows == 0 {
				return errLeaseLost
			}
			if err := d.insertBranches(ctx, q, run, ne, o.fanout); err != nil {
				return err
			}
			if err := runs.AppendEvent(ctx, q, run, "node.fanout", ne.NodeID, ne.Attempt, map[string]any{"branches": len(o.fanout)}); err != nil {
				return err
			}
			if err := q.NotifyReady(ctx, uid.String(ne.RunID)); err != nil {
				return fmt.Errorf("notify ready: %w", err)
			}
			if err := d.runs.Recompute(ctx, q, run, compiled); err != nil {
				return err
			}
			return q.NotifyRunEvents(ctx, uid.String(ne.RunID))
		}

		if o.park != nil {
			rows, err := q.ParkNode(ctx, gen.ParkNodeParams{
				ID: ne.ID, LeaseToken: ne.LeaseToken, WorkerID: d.leaseOwner(), Output: o.output, Metrics: o.metrics,
				RunAfter: pgtype.Timestamptz{Time: o.park.until, Valid: true},
			})
			if err != nil {
				return fmt.Errorf("park node: %w", err)
			}
			if rows == 0 {
				return errLeaseLost
			}
			payload := map[string]any{"until": o.park.until.UTC().Format(time.RFC3339)}
			if o.park.wait != nil {
				waitID := uid.New()
				if err := q.InsertWait(ctx, gen.InsertWaitParams{
					ID: waitID, TenantID: ne.TenantID, RunID: ne.RunID, NodeExecutionID: ne.ID,
					WaitKey: o.park.wait.Key, ExpiresAt: pgtype.Timestamptz{Time: o.park.wait.Expires, Valid: true},
					Schema: optionalJSON(o.park.wait.Schema),
				}); err != nil {
					return fmt.Errorf("register wait: %w", err)
				}
				payload["key"] = o.park.wait.Key
				if task := o.park.wait.Task; task != nil {
					taskID := uid.New()
					if err := q.InsertTask(ctx, gen.InsertTaskParams{
						ID: taskID, TenantID: ne.TenantID, RunID: ne.RunID, NodeExecutionID: ne.ID, WaitID: waitID,
						NodeID: ne.NodeID, Queue: task.Queue, Form: optionalText(task.Form), Assignee: optionalText(task.Assignee),
						Payload: optionalJSON(task.Payload), DueAt: pgtype.Timestamptz{Time: o.park.wait.Expires, Valid: true},
					}); err != nil {
						return fmt.Errorf("register task: %w", err)
					}
					payload["task_id"] = uid.String(taskID)
				}
			}
			if err := runs.AppendEvent(ctx, q, run, "node.waiting", ne.NodeID, ne.Attempt, payload); err != nil {
				return err
			}
			if err := d.runs.Recompute(ctx, q, run, compiled); err != nil {
				return err
			}
			return q.NotifyRunEvents(ctx, uid.String(ne.RunID))
		}

		rows, err := q.CompleteNode(ctx, gen.CompleteNodeParams{
			ID: ne.ID, LeaseToken: ne.LeaseToken, WorkerID: d.leaseOwner(), Status: o.status,
			Output: o.output, Error: o.err, Metrics: o.metrics,
		})
		if err != nil {
			return fmt.Errorf("complete node execution: %w", err)
		}
		if rows == 0 {
			return errLeaseLost
		}
		payload := map[string]any{}
		if o.code != "" {
			payload["code"] = o.code
		}
		if len(o.output) > 0 {
			payload["output_bytes"] = len(o.output)
		}
		if err := runs.AppendEvent(ctx, q, run, "node."+o.status, ne.NodeID, ne.Attempt, branchPayload(ne.BranchKey, payload)); err != nil {
			return err
		}

		if !o.retryAt.IsZero() && d.runs.MaxNodesPerRun > 0 {
			count, err := q.CountRunNodeExecutions(ctx, ne.RunID)
			if err != nil {
				return fmt.Errorf("count run node executions: %w", err)
			}
			if count >= int64(d.runs.MaxNodesPerRun) {
				// Poison-node protection (03 §6.4): the budget wins over retries.
				return d.runs.FailRun(ctx, q, run, "E_RUN_BUDGET", fmt.Sprintf("run exceeded %d node executions", d.runs.MaxNodesPerRun))
			}
		}
		if !o.retryAt.IsZero() {
			if _, err := q.EnqueueRetry(ctx, gen.EnqueueRetryParams{
				ID: uid.New(), RunID: ne.RunID, TenantID: ne.TenantID, NodeID: ne.NodeID,
				NodeType: ne.NodeType, BranchKey: ne.BranchKey, Attempt: ne.Attempt + 1,
				IdempotencyKey: ne.IdempotencyKey, // identical across attempts (I5)
				RunAfter:       pgtype.Timestamptz{Time: o.retryAt, Valid: true},
				Item:           ne.Item, // a foreach branch retries the same element
			}); err != nil {
				return fmt.Errorf("enqueue node retry: %w", err)
			}
			if err := runs.AppendEvent(ctx, q, run, "node.retrying", ne.NodeID, ne.Attempt+1, map[string]any{
				"delay_ms": time.Until(o.retryAt).Milliseconds(),
			}); err != nil {
				return err
			}
			return q.NotifyRunEvents(ctx, uid.String(ne.RunID))
		}

		if compiled == nil {
			// The spec itself could not be loaded; nothing can advance this run.
			if err := d.runs.FailRun(ctx, q, run, "E_SPEC_DECODE", "published workflow spec could not be loaded"); err != nil {
				return err
			}
		} else if err := d.runs.Advance(ctx, q, run, compiled, runs.Completed{
			NodeID: ne.NodeID, BranchKey: ne.BranchKey, Status: o.status, Goto: o.gotoID,
		}); err != nil {
			return err
		}
		return q.NotifyRunEvents(ctx, uid.String(ne.RunID))
	})

	switch {
	case errors.Is(err, errLeaseLost):
		obs.LeaseLost.Inc()
		d.logLeaseError(ctx, ne, err)
	case err != nil:
		d.logger.Error("persist node outcome", zap.Error(err),
			zap.String("node_execution_id", uid.String(ne.ID)), zap.String("status", o.status))
	default:
		obs.NodeAttempts.WithLabelValues(ne.NodeType, o.status, o.code).Inc()
	}
}

// cancelStale marks this worker's running node cancelled when its run already
// ended (cancelled, failed, or past its deadline). Fenced by lease owner.
func (d *Dispatcher) cancelStale(parent context.Context, ne gen.NodeExecution, metrics []byte) {
	if parent.Err() != nil {
		return // shutdown: the lease is released or expires and the node is re-delivered
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	run, err := d.store.GetRun(ctx, gen.GetRunParams{ID: ne.RunID, TenantID: ne.TenantID})
	if err != nil || !runs.IsTerminalRun(run.Status) {
		return
	}
	if _, err := d.store.CancelRunningNode(ctx, gen.CancelRunningNodeParams{ID: ne.ID, LeaseToken: ne.LeaseToken, WorkerID: d.leaseOwner(), Metrics: metrics}); err != nil {
		d.logger.Warn("mark stale node cancelled", zap.Error(err), zap.String("node_execution_id", uid.String(ne.ID)))
	}
}

// failPermanent fails the attempt without retry, for deterministic errors found
// before or instead of executing the node.
func (d *Dispatcher) failPermanent(ctx context.Context, ne gen.NodeExecution, compiled *spec.Compiled, code string, err error) {
	d.logger.Warn("fail node execution permanently", zap.Error(err), zap.String("code", code),
		zap.String("node_execution_id", uid.String(ne.ID)), zap.String("node_id", ne.NodeID))
	d.persist(ctx, ctx, ne, compiled, permanentOutcome(code, err, nil))
}

func permanentOutcome(code string, err error, metrics []byte) outcome {
	return outcome{status: "failed", code: code, err: errorJSON(code, err, false), metrics: metrics}
}

// encodeMetrics adds the engine-measured duration to executor metrics. Values the
// executor supplied must not block the result write, so unencodable metrics are
// replaced by the duration alone.
func encodeMetrics(metrics map[string]any, duration time.Duration) []byte {
	out := make(map[string]any, len(metrics)+1)
	maps.Copy(out, metrics)
	out["duration_ms"] = duration.Milliseconds()
	encoded, err := json.Marshal(out)
	if err != nil {
		encoded, _ = json.Marshal(map[string]any{"duration_ms": duration.Milliseconds()})
	}
	return encoded
}

// optionalJSON stores a wait_event payload schema; JSON null means none.
func optionalText(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: value != ""}
}

func optionalJSON(schema json.RawMessage) []byte {
	if len(schema) == 0 || string(schema) == "null" {
		return nil
	}
	return schema
}

// branchPayload tags a foreach branch's events with its list index; events
// carry no branch column, and a timeline of 100 identical node.succeeded rows
// is not readable.
func branchPayload(branchKey string, payload map[string]any) map[string]any {
	if branchKey == "" {
		return payload
	}
	if payload == nil {
		payload = map[string]any{}
	}
	payload["branch"] = branchKey
	return payload
}
