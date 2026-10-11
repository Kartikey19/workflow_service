package runs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kartikey/workflow_service/internal/core/expr"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/uid"
	"go.uber.org/zap"
)

const defaultMaxNodesPerRun = 1000

// Service is safe for concurrent use; build one per process.
type Service struct {
	// Allowance is optional; nil means unlimited run starts.
	Allowance Allowance

	Pool  *pgxpool.Pool
	Q     *gen.Queries
	Specs *SpecCache
	Env   *expr.Env
	Log   *zap.Logger

	// MaxNodesPerRun bounds node executions per run (E_RUN_BUDGET, 03 §6.4).
	MaxNodesPerRun int
}

func New(pool *pgxpool.Pool, log *zap.Logger) (*Service, error) {
	env, err := expr.NewEnv()
	if err != nil {
		return nil, fmt.Errorf("create expression environment: %w", err)
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Service{
		Pool: pool, Q: gen.New(pool), Specs: NewSpecCache(env, 512), Env: env, Log: log,
		MaxNodesPerRun: defaultMaxNodesPerRun,
	}, nil
}

// Terminal run and node statuses.
func IsTerminalRun(status string) bool {
	return status == "succeeded" || status == "failed" || status == "cancelled"
}

func isTerminalNode(status string) bool {
	switch status {
	case "succeeded", "failed", "skipped", "cancelled":
		return true
	}
	return false
}

// AppendEvent writes one run event. Callers hold the run row lock, which makes
// the max(seq)+1 allocation gap-free (I6). Payloads carry summaries only.
func AppendEvent(ctx context.Context, q *gen.Queries, run gen.Run, eventType, nodeID string, attempt int32, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", eventType, err)
	}
	params := gen.AppendEventParams{RunID: run.ID, TenantID: run.TenantID, Type: eventType, Payload: encoded}
	if nodeID != "" {
		params.NodeID = pgtype.Text{String: nodeID, Valid: true}
		params.BranchKey = pgtype.Text{String: "", Valid: true}
	}
	if attempt > 0 {
		params.Attempt = pgtype.Int4{Int32: attempt, Valid: true}
	}
	if err := q.AppendEvent(ctx, params); err != nil {
		return fmt.Errorf("append %s event: %w", eventType, err)
	}
	return nil
}

// Bindings loads expression data for one run: the input and every finished
// node, in one query (03 §4.2). Secrets are added by the caller when needed.
func Bindings(ctx context.Context, q *gen.Queries, run gen.Run, branchKey string) (expr.Bindings, error) {
	input := make(map[string]any)
	if len(run.Input) > 0 {
		if err := json.Unmarshal(run.Input, &input); err != nil {
			return expr.Bindings{}, fmt.Errorf("decode run input: %w", err)
		}
	}
	if input == nil {
		input = make(map[string]any)
	}
	rows, err := q.LatestAttemptsForRun(ctx, gen.LatestAttemptsForRunParams{RunID: run.ID, BranchKey: branchKey})
	if err != nil {
		return expr.Bindings{}, fmt.Errorf("load finished node attempts: %w", err)
	}
	nodes := make(map[string]expr.NodeView, len(rows))
	for _, row := range rows {
		view := expr.NodeView{Status: row.Status, Attempt: int(row.Attempt), Metrics: map[string]any{}}
		if len(row.Output) > 0 {
			if err := json.Unmarshal(row.Output, &view.Output); err != nil {
				return expr.Bindings{}, fmt.Errorf("decode output of node %q: %w", row.NodeID, err)
			}
		}
		if len(row.Metrics) > 0 {
			if err := json.Unmarshal(row.Metrics, &view.Metrics); err != nil {
				return expr.Bindings{}, fmt.Errorf("decode metrics of node %q: %w", row.NodeID, err)
			}
		}
		nodes[row.NodeID] = view
	}
	startedAt := run.CreatedAt.Time
	if run.StartedAt.Valid {
		startedAt = run.StartedAt.Time
	}
	return expr.Bindings{
		Input: input,
		Nodes: nodes,
		Run: expr.RunView{
			ID: uid.String(run.ID), CorrelationID: run.CorrelationID.String,
			StartedAt: startedAt, Attempt: 1,
		},
		Secrets: map[string]string{},
		Now:     time.Now(),
	}, nil
}
