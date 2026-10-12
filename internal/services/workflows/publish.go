// Package workflows publishes immutable workflow versions (02 §8).
package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/internal/spec"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/db"
	"github.com/kartikey/workflow_service/pkg/uid"
)

// CompileError carries every diagnostic of a rejected publish (400).
type CompileError struct{ Diagnostics []spec.Diagnostic }

func (e *CompileError) Error() string {
	count := 0
	for _, d := range e.Diagnostics {
		if d.Severity == "error" {
			count++
		}
	}
	return fmt.Sprintf("%d error(s) found while compiling the workflow", count)
}

// LimitError is a plan limit violation (402).
type LimitError struct {
	Limit   string
	Allowed int64
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("plan limit reached: %s (allowed %d)", e.Limit, e.Allowed)
}

var ErrArchived = errors.New("workflow is archived")

// Limits are the plan allowances checked at publish; zero means unlimited.
type Limits struct {
	MaxWorkflows int64
	MaxNodes     int64
}

// Service publishes and reads workflows. Set Plans to enforce plan limits.
type Service struct {
	Plans Plans

	pool     *pgxpool.Pool
	q        *gen.Queries
	registry *executor.Registry
}

func NewService(pool *pgxpool.Pool, registry *executor.Registry) *Service {
	return &Service{pool: pool, q: gen.New(pool), registry: registry}
}

type PublishParams struct {
	TenantID  pgtype.UUID
	Source    []byte
	CreatedBy string
	DryRun    bool
	Limits    Limits
}

type PublishResult struct {
	Workflow    gen.Workflow
	Version     gen.WorkflowVersion
	Spec        spec.Spec
	Unchanged   bool
	Diagnostics []spec.Diagnostic // warnings when accepted
}

// Publish compiles source and stores version max+1, or returns the latest
// version unchanged when the compiled spec hash matches (02 §8 step 3).
func (s *Service) Publish(ctx context.Context, p PublishParams) (PublishResult, error) {
	if s.Plans != nil {
		limits, err := s.Plans.PublishLimits(ctx, p.TenantID)
		if err != nil {
			return PublishResult{}, err
		}
		p.Limits = limits
	}
	compiled, diagnostics := spec.CompileWith(p.Source, spec.Options{Registry: s.registry})
	if compiled == nil || hasErrors(diagnostics) {
		return PublishResult{}, &CompileError{Diagnostics: diagnostics}
	}
	if p.Limits.MaxNodes > 0 && int64(len(compiled.Spec.Nodes)) > p.Limits.MaxNodes {
		return PublishResult{}, &LimitError{Limit: "steps per workflow", Allowed: p.Limits.MaxNodes}
	}
	result := PublishResult{Spec: compiled.Spec, Diagnostics: diagnostics}
	if p.DryRun {
		result.Workflow = gen.Workflow{Name: compiled.Spec.Name}
		return result, nil
	}
	hash, err := compiled.Spec.Hash()
	if err != nil {
		return PublishResult{}, err
	}
	specJSON, err := json.Marshal(compiled.Spec)
	if err != nil {
		return PublishResult{}, fmt.Errorf("encode spec: %w", err)
	}
	name := compiled.Spec.Name

	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if err := q.EnsureWorkflow(ctx, gen.EnsureWorkflowParams{
			ID: uid.New(), TenantID: p.TenantID, Name: name, Description: optionalText(compiled.Spec.Description),
		}); err != nil {
			return fmt.Errorf("create workflow: %w", err)
		}
		// The row lock serialises concurrent publishes of this workflow.
		workflow, err := q.LockWorkflowByName(ctx, gen.LockWorkflowByNameParams{TenantID: p.TenantID, Name: name})
		if err != nil {
			return fmt.Errorf("lock workflow: %w", err)
		}
		if workflow.ArchivedAt.Valid {
			return ErrArchived
		}
		latest, err := q.LatestWorkflowVersion(ctx, workflow.ID)
		if err == nil && latest.SpecHash == hash {
			result.Workflow, result.Version, result.Unchanged = workflow, latest, true
			return nil
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("load latest version: %w", err)
		}
		// A draft-only workflow already has a row, but its first publication
		// adds a workflow to the plan count just like a brand-new workflow.
		if errors.Is(err, pgx.ErrNoRows) && p.Limits.MaxWorkflows > 0 {
			count, err := q.CountActiveWorkflows(ctx, p.TenantID)
			if err != nil {
				return fmt.Errorf("count workflows: %w", err)
			}
			if count >= p.Limits.MaxWorkflows {
				return &LimitError{Limit: "workflows", Allowed: p.Limits.MaxWorkflows}
			}
		}
		version, err := q.InsertWorkflowVersion(ctx, gen.InsertWorkflowVersionParams{
			ID: uid.New(), WorkflowID: workflow.ID, YamlSource: string(p.Source),
			Spec: specJSON, SpecHash: hash, CreatedBy: optionalText(p.CreatedBy),
		})
		if err != nil {
			return fmt.Errorf("insert workflow version: %w", err)
		}
		if err := q.TouchWorkflow(ctx, gen.TouchWorkflowParams{ID: workflow.ID, Description: optionalText(compiled.Spec.Description)}); err != nil {
			return fmt.Errorf("update workflow: %w", err)
		}
		result.Workflow, result.Version = workflow, version
		return nil
	})
	if err != nil {
		return PublishResult{}, err
	}
	return result, nil
}

func hasErrors(diagnostics []spec.Diagnostic) bool {
	for _, d := range diagnostics {
		if d.Severity == "error" {
			return true
		}
	}
	return false
}

func optionalText(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: value != ""}
}

// TestVersion is the draft version a test run executes.
type TestVersion struct {
	Workflow    gen.Workflow
	Version     gen.WorkflowVersion
	Spec        spec.Spec
	Diagnostics []spec.Diagnostic
}

// PrepareTest compiles an unpublished draft and stores it as a test version
// (numbered below zero) that runs can pin, without publishing it. An identical
// draft reuses its earlier test version. A workflow that has never been
// published gets its row here but stays out of listings and plan counts until
// its first publish.
func (s *Service) PrepareTest(ctx context.Context, tenantID pgtype.UUID, source []byte, createdBy string) (TestVersion, error) {
	compiled, diagnostics := spec.CompileWith(source, spec.Options{Registry: s.registry})
	if compiled == nil || hasErrors(diagnostics) {
		return TestVersion{}, &CompileError{Diagnostics: diagnostics}
	}
	if s.Plans != nil {
		limits, err := s.Plans.PublishLimits(ctx, tenantID)
		if err != nil {
			return TestVersion{}, err
		}
		if limits.MaxNodes > 0 && int64(len(compiled.Spec.Nodes)) > limits.MaxNodes {
			return TestVersion{}, &LimitError{Limit: "steps per workflow", Allowed: limits.MaxNodes}
		}
	}
	hash, err := compiled.Spec.Hash()
	if err != nil {
		return TestVersion{}, err
	}
	specJSON, err := json.Marshal(compiled.Spec)
	if err != nil {
		return TestVersion{}, fmt.Errorf("encode spec: %w", err)
	}
	result := TestVersion{Spec: compiled.Spec, Diagnostics: diagnostics}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if err := q.EnsureWorkflow(ctx, gen.EnsureWorkflowParams{
			ID: uid.New(), TenantID: tenantID, Name: compiled.Spec.Name, Description: optionalText(compiled.Spec.Description),
		}); err != nil {
			return fmt.Errorf("create workflow: %w", err)
		}
		workflow, err := q.LockWorkflowByName(ctx, gen.LockWorkflowByNameParams{TenantID: tenantID, Name: compiled.Spec.Name})
		if err != nil {
			return fmt.Errorf("lock workflow: %w", err)
		}
		if workflow.ArchivedAt.Valid {
			return ErrArchived
		}
		result.Workflow = workflow
		existing, err := q.FindTestVersion(ctx, gen.FindTestVersionParams{WorkflowID: workflow.ID, SpecHash: hash})
		if err == nil {
			result.Version = existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("find test version: %w", err)
		}
		result.Version, err = q.InsertTestVersion(ctx, gen.InsertTestVersionParams{
			ID: uid.New(), WorkflowID: workflow.ID, YamlSource: string(source),
			Spec: specJSON, SpecHash: hash, CreatedBy: optionalText(createdBy),
		})
		if err != nil {
			return fmt.Errorf("insert test version: %w", err)
		}
		return nil
	})
	if err != nil {
		return TestVersion{}, err
	}
	return result, nil
}
