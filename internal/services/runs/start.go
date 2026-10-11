package runs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/core/expr"
	"github.com/kartikey/workflow_service/internal/spec"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/db"
	"github.com/kartikey/workflow_service/pkg/obs"
	"github.com/kartikey/workflow_service/pkg/uid"
)

var (
	ErrNotFound            = errors.New("not found")
	ErrArchived            = errors.New("workflow is archived")
	ErrIdempotencyConflict = errors.New("idempotency key was used with a different request")
	ErrRunTerminal         = errors.New("run already finished")
	ErrNoWaiter            = errors.New("no live waiter for key")
)

// InputError lists per-field input validation failures (400).
type InputError struct{ Fields []spec.FieldError }

func (e *InputError) Error() string { return fmt.Sprintf("%d input field(s) invalid", len(e.Fields)) }

type StartParams struct {
	TenantID       pgtype.UUID
	Workflow       string
	Version        int32 // 0 pins the latest version at creation (04 §3.1)
	Input          map[string]any
	CorrelationID  string
	IdempotencyKey string
	Trigger        string // api | ui | webhook | schedule | assistant
	CreatedBy      string
	ParentRunID    pgtype.UUID
}

type StartResult struct {
	Run          gen.Run
	Created      bool
	WorkflowName string
	Version      int32
}

// start creates a run and its root nodes in one transaction (04 §3.1). A repeated
// idempotency key returns the existing run; a reused key with a different
// request body is ErrIdempotencyConflict.
func (f *Service) start(ctx context.Context, p StartParams) (StartResult, error) {
	workflow, err := f.Q.GetWorkflowByName(ctx, gen.GetWorkflowByNameParams{TenantID: p.TenantID, Name: p.Workflow})
	if errors.Is(err, pgx.ErrNoRows) {
		return StartResult{}, ErrNotFound
	}
	if err != nil {
		return StartResult{}, fmt.Errorf("load workflow: %w", err)
	}
	if workflow.ArchivedAt.Valid {
		return StartResult{}, ErrArchived
	}
	var version gen.WorkflowVersion
	if p.Version > 0 {
		version, err = f.Q.GetWorkflowVersionByNumber(ctx, gen.GetWorkflowVersionByNumberParams{WorkflowID: workflow.ID, Version: p.Version})
	} else {
		version, err = f.Q.LatestWorkflowVersion(ctx, workflow.ID)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return StartResult{}, ErrNotFound
	}
	if err != nil {
		return StartResult{}, fmt.Errorf("load workflow version: %w", err)
	}
	compiled, err := f.Specs.Get(ctx, f.Q, version.ID, p.TenantID)
	if err != nil {
		return StartResult{}, err
	}

	input, fieldErrors := spec.ValidateInput(compiled.Spec.Inputs, p.Input)
	if len(fieldErrors) > 0 {
		return StartResult{}, &InputError{Fields: fieldErrors}
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return StartResult{}, fmt.Errorf("encode run input: %w", err)
	}
	correlation := p.CorrelationID
	if compiled.Programs.Correlation != nil {
		value, err := compiled.Programs.Correlation.Resolve(ctx, bindingsForInput(input))
		if err != nil {
			return StartResult{}, &InputError{Fields: []spec.FieldError{{Field: "correlation", Message: "could not be evaluated from input"}}}
		}
		correlation = fmt.Sprint(value)
	}
	requestHash := hashRequest(p.Workflow, version.Version, inputJSON, correlation)
	trigger := p.Trigger
	if trigger == "" {
		trigger = "api"
	}

	result := StartResult{WorkflowName: workflow.Name, Version: version.Version}
	err = db.InTx(ctx, f.Pool, func(tx pgx.Tx) error {
		q := f.Q.WithTx(tx)
		run, err := q.CreateRun(ctx, gen.CreateRunParams{
			ID: uid.New(), TenantID: p.TenantID, WorkflowID: workflow.ID, WorkflowVersionID: version.ID,
			Input:          inputJSON,
			IdempotencyKey: optionalText(p.IdempotencyKey),
			CorrelationID:  optionalText(correlation),
			Trigger:        trigger,
			ParentRunID:    p.ParentRunID,
			DeadlineAt:     pgtype.Timestamptz{Time: time.Now().Add(time.Duration(compiled.Spec.TimeoutNS)), Valid: true},
			RequestHash:    optionalText(requestHash),
			CreatedBy:      optionalText(p.CreatedBy),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			existing, err := q.GetRunByIdempotencyKey(ctx, gen.GetRunByIdempotencyKeyParams{
				TenantID: p.TenantID, IdempotencyKey: optionalText(p.IdempotencyKey),
			})
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
			return fmt.Errorf("create run: %w", err)
		}
		result.Run, result.Created = run, true
		if err := AppendEvent(ctx, q, run, "run.created", "", 0, map[string]any{"trigger": trigger}); err != nil {
			return err
		}
		if err := AppendEvent(ctx, q, run, "run.started", "", 0, nil); err != nil {
			return err
		}
		// Only roots exist at start; successors are created lazily (05 §9).
		for _, root := range compiled.Spec.Roots {
			node, _ := compiled.Spec.Node(root)
			if _, err := q.EnqueueNode(ctx, gen.EnqueueNodeParams{
				ID: uid.New(), RunID: run.ID, TenantID: run.TenantID, NodeID: node.ID, NodeType: node.Type,
				Status: "ready", IdempotencyKey: NodeKey(run.ID, node.ID, ""),
			}); err != nil {
				return fmt.Errorf("enqueue root %q: %w", root, err)
			}
			if err := AppendEvent(ctx, q, run, "node.scheduled", node.ID, 1, nil); err != nil {
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
		obs.RunsStarted.WithLabelValues(trigger).Inc()
	}
	return result, nil
}

// bindingsForInput is the correlation scope: input only, no nodes (02 §3.4).
func bindingsForInput(input map[string]any) expr.Bindings {
	return expr.Bindings{Input: input, Now: time.Now()}
}

func hashRequest(workflow string, version int32, input []byte, correlation string) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%s\x00%s", workflow, version, input, correlation))
	return hex.EncodeToString(sum[:])
}

func optionalText(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: value != ""}
}
