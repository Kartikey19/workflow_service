package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/exception"
)

// Allowance enforces the monthly run allowance of an organization (M11).
type Allowance interface {
	// CheckRun returns a *workflows.LimitError when the allowance is used up.
	CheckRun(ctx context.Context, tenantID pgtype.UUID) error
}

// Start applies the monthly run allowance, then starts the run. An idempotent
// replay of an existing run is not charged again.
func (s *Service) Start(ctx context.Context, p StartParams) (StartResult, error) {
	if s.Allowance != nil && !s.isReplay(ctx, p) {
		if err := s.Allowance.CheckRun(ctx, p.TenantID); err != nil {
			return StartResult{}, err
		}
	}
	return s.start(ctx, p)
}

func (s *Service) isReplay(ctx context.Context, p StartParams) bool {
	if p.IdempotencyKey == "" {
		return false
	}
	_, err := s.Q.GetRunByIdempotencyKey(ctx, gen.GetRunByIdempotencyKeyParams{
		TenantID: p.TenantID, IdempotencyKey: pgtype.Text{String: p.IdempotencyKey, Valid: true},
	})
	return err == nil
}

// Get returns a run with its workflow name and version. Every lookup is
// tenant-scoped: another organization's run is a 404.
func (s *Service) Get(ctx context.Context, tenantID, id pgtype.UUID) (gen.GetRunDetailRow, error) {
	row, err := s.Q.GetRunDetail(ctx, gen.GetRunDetailParams{ID: id, TenantID: tenantID})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.GetRunDetailRow{}, exception.NotFound("run")
	}
	if err != nil {
		return gen.GetRunDetailRow{}, fmt.Errorf("get run: %w", err)
	}
	return row, nil
}

// Progress counts the latest attempt of every node by status. total is the
// spec's node count, an upper bound.
func (s *Service) Progress(ctx context.Context, row gen.GetRunDetailRow) (map[string]int, error) {
	progress := map[string]int{"total": 0, "succeeded": 0, "failed": 0, "skipped": 0, "running": 0, "waiting": 0, "ready": 0, "cancelled": 0}
	if compiled, err := s.Specs.Get(ctx, s.Q, row.Run.WorkflowVersionID, row.Run.TenantID); err == nil {
		progress["total"] = len(compiled.Spec.Nodes)
	}
	states, err := s.Q.LatestNodeStates(ctx, row.Run.ID)
	if err != nil {
		return nil, fmt.Errorf("load node states: %w", err)
	}
	for _, state := range states {
		progress[state.Status]++
	}
	return progress, nil
}

// List returns up to limit runs (keyset paginated by params) and whether more
// follow.
func (s *Service) List(ctx context.Context, params gen.ListRunsParams, limit int) ([]gen.ListRunsRow, bool, error) {
	params.Lim = int32(limit + 1)
	rows, err := s.Q.ListRuns(ctx, params)
	if err != nil {
		return nil, false, fmt.Errorf("list runs: %w", err)
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	return rows, more, nil
}

// Nodes returns every attempt of every node of a run, in order.
func (s *Service) Nodes(ctx context.Context, tenantID, id pgtype.UUID) ([]gen.NodeExecution, error) {
	if _, err := s.Get(ctx, tenantID, id); err != nil {
		return nil, err
	}
	nodes, err := s.Q.ListRunNodes(ctx, gen.ListRunNodesParams{RunID: id, TenantID: tenantID})
	if err != nil {
		return nil, fmt.Errorf("list run nodes: %w", err)
	}
	return nodes, nil
}

// Events returns up to limit events with seq greater than after. seq is
// gap-free, so a consumer resuming from its last seq misses nothing.
func (s *Service) Events(ctx context.Context, tenantID, id pgtype.UUID, after, limit int32) ([]gen.RunEvent, error) {
	events, err := s.Q.ListRunEvents(ctx, gen.ListRunEventsParams{RunID: id, TenantID: tenantID, AfterSeq: after, Lim: limit})
	if err != nil {
		return nil, fmt.Errorf("list run events: %w", err)
	}
	return events, nil
}

// Status returns the current run status.
func (s *Service) Status(ctx context.Context, tenantID, id pgtype.UUID) (string, error) {
	run, err := s.Q.GetRun(ctx, gen.GetRunParams{ID: id, TenantID: tenantID})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", exception.NotFound("run")
	}
	if err != nil {
		return "", fmt.Errorf("get run: %w", err)
	}
	return run.Status, nil
}

// Retry starts a new run of the same version and input with parent_run_id set;
// deliberately not a resume.
func (s *Service) Retry(ctx context.Context, tenantID, id pgtype.UUID, actor, idempotencyKey string) (StartResult, error) {
	row, err := s.Get(ctx, tenantID, id)
	if err != nil {
		return StartResult{}, err
	}
	var input map[string]any
	if err := json.Unmarshal(row.Run.Input, &input); err != nil {
		return StartResult{}, fmt.Errorf("decode run input: %w", err)
	}
	return s.Start(ctx, StartParams{
		TenantID: tenantID, Workflow: row.WorkflowName, Version: row.WorkflowVersion, Input: input,
		CorrelationID: row.Run.CorrelationID.String, IdempotencyKey: idempotencyKey,
		Trigger: "retry", CreatedBy: actor, ParentRunID: row.Run.ID,
	})
}
