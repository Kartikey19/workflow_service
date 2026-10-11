package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/spec"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/obs"
	"github.com/kartikey/workflow_service/pkg/uid"
)

// Completed is a node that just reached a terminal status inside the caller's
// transaction. Goto is set only for a branch node: the chosen successor, or ""
// when no arm was chosen.
type Completed struct {
	NodeID    string
	BranchKey string
	Status    string
	Goto      *string
}

// Advance enqueues or skips successors of a terminal node and recomputes the run
// status (03 §5). The caller holds the run row lock taken by LockRun, in the same
// transaction that wrote the node's terminal state.
func (f *Service) Advance(ctx context.Context, q *gen.Queries, run gen.Run, compiled *spec.Compiled, done Completed) error {
	if done.BranchKey != "" {
		if node, ok := compiled.Spec.Node(done.NodeID); ok && node.Foreach != nil {
			return f.advanceBranch(ctx, q, run, compiled, done)
		}
	}
	if budget := f.MaxNodesPerRun; budget > 0 {
		count, err := q.CountRunNodeExecutions(ctx, run.ID)
		if err != nil {
			return fmt.Errorf("count run node executions: %w", err)
		}
		if count >= int64(budget) {
			return f.finishRun(ctx, q, run, "failed", nil, map[string]any{
				"code": "E_RUN_BUDGET", "message": fmt.Sprintf("run exceeded %d node executions", budget),
			})
		}
	}

	type item struct{ nodeID, status string }
	queue := []item{{done.NodeID, done.Status}}
	notify := false
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, successorID := range compiled.Spec.Edges[current.nodeID] {
			successor, ok := compiled.Spec.Node(successorID)
			if !ok {
				return fmt.Errorf("successor %q missing from spec", successorID)
			}
			var next string
			if done.Goto != nil && current.nodeID == done.NodeID && successorID != *done.Goto {
				next = "skipped" // branch arm not taken (03 §5.3)
			} else {
				states, err := q.DepStates(ctx, gen.DepStatesParams{
					RunID: run.ID, BranchKey: done.BranchKey, DepNodeIds: successor.Needs,
				})
				if err != nil {
					return fmt.Errorf("load dependency states of %q: %w", successorID, err)
				}
				if len(states) < len(successor.Needs) || slices.ContainsFunc(states, func(s gen.DepStatesRow) bool {
					return !isTerminalNode(s.Status)
				}) {
					continue // not our turn: whoever finishes last enqueues it (Race B)
				}
				next = readiness(compiled.Spec, successor, states)
			}

			_, err := q.EnqueueNode(ctx, gen.EnqueueNodeParams{
				ID: uid.New(), RunID: run.ID, TenantID: run.TenantID,
				NodeID: successor.ID, NodeType: successor.Type, BranchKey: done.BranchKey,
				Status: next, IdempotencyKey: NodeKey(run.ID, successor.ID, done.BranchKey),
			})
			if errors.Is(err, pgx.ErrNoRows) {
				continue // another resolver inserted it first (Race A)
			}
			if err != nil {
				return fmt.Errorf("enqueue node %q: %w", successorID, err)
			}
			if next == "skipped" {
				if err := AppendEvent(ctx, q, run, "node.skipped", successorID, 1, nil); err != nil {
					return err
				}
				queue = append(queue, item{successorID, "skipped"})
				continue
			}
			if err := AppendEvent(ctx, q, run, "node.scheduled", successorID, 1, nil); err != nil {
				return err
			}
			notify = true
		}
	}
	if notify {
		if err := q.NotifyReady(ctx, uid.String(run.ID)); err != nil {
			return fmt.Errorf("notify ready: %w", err)
		}
	}
	return f.Recompute(ctx, q, run, compiled)
}

// readiness applies join semantics once every dependency is terminal. A node
// named as some dependency's on_failure/on_timeout is a handler: it runs only
// when such a dependency failed, and is skipped on the happy path.
func readiness(s spec.Spec, node spec.NodeSpec, states []gen.DepStatesRow) string {
	handlerOf := false
	handled := false
	succeeded := 0
	for _, state := range states {
		dependency, _ := s.Node(state.NodeID)
		if dependency.OnFailure == node.ID || dependency.OnTimeout == node.ID {
			handlerOf = true
			if state.Status == "failed" {
				handled = true
			}
		}
		if state.Status == "succeeded" {
			succeeded++
		}
	}
	switch {
	case handlerOf && handled:
		return "ready"
	case handlerOf:
		return "skipped"
	case node.Join == "any" && succeeded > 0:
		return "ready"
	case node.Join != "any" && succeeded == len(states):
		return "ready"
	default:
		return "skipped"
	}
}

// Recompute derives the run status from the latest attempt of every node
// (01 §3.2) and completes the run when nothing remains runnable.
func (f *Service) Recompute(ctx context.Context, q *gen.Queries, run gen.Run, compiled *spec.Compiled) error {
	states, err := q.LatestNodeStates(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("load node states: %w", err)
	}
	active, waiting := false, false
	var unhandled *gen.LatestNodeStatesRow
	for i, state := range states {
		switch state.Status {
		case "ready", "running":
			active = true
		case "waiting":
			waiting = true
		case "failed":
			node, _ := compiled.Spec.Node(state.NodeID)
			if node.OnFailure == "" && node.OnTimeout == "" && unhandled == nil {
				unhandled = &states[i]
			}
		}
	}
	switch {
	case active:
		return setStatus(ctx, q, run, "running")
	case waiting:
		return setStatus(ctx, q, run, "waiting")
	case unhandled != nil:
		message := "node failed"
		var nodeError struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(unhandled.Error, &nodeError) == nil && nodeError.Message != "" {
			message = nodeError.Message
		}
		return f.finishRun(ctx, q, run, "failed", nil, map[string]any{
			"code": "node_failed", "node_id": unhandled.NodeID, "node_code": nodeError.Code, "message": message,
		})
	}

	bindings, err := Bindings(ctx, q, run, "")
	if err != nil {
		return err
	}
	var output any = map[string]any{}
	if compiled.Programs.Outputs != nil {
		output, err = compiled.Programs.Outputs.Resolve(ctx, bindings)
		if err != nil {
			return f.finishRun(ctx, q, run, "failed", nil, map[string]any{
				"code": "E_OUTPUTS", "message": "workflow outputs could not be evaluated: " + err.Error(),
			})
		}
	}
	return f.finishRun(ctx, q, run, "succeeded", output, nil)
}

func setStatus(ctx context.Context, q *gen.Queries, run gen.Run, status string) error {
	if run.Status == status {
		return nil
	}
	if err := q.SetRunStatus(ctx, gen.SetRunStatusParams{ID: run.ID, Status: status}); err != nil {
		return fmt.Errorf("set run status: %w", err)
	}
	return nil
}

// finishRun ends the run and cancels anything still pending, so a budget or
// deadline failure cannot leave runnable rows behind (I4).
func (f *Service) finishRun(ctx context.Context, q *gen.Queries, run gen.Run, status string, output any, runError map[string]any) error {
	var outputJSON, errorJSON []byte
	var err error
	if output != nil {
		if outputJSON, err = json.Marshal(output); err != nil {
			return fmt.Errorf("encode run output: %w", err)
		}
	}
	if runError != nil {
		if errorJSON, err = json.Marshal(runError); err != nil {
			return fmt.Errorf("encode run error: %w", err)
		}
	}
	rows, err := q.FinishRun(ctx, gen.FinishRunParams{ID: run.ID, Status: status, Output: outputJSON, Error: errorJSON})
	if err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	if rows == 0 {
		return nil // already terminal
	}
	if _, err := q.CancelRunNodes(ctx, run.ID); err != nil {
		return fmt.Errorf("cancel pending nodes: %w", err)
	}
	if err := q.CancelRunWaits(ctx, run.ID); err != nil {
		return fmt.Errorf("cancel run waits: %w", err)
	}
	if err := f.cancelActiveChildren(ctx, q, run, "the parent run ended"); err != nil {
		return err
	}
	payload := map[string]any{}
	if runError != nil {
		payload["code"] = runError["code"]
	}
	if err := AppendEvent(ctx, q, run, "run."+status, "", 0, payload); err != nil {
		return err
	}
	obs.RunsFinished.WithLabelValues(status).Inc()
	if run.StartedAt.Valid {
		if workflow, err := q.GetWorkflowName(ctx, run.WorkflowID); err == nil {
			obs.RunDuration.WithLabelValues(workflow, status).Observe(time.Since(run.StartedAt.Time).Seconds())
		}
	}
	return nil
}

// LockRun takes the run row lock that serialises every transition of one run.
func LockRun(ctx context.Context, q *gen.Queries, runID pgtype.UUID) (gen.Run, error) {
	run, err := q.LockRun(ctx, runID)
	if err != nil {
		return gen.Run{}, fmt.Errorf("lock run: %w", err)
	}
	return run, nil
}

// FailRun ends a run as failed with code, for callers that cannot advance it.
func (f *Service) FailRun(ctx context.Context, q *gen.Queries, run gen.Run, code, message string) error {
	return f.finishRun(ctx, q, run, "failed", nil, map[string]any{"code": code, "message": message})
}
