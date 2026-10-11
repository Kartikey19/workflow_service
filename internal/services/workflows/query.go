package workflows

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/exception"
)

// Detail is a workflow with its latest version (nil before the first publish)
// and the summary of every version.
type Detail struct {
	Workflow gen.Workflow
	Latest   *gen.WorkflowVersion
	Versions []gen.ListWorkflowVersionsRow
}

// VersionDetail is one immutable version and the workflow it belongs to.
type VersionDetail struct {
	Workflow gen.Workflow
	Version  gen.WorkflowVersion
}

func notFound(name string) error {
	return exception.NotFound("workflow " + strconv.Quote(name))
}

// List returns the organization's workflows with run statistics.
func (s *Service) List(ctx context.Context, tenantID pgtype.UUID, includeArchived bool) ([]gen.ListWorkflowsRow, error) {
	rows, err := s.q.ListWorkflows(ctx, gen.ListWorkflowsParams{TenantID: tenantID, IncludeArchived: includeArchived})
	if err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	return rows, nil
}

func (s *Service) byName(ctx context.Context, tenantID pgtype.UUID, name string) (gen.Workflow, error) {
	workflow, err := s.q.GetWorkflowByName(ctx, gen.GetWorkflowByNameParams{TenantID: tenantID, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.Workflow{}, notFound(name)
	}
	if err != nil {
		return gen.Workflow{}, fmt.Errorf("get workflow: %w", err)
	}
	return workflow, nil
}

// Get returns the workflow, its latest version and the version list.
func (s *Service) Get(ctx context.Context, tenantID pgtype.UUID, name string) (Detail, error) {
	workflow, err := s.byName(ctx, tenantID, name)
	if err != nil {
		return Detail{}, err
	}
	detail := Detail{Workflow: workflow}
	latest, err := s.q.LatestWorkflowVersion(ctx, workflow.ID)
	switch {
	case err == nil:
		detail.Latest = &latest
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return Detail{}, fmt.Errorf("get latest version: %w", err)
	}
	if detail.Versions, err = s.q.ListWorkflowVersions(ctx, workflow.ID); err != nil {
		return Detail{}, fmt.Errorf("list versions: %w", err)
	}
	return detail, nil
}

// GetVersion returns one version of the named workflow.
func (s *Service) GetVersion(ctx context.Context, tenantID pgtype.UUID, name string, number int32) (VersionDetail, error) {
	workflow, err := s.byName(ctx, tenantID, name)
	if err != nil {
		return VersionDetail{}, err
	}
	version, err := s.q.GetWorkflowVersionByNumber(ctx, gen.GetWorkflowVersionByNumberParams{WorkflowID: workflow.ID, Version: number})
	if errors.Is(err, pgx.ErrNoRows) {
		return VersionDetail{}, exception.NotFound("version " + strconv.Itoa(int(number)))
	}
	if err != nil {
		return VersionDetail{}, fmt.Errorf("get version: %w", err)
	}
	return VersionDetail{Workflow: workflow, Version: version}, nil
}

// SetArchived archives or restores a workflow: new runs are rejected while it
// is archived, existing runs continue, versions are never deleted.
func (s *Service) SetArchived(ctx context.Context, tenantID pgtype.UUID, name string, archived bool) error {
	rows, err := s.q.SetWorkflowArchived(ctx, gen.SetWorkflowArchivedParams{TenantID: tenantID, Name: name, Archived: archived})
	if err != nil {
		return fmt.Errorf("set workflow archived: %w", err)
	}
	if rows == 0 {
		return notFound(name)
	}
	return nil
}
