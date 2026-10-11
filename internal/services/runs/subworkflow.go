package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/db"
	"github.com/kartikey/workflow_service/pkg/obs"
	"github.com/kartikey/workflow_service/pkg/uid"
)

// A sub-workflow is a child run (trigger "subworkflow", parent_run_id set) plus
// an ordinary wait on the parent's node, keyed by the child's run ID. When the
// child ends, the parent's waiting node is completed and the parent advances;
// nothing else in the engine knows sub-workflows exist.

// MaxSubworkflowDepth bounds nesting, which also stops a workflow that calls
// itself: the run at depth 6 is refused.
const MaxSubworkflowDepth = 5

const subworkflowKeyPrefix = "subworkflow:"

func toPG(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: [16]byte(id), Valid: true} }

var _ executor.ChildStarter = (*Service)(nil)

// StartChild starts the child run of a sub-workflow node. The request's
// idempotency key is the parent node's stable key, so repeating it (a retried
// attempt, or a redelivery after a crash) returns the run the first call made.
// Child runs are not charged against the monthly run allowance.
func (f *Service) StartChild(ctx context.Context, req executor.ChildRequest) (uuid.UUID, error) {
	tenantID, parentID := toPG(req.TenantID), toPG(req.ParentRunID)
	depth, err := f.Q.RunDepth(ctx, parentID)
	if err != nil {
		return uuid.Nil(), fmt.Errorf("measure sub-workflow depth: %w", err)
	}
	if int(depth)+1 > MaxSubworkflowDepth {
		return uuid.Nil(), executor.Permanent("E_SUBWORKFLOW_DEPTH",
			fmt.Errorf("sub-workflows may be nested at most %d deep", MaxSubworkflowDepth))
	}
	result, err := f.start(ctx, StartParams{
		TenantID: tenantID, Workflow: req.Workflow, Version: int32(req.Version), Input: req.Input,
		IdempotencyKey: req.IdempotencyKey, Trigger: "subworkflow", CreatedBy: "subworkflow", ParentRunID: parentID,
	})
	var inputErr *InputError
	switch {
	case errors.Is(err, ErrNotFound):
		return uuid.Nil(), executor.Permanent("E_SUBWORKFLOW_NOT_FOUND",
			fmt.Errorf("workflow %q (version %s) does not exist", req.Workflow, versionLabel(req.Version)))
	case errors.Is(err, ErrArchived):
		return uuid.Nil(), executor.Permanent("E_SUBWORKFLOW_ARCHIVED", fmt.Errorf("workflow %q is archived", req.Workflow))
	case errors.As(err, &inputErr):
		return uuid.Nil(), executor.Permanent("E_SUBWORKFLOW_INPUT", fmt.Errorf("input for %q is invalid: %v", req.Workflow, inputErr.Fields))
	case errors.Is(err, ErrIdempotencyConflict):
		return uuid.Nil(), executor.Permanent("E_SUBWORKFLOW_CONFLICT",
			errors.New("the child run was already started with different input"))
	case err != nil:
		return uuid.Nil(), err
	}
	return uuid.UUID(result.Run.ID.Bytes), nil
}

func versionLabel(version int) string {
	if version == 0 {
		return "latest"
	}
	return fmt.Sprint(version)
}

// CompleteSubworkflow finishes the parent's node that waits on a child run that
// has ended. It is safe to call at any time, any number of times: with the child
// still running, or no node waiting on it, it does nothing. It takes only the
// parent's run lock, so it cannot deadlock with a parent cancelling its
// children (parent first, then child).
func (f *Service) CompleteSubworkflow(ctx context.Context, childRunID pgtype.UUID) error {
	child, err := f.Q.GetRunAny(ctx, childRunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load child run: %w", err)
	}
	if !IsTerminalRun(child.Status) || !child.ParentRunID.Valid {
		return nil
	}
	key := subworkflowKeyPrefix + uid.String(child.ID)
	return db.InTx(ctx, f.Pool, func(tx pgx.Tx) error {
		q := f.Q.WithTx(tx)
		parent, err := LockRun(ctx, q, child.ParentRunID)
		if err != nil {
			return err
		}
		if IsTerminalRun(parent.Status) {
			return nil
		}
		wait, err := q.LockLiveWait(ctx, gen.LockLiveWaitParams{TenantID: parent.TenantID, WaitKey: key})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // not waiting yet, or already completed
		}
		if err != nil {
			return fmt.Errorf("lock sub-workflow wait: %w", err)
		}
		if wait.RunID != parent.ID {
			return nil // a user-chosen key that only looks like ours
		}
		node, err := q.GetNodeExecution(ctx, wait.NodeExecutionID)
		if err != nil {
			return fmt.Errorf("load waiting node: %w", err)
		}
		status, output, failure := childOutcome(child)
		if err := q.FulfillWait(ctx, gen.FulfillWaitParams{ID: wait.ID, Payload: output}); err != nil {
			return fmt.Errorf("fulfil sub-workflow wait: %w", err)
		}
		rows, err := q.CompleteWaitingNode(ctx, gen.CompleteWaitingNodeParams{ID: node.ID, Status: status, Output: output, Error: failure})
		if err != nil {
			return fmt.Errorf("complete sub-workflow node: %w", err)
		}
		if rows == 0 {
			return nil
		}
		payload := map[string]any{"child_run_id": uid.String(child.ID), "child_status": child.Status}
		if node.BranchKey != "" {
			payload["branch"] = node.BranchKey
		}
		if err := AppendEvent(ctx, q, parent, "node."+status, node.NodeID, node.Attempt, payload); err != nil {
			return err
		}
		compiled, err := f.Specs.Get(ctx, q, parent.WorkflowVersionID, parent.TenantID)
		if err != nil {
			return err
		}
		if err := f.Advance(ctx, q, parent, compiled, Completed{NodeID: node.NodeID, BranchKey: node.BranchKey, Status: status}); err != nil {
			return err
		}
		return q.NotifyRunEvents(ctx, uid.String(parent.ID))
	})
}

// childOutcome maps a finished child run to the parent node's terminal state.
// A succeeded node's output is {run_id, status, output}; a failed child fails
// the node with the child's error attached.
func childOutcome(child gen.Run) (status string, output, failure []byte) {
	if child.Status == "succeeded" {
		result := map[string]any{"run_id": uid.String(child.ID), "status": child.Status, "output": rawOrNull(child.Output)}
		output, _ = json.Marshal(result)
		return "succeeded", output, nil
	}
	code, message := "E_SUBWORKFLOW_FAILED", "the sub-workflow run failed"
	if child.Status == "cancelled" {
		code, message = "E_SUBWORKFLOW_CANCELLED", "the sub-workflow run was cancelled"
	}
	failure, _ = json.Marshal(map[string]any{
		"code": code, "message": message, "retryable": false,
		"child_run_id": uid.String(child.ID), "child_error": rawOrNull(child.Error),
	})
	// The wait payload records how the child ended even when the node failed.
	output, _ = json.Marshal(map[string]any{"run_id": uid.String(child.ID), "status": child.Status})
	return "failed", output, failure
}

func rawOrNull(data []byte) json.RawMessage {
	if len(data) == 0 {
		return json.RawMessage("null")
	}
	return json.RawMessage(data)
}

// PropagateFinishedChildren is the sweeper pass behind CompleteSubworkflow: it
// completes parent nodes whose child ended but whose propagation was missed.
func (f *Service) PropagateFinishedChildren(ctx context.Context) (int, error) {
	children, err := f.Q.FinishedChildrenWithLiveWait(ctx)
	if err != nil {
		return 0, fmt.Errorf("find finished children: %w", err)
	}
	handled := 0
	for _, child := range children {
		if err := f.CompleteSubworkflow(ctx, child); err != nil {
			return handled, err
		}
		handled++
	}
	if handled > 0 {
		obs.SweeperActions.WithLabelValues("subworkflow_completed").Add(float64(handled))
	}
	return handled, nil
}

// cancelActiveChildren cancels every unfinished child of a run that is ending,
// recursively. The caller holds the parent's lock; children are locked after it,
// the same order as everywhere else (parent, then child).
func (f *Service) cancelActiveChildren(ctx context.Context, q *gen.Queries, parent gen.Run, reason string) error {
	children, err := q.ListActiveChildRuns(ctx, parent.ID)
	if err != nil {
		return fmt.Errorf("list child runs: %w", err)
	}
	for _, listed := range children {
		child, err := LockRun(ctx, q, listed.ID)
		if err != nil {
			return err
		}
		if IsTerminalRun(child.Status) {
			continue
		}
		if err := f.cancelLocked(ctx, q, child, reason); err != nil {
			return err
		}
	}
	return nil
}

// cancelWaitedChildren cancels the children behind expired sub-workflow waits:
// when a parent stops waiting (timeout, admin retry or skip), the child's work
// is no longer wanted.
func (f *Service) cancelWaitedChildren(ctx context.Context, q *gen.Queries, parent gen.Run, expiredKeys []string) error {
	for _, key := range expiredKeys {
		if !strings.HasPrefix(key, subworkflowKeyPrefix) {
			continue
		}
		id, err := uuid.Parse(strings.TrimPrefix(key, subworkflowKeyPrefix))
		if err != nil {
			continue
		}
		child, err := q.GetRunAny(ctx, toPG(id))
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (child.ParentRunID != parent.ID || IsTerminalRun(child.Status))) {
			continue
		}
		if err != nil {
			return fmt.Errorf("load child run: %w", err)
		}
		locked, err := LockRun(ctx, q, child.ID)
		if err != nil {
			return err
		}
		if IsTerminalRun(locked.Status) {
			continue
		}
		if err := f.cancelLocked(ctx, q, locked, "the parent stopped waiting for this sub-workflow"); err != nil {
			return err
		}
	}
	return nil
}
