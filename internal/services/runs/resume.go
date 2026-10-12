package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/spec"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/db"
	"github.com/kartikey/workflow_service/pkg/exception"
	"github.com/kartikey/workflow_service/pkg/obs"
	"github.com/kartikey/workflow_service/pkg/uid"
)

// Resume starts a new run of the same version and input that reuses every
// result of a failed or cancelled run except the steps that failed or were
// cut short and everything after them. Those steps run again, so fixing a
// credential or a flaky service does not repeat the work (and side effects)
// that already succeeded. The original run is left untouched.
//
// The rerun set is the "failure cone": every failed or cancelled node plus all
// of its descendants. Results outside the cone were decided by dependencies
// that succeeded, so they are copied as they are; the cone's entry points had
// all dependencies finished and are scheduled as ready.
func (s *Service) Resume(ctx context.Context, tenantID, id pgtype.UUID, actor, idempotencyKey string) (StartResult, error) {
	source, err := s.Get(ctx, tenantID, id)
	if err != nil {
		return StartResult{}, err
	}
	if source.Run.Status != "failed" && source.Run.Status != "cancelled" {
		return StartResult{}, exception.Conflict("run_not_resumable", "only a failed or cancelled run can be resumed")
	}
	if s.Allowance != nil && !s.isReplay(ctx, StartParams{TenantID: tenantID, IdempotencyKey: idempotencyKey}) {
		if err := s.Allowance.CheckRun(ctx, tenantID); err != nil {
			return StartResult{}, err
		}
	}
	compiled, err := s.Specs.Get(ctx, s.Q, source.Run.WorkflowVersionID, tenantID)
	if err != nil {
		return StartResult{}, err
	}
	previous, err := s.Q.LatestSharedAttempts(ctx, source.Run.ID)
	if err != nil {
		return StartResult{}, fmt.Errorf("load results to reuse: %w", err)
	}
	reuse, entry := resumePlan(compiled.Spec, previous)
	requestHash := hashRequest(source.WorkflowName, source.WorkflowVersion, source.Run.Input, "resume:"+uid.String(source.Run.ID))

	result := StartResult{WorkflowName: source.WorkflowName, Version: source.WorkflowVersion}
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		run, err := q.CreateRun(ctx, gen.CreateRunParams{
			ID: uid.New(), TenantID: tenantID, WorkflowID: source.Run.WorkflowID, WorkflowVersionID: source.Run.WorkflowVersionID,
			Input: source.Run.Input, IdempotencyKey: optionalText(idempotencyKey), CorrelationID: source.Run.CorrelationID,
			Trigger: "resume", ParentRunID: source.Run.ID,
			DeadlineAt:  pgtype.Timestamptz{Time: time.Now().Add(time.Duration(compiled.Spec.TimeoutNS)), Valid: true},
			RequestHash: optionalText(requestHash), CreatedBy: optionalText(actor),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			existing, err := q.GetRunByIdempotencyKey(ctx, gen.GetRunByIdempotencyKeyParams{TenantID: tenantID, IdempotencyKey: optionalText(idempotencyKey)})
			if err != nil {
				return fmt.Errorf("load run for idempotency key: %w", err)
			}
			if existing.RequestHash.String != requestHash {
				return ErrIdempotencyConflict
			}
			result.Run = existing
			return nil
		}
		if err != nil {
			return fmt.Errorf("create resumed run: %w", err)
		}
		result.Run, result.Created = run, true
		if err := AppendEvent(ctx, q, run, "run.created", "", 0, map[string]any{"trigger": "resume", "resumed_from": uid.String(source.Run.ID)}); err != nil {
			return err
		}
		if err := AppendEvent(ctx, q, run, "run.started", "", 0, nil); err != nil {
			return err
		}
		reusedFrom, _ := json.Marshal(map[string]any{"reused_from_run": uid.String(source.Run.ID)})
		for _, row := range reuse {
			if _, err := q.CopyReusedNode(ctx, gen.CopyReusedNodeParams{
				ID: uid.New(), RunID: run.ID, TenantID: run.TenantID, NodeID: row.NodeID, NodeType: row.NodeType,
				Status: row.Status, Input: row.Input, Output: row.Output, Metrics: reusedFrom,
				IdempotencyKey: NodeKey(run.ID, row.NodeID, ""),
			}); err != nil {
				return fmt.Errorf("reuse result of %q: %w", row.NodeID, err)
			}
			if err := AppendEvent(ctx, q, run, "node.reused", row.NodeID, 1, map[string]any{"status": row.Status}); err != nil {
				return err
			}
		}
		for _, nodeID := range entry {
			node, _ := compiled.Spec.Node(nodeID)
			if _, err := q.EnqueueNode(ctx, gen.EnqueueNodeParams{
				ID: uid.New(), RunID: run.ID, TenantID: run.TenantID, NodeID: node.ID, NodeType: node.Type,
				Status: "ready", IdempotencyKey: NodeKey(run.ID, node.ID, ""),
			}); err != nil {
				return fmt.Errorf("enqueue %q: %w", nodeID, err)
			}
			if err := AppendEvent(ctx, q, run, "node.scheduled", node.ID, 1, nil); err != nil {
				return err
			}
		}
		// Nothing to rerun (the run failed on its outputs or deadline, say):
		// recompute finishes it from the reused results.
		if len(entry) == 0 {
			if err := s.Recompute(ctx, q, run, compiled); err != nil {
				return err
			}
		}
		if err := q.NotifyReady(ctx, uid.String(run.ID)); err != nil {
			return fmt.Errorf("notify ready: %w", err)
		}
		return q.NotifyRunEvents(ctx, uid.String(run.ID))
	})
	if err != nil {
		return StartResult{}, err
	}
	if result.Created {
		obs.RunsStarted.WithLabelValues("resume").Inc()
	}
	return result, nil
}

// resumePlan splits a finished run's latest shared attempts into results to
// copy and the nodes to schedule first. It is pure so it can be tested
// without a database.
func resumePlan(s spec.Spec, latest []gen.NodeExecution) (reuse []gen.NodeExecution, entry []string) {
	status := make(map[string]string, len(latest))
	for _, row := range latest {
		status[row.NodeID] = row.Status
	}
	cone := map[string]bool{}
	var mark func(string)
	mark = func(id string) {
		if cone[id] {
			return
		}
		cone[id] = true
		for _, next := range s.Edges[id] {
			mark(next)
		}
	}
	for _, row := range latest {
		switch row.Status {
		case "succeeded", "skipped":
		default: // failed, cancelled, or still marked live by a crash
			mark(row.NodeID)
		}
	}
	for _, row := range latest {
		if !cone[row.NodeID] {
			reuse = append(reuse, row)
		}
	}
	for _, node := range s.Nodes {
		if _, ran := status[node.ID]; !ran && len(node.Needs) == 0 {
			mark(node.ID) // a root that never ran (cancelled before start)
		}
	}
	for _, node := range s.Nodes {
		if !cone[node.ID] {
			continue
		}
		ready := true
		for _, dep := range node.Needs {
			if cone[dep] {
				ready = false
				break
			}
			if st := status[dep]; st != "succeeded" && st != "skipped" {
				ready = false
				break
			}
		}
		if ready {
			entry = append(entry, node.ID)
		}
	}
	return reuse, entry
}
