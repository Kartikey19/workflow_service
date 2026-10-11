package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/kartikey/workflow_service/pkg/uid"
	"go.uber.org/zap"

	"github.com/kartikey/workflow_service/internal/core/expr"
	"github.com/kartikey/workflow_service/internal/services/runs"
	"github.com/kartikey/workflow_service/internal/spec"
	"github.com/kartikey/workflow_service/internal/store/gen"
)

// A foreach node runs as one controller row (branch_key '') and one row per list
// element (branch_key = the element's index). The controller evaluates items,
// inserts the branch rows and parks; the last branch to finish completes it
// (see runs.Service.Advance), so the node looks like any other to its successors.

// fanOut is the controller's whole execution: it never calls the node's executor.
func (d *Dispatcher) fanOut(parent, ctx context.Context, ne gen.NodeExecution, compiled *spec.Compiled, node spec.NodeSpec, bindings expr.Bindings) {
	items, err := compiled.Programs.Nodes[node.ID].Items.EvalList(ctx, bindings)
	if err != nil {
		d.failPermanent(ctx, ne, compiled, "E_EXPR_EVAL", fmt.Errorf("evaluate foreach items: %w", err))
		return
	}
	if len(items) > node.Foreach.MaxItems {
		d.failPermanent(ctx, ne, compiled, "E_FOREACH_TOO_MANY",
			fmt.Errorf("foreach items has %d elements, over the limit of %d (max_items)", len(items), node.Foreach.MaxItems))
		return
	}
	if budget := d.runs.MaxNodesPerRun; budget > 0 {
		count, err := d.store.CountRunNodeExecutions(ctx, ne.RunID)
		if err != nil {
			d.logger.Error("count run node executions", zap.Error(err)) // transient: the lease expires and the node is redelivered
			return
		}
		if count+int64(len(items)) > int64(budget) {
			d.failPermanent(ctx, ne, compiled, "E_RUN_BUDGET",
				fmt.Errorf("foreach would exceed the run budget of %d node executions", budget))
			return
		}
	}
	metrics := encodeMetrics(map[string]any{"items": len(items)}, 0)
	if len(items) == 0 {
		d.persist(parent, ctx, ne, compiled, outcome{status: "succeeded", output: []byte(`[]`), metrics: metrics})
		return
	}
	encoded := make([][]byte, len(items))
	for i, item := range items {
		data, err := json.Marshal(item)
		if err != nil {
			d.failPermanent(ctx, ne, compiled, "E_FOREACH_ITEM", fmt.Errorf("encode item %d: %w", i, err))
			return
		}
		if len(data) > d.maxPayload {
			d.failPermanent(ctx, ne, compiled, "input_too_large", fmt.Errorf("item %d is %d bytes, over the %d byte limit", i, len(data), d.maxPayload))
			return
		}
		encoded[i] = data
	}
	d.persist(parent, ctx, ne, compiled, outcome{status: "waiting", fanout: encoded, metrics: metrics})
}

// branchBindings sets item and index for a branch from the row's stored element.
func branchBindings(ne gen.NodeExecution, bindings expr.Bindings) (expr.Bindings, error) {
	index, err := strconv.Atoi(ne.BranchKey)
	if err != nil || index < 0 {
		return bindings, fmt.Errorf("branch key %q is not a list index", ne.BranchKey)
	}
	var item any
	if len(ne.Item) > 0 {
		if err := json.Unmarshal(ne.Item, &item); err != nil {
			return bindings, fmt.Errorf("decode foreach item: %w", err)
		}
	}
	bindings.Item, bindings.Index = item, index
	return bindings, nil
}

// insertBranches creates one ready row per element in the controller's transaction.
func (d *Dispatcher) insertBranches(ctx context.Context, q *gen.Queries, run gen.Run, ne gen.NodeExecution, items [][]byte) error {
	for i, item := range items {
		key := strconv.Itoa(i)
		if _, err := q.EnqueueNode(ctx, gen.EnqueueNodeParams{
			ID: uid.New(), RunID: run.ID, TenantID: run.TenantID, NodeID: ne.NodeID, NodeType: ne.NodeType,
			BranchKey: key, Status: "ready", IdempotencyKey: runs.NodeKey(run.ID, ne.NodeID, key), Item: item,
		}); err != nil && !errors.Is(err, pgx.ErrNoRows) { // ErrNoRows: the branch already exists
			return fmt.Errorf("enqueue branch %d: %w", i, err)
		}
	}
	return nil
}
