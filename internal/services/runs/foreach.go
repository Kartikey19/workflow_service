package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/kartikey/workflow_service/internal/spec"
	"github.com/kartikey/workflow_service/internal/store/gen"
)

// MaxFanoutOutputBytes bounds the aggregated output of a foreach node: the list
// of every branch's output.
const MaxFanoutOutputBytes = 8 << 20

// maxReportedBranchFailures bounds how many failed branches the controller's
// error lists.
const maxReportedBranchFailures = 10

// advanceBranch handles a finished foreach branch. A branch's successors are
// the foreach node's successors, so nothing is enqueued until the last branch
// is terminal; that branch completes the controller row, and the node then
// advances exactly like any other (03 §5). It runs under the run row lock, in
// the transaction that wrote the branch's terminal state, so exactly one
// branch can observe "no branch is open".
func (f *Service) advanceBranch(ctx context.Context, q *gen.Queries, run gen.Run, compiled *spec.Compiled, done Completed) error {
	if done.Status == "failed" {
		// One failed branch fails the node, so branches that have not started
		// are not worth running.
		if _, err := q.CancelReadyBranches(ctx, gen.CancelReadyBranchesParams{RunID: run.ID, NodeID: done.NodeID}); err != nil {
			return fmt.Errorf("cancel unstarted branches: %w", err)
		}
	}
	open, err := q.CountOpenBranches(ctx, gen.CountOpenBranchesParams{RunID: run.ID, NodeID: done.NodeID})
	if err != nil {
		return fmt.Errorf("count open branches: %w", err)
	}
	if open > 0 {
		return f.Recompute(ctx, q, run, compiled)
	}
	rows, err := q.ListBranchResults(ctx, gen.ListBranchResultsParams{RunID: run.ID, NodeID: done.NodeID})
	if err != nil {
		return fmt.Errorf("list branch results: %w", err)
	}
	status, output, failure := aggregateBranches(rows)
	attempt, err := q.FinishFanout(ctx, gen.FinishFanoutParams{
		RunID: run.ID, NodeID: done.NodeID, Status: status, Output: output, Error: failure,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return f.Recompute(ctx, q, run, compiled) // the controller was already completed
	}
	if err != nil {
		return fmt.Errorf("finish foreach node: %w", err)
	}
	if err := AppendEvent(ctx, q, run, "node."+status, done.NodeID, attempt, map[string]any{"branches": len(rows)}); err != nil {
		return err
	}
	return f.Advance(ctx, q, run, compiled, Completed{NodeID: done.NodeID, BranchKey: "", Status: status})
}

// aggregateBranches folds the latest attempt of every branch into the
// controller's terminal state. The output is the list of branch outputs in list
// order (null for a branch that was skipped or cancelled). One failed branch
// fails the node; its error lists which branches failed and why.
func aggregateBranches(rows []gen.ListBranchResultsRow) (status string, output, failure []byte) {
	type branch struct {
		index int
		row   gen.ListBranchResultsRow
	}
	ordered := make([]branch, 0, len(rows))
	for _, row := range rows {
		index, err := strconv.Atoi(row.BranchKey)
		if err != nil {
			continue
		}
		ordered = append(ordered, branch{index, row})
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].index < ordered[j].index })

	outputs := make([]json.RawMessage, 0, len(ordered))
	var failed []map[string]any
	failedCount := 0
	for _, b := range ordered {
		value := json.RawMessage("null")
		if b.row.Status == "succeeded" && len(b.row.Output) > 0 {
			value = b.row.Output
		}
		outputs = append(outputs, value)
		if b.row.Status == "failed" {
			failedCount++
			if len(failed) < maxReportedBranchFailures {
				var cause any
				_ = json.Unmarshal(b.row.Error, &cause)
				failed = append(failed, map[string]any{"index": b.index, "error": cause})
			}
		}
	}
	if failedCount > 0 {
		failure, _ = json.Marshal(map[string]any{
			"code": "E_FOREACH_BRANCH_FAILED", "retryable": false, "branches": failed,
			"message": fmt.Sprintf("%d of %d branches failed", failedCount, len(ordered)),
		})
		return "failed", nil, failure
	}
	output, _ = json.Marshal(outputs)
	if len(output) > MaxFanoutOutputBytes {
		failure, _ = json.Marshal(map[string]any{
			"code": "output_too_large", "retryable": false,
			"message": fmt.Sprintf("the branch outputs total %d bytes, over the %d byte limit", len(output), MaxFanoutOutputBytes),
		})
		return "failed", nil, failure
	}
	return "succeeded", output, nil
}
