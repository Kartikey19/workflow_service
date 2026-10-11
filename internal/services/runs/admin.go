package runs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/db"
	"github.com/kartikey/workflow_service/pkg/exception"
	"github.com/kartikey/workflow_service/pkg/obs"
	"github.com/kartikey/workflow_service/pkg/uid"
)

// Operator interventions for stuck work. They are cross-tenant by design (the
// caller is a platform admin), follow the same lock order as the engine (run
// row first), and never force a write: a node that moved on is a 409.

// NodeAdminResult describes the effect of an intervention.
type NodeAdminResult struct {
	RunID     pgtype.UUID
	NodeID    string
	BranchKey string
	// Attempt is the attempt now in play: the new attempt for a retry, the
	// stopped attempt for a skip.
	Attempt int32
	Status  string
}

func notRetryable(detail string) error { return exception.Conflict("node_not_retryable", detail) }

// RetryNode inserts attempt+1 of a node as ready. A live attempt (ready,
// running or waiting) is cancelled first, so a late result from its worker is
// discarded by fencing; a failed attempt in a still-live run is retried as is.
func (f *Service) RetryNode(ctx context.Context, nodeExecutionID pgtype.UUID, actor string) (NodeAdminResult, error) {
	var result NodeAdminResult
	err := f.withNodeAndRun(ctx, nodeExecutionID, func(q *gen.Queries, run gen.Run, ne gen.NodeExecution) error {
		if IsTerminalRun(run.Status) {
			return exception.Conflict("run_terminal", "the run has finished; retry the run instead")
		}
		latest, err := q.LatestNodeStates(ctx, run.ID)
		if err != nil {
			return fmt.Errorf("load node states: %w", err)
		}
		for _, state := range latest {
			if state.NodeID == ne.NodeID && state.BranchKey == ne.BranchKey && state.Attempt != ne.Attempt {
				return notRetryable("a later attempt of this node already exists")
			}
		}
		switch ne.Status {
		case "ready", "running", "waiting":
			_, err := q.AdminStopNode(ctx, gen.AdminStopNodeParams{ID: ne.ID, Status: "cancelled"})
			if errors.Is(err, pgx.ErrNoRows) {
				return notRetryable("the attempt finished while the retry was being applied")
			}
			if err != nil {
				return fmt.Errorf("cancel live attempt: %w", err)
			}
			expired, err := q.ExpireNodeWaits(ctx, ne.ID)
			if err != nil {
				return fmt.Errorf("expire waits: %w", err)
			}
			if err := f.cancelWaitedChildren(ctx, q, run, expired); err != nil {
				return err
			}
		case "failed":
		default:
			return notRetryable(fmt.Sprintf("a %s attempt cannot be retried", ne.Status))
		}
		next := ne.Attempt + 1
		rows, err := q.EnqueueRetry(ctx, gen.EnqueueRetryParams{
			ID: uid.New(), RunID: run.ID, TenantID: run.TenantID, NodeID: ne.NodeID, NodeType: ne.NodeType,
			BranchKey: ne.BranchKey, Attempt: next, IdempotencyKey: ne.IdempotencyKey, Item: ne.Item,
			RunAfter: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		})
		if err != nil {
			return fmt.Errorf("enqueue retry: %w", err)
		}
		if rows == 0 {
			return notRetryable("a later attempt of this node already exists")
		}
		if err := AppendEvent(ctx, q, run, "admin.node_retried", ne.NodeID, next, map[string]any{"actor": actor, "from_status": ne.Status}); err != nil {
			return err
		}
		if err := q.NotifyReady(ctx, uid.String(run.ID)); err != nil {
			return fmt.Errorf("notify ready: %w", err)
		}
		if err := q.NotifyRunEvents(ctx, uid.String(run.ID)); err != nil {
			return fmt.Errorf("notify run events: %w", err)
		}
		result = NodeAdminResult{RunID: run.ID, NodeID: ne.NodeID, BranchKey: ne.BranchKey, Attempt: next, Status: "ready"}
		return nil
	})
	return result, err
}

// SkipNode force-skips a live node and advances the run as if the node had
// been skipped by its own condition, so skip semantics cascade normally.
func (f *Service) SkipNode(ctx context.Context, nodeExecutionID pgtype.UUID, actor string) (NodeAdminResult, error) {
	var result NodeAdminResult
	err := f.withNodeAndRun(ctx, nodeExecutionID, func(q *gen.Queries, run gen.Run, ne gen.NodeExecution) error {
		if IsTerminalRun(run.Status) {
			return exception.Conflict("run_terminal", "the run has already finished")
		}
		stopped, err := q.AdminStopNode(ctx, gen.AdminStopNodeParams{ID: ne.ID, Status: "skipped"})
		if errors.Is(err, pgx.ErrNoRows) {
			return exception.Conflict("node_not_skippable", fmt.Sprintf("a %s attempt cannot be skipped", ne.Status))
		}
		if err != nil {
			return fmt.Errorf("skip node: %w", err)
		}
		expired, err := q.ExpireNodeWaits(ctx, ne.ID)
		if err != nil {
			return fmt.Errorf("expire waits: %w", err)
		}
		if err := f.cancelWaitedChildren(ctx, q, run, expired); err != nil {
			return err
		}
		if err := AppendEvent(ctx, q, run, "admin.node_skipped", ne.NodeID, ne.Attempt, map[string]any{"actor": actor, "from_status": ne.Status}); err != nil {
			return err
		}
		compiled, err := f.Specs.Get(ctx, q, run.WorkflowVersionID, run.TenantID)
		if err != nil {
			return err
		}
		if err := f.Advance(ctx, q, run, compiled, Completed{NodeID: stopped.NodeID, BranchKey: stopped.BranchKey, Status: "skipped"}); err != nil {
			return err
		}
		if err := q.NotifyRunEvents(ctx, uid.String(run.ID)); err != nil {
			return fmt.Errorf("notify run events: %w", err)
		}
		result = NodeAdminResult{RunID: run.ID, NodeID: ne.NodeID, BranchKey: ne.BranchKey, Attempt: ne.Attempt, Status: "skipped"}
		return nil
	})
	return result, err
}

// ReleaseWorkerLeases hands every running node of a crashed worker back to the
// ready queue now, instead of waiting for the leases to expire. A worker that
// is in fact alive loses its fence and discards its results. It returns the
// number of nodes released.
func (f *Service) ReleaseWorkerLeases(ctx context.Context, workerID string) (int, error) {
	var released int
	err := db.InTx(ctx, f.Pool, func(tx pgx.Tx) error {
		q := f.Q.WithTx(tx)
		ids, err := q.ReleaseWorkerLeases(ctx, pgtype.Text{String: workerID, Valid: true})
		if err != nil {
			return fmt.Errorf("release worker leases: %w", err)
		}
		released = len(ids)
		if released > 0 {
			return q.NotifyReady(ctx, "admin")
		}
		return nil
	})
	if err == nil && released > 0 {
		obs.SweeperActions.WithLabelValues("lease_released").Add(float64(released))
	}
	return released, err
}

// withNodeAndRun locks the node's run, reloads the node under the lock and
// runs fn in one transaction.
func (f *Service) withNodeAndRun(ctx context.Context, nodeExecutionID pgtype.UUID, fn func(*gen.Queries, gen.Run, gen.NodeExecution) error) error {
	peek, err := f.Q.GetNodeExecution(ctx, nodeExecutionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return exception.NotFound("node execution")
	}
	if err != nil {
		return fmt.Errorf("get node execution: %w", err)
	}
	return db.InTx(ctx, f.Pool, func(tx pgx.Tx) error {
		q := f.Q.WithTx(tx)
		run, err := q.LockRun(ctx, peek.RunID)
		if err != nil {
			return fmt.Errorf("lock run: %w", err)
		}
		ne, err := q.GetNodeExecution(ctx, nodeExecutionID)
		if err != nil {
			return fmt.Errorf("reload node execution: %w", err)
		}
		return fn(q, run, ne)
	})
}
