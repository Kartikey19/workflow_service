package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/services/runs"
	"github.com/kartikey/workflow_service/internal/services/workflows"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/exception"
)

// Plans enforces plan limits. It implements workflows.Plans (publish limits)
// and runs.Allowance (monthly runs). A plan column that is null is unlimited.
type Plans struct{ s *Service }

var (
	_ workflows.Plans = Plans{}
	_ runs.Allowance  = Plans{}
)

// Plans returns the limit enforcer backed by this service.
func (s *Service) Plans() Plans { return Plans{s} }

func unlimited(v pgtype.Int8) int64 {
	if !v.Valid {
		return 0 // the engine reads 0 as "no limit"
	}
	return v.Int64
}

// PublishLimits returns the organization's workflow and step allowances.
func (p Plans) PublishLimits(ctx context.Context, tenantID pgtype.UUID) (workflows.Limits, error) {
	plan, err := p.s.q.GetOrgPlan(ctx, tenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return workflows.Limits{}, nil // not an identity-managed organization
	}
	if err != nil {
		return workflows.Limits{}, fmt.Errorf("load plan: %w", err)
	}
	return workflows.Limits{MaxWorkflows: unlimited(plan.MaxWorkflows), MaxNodes: unlimited(plan.MaxSteps)}, nil
}

// CheckRun counts one run against this month's allowance and refuses it once
// the allowance is used up. The count is atomic, so concurrent starts cannot
// overshoot the limit. An idempotent replay is never charged (runs.Service
// skips this check for it); a start that fails after being counted is.
func (p Plans) CheckRun(ctx context.Context, tenantID pgtype.UUID) error {
	plan, err := p.s.q.GetOrgPlan(ctx, tenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load plan: %w", err)
	}
	max := unlimited(plan.MaxRunsPerMonth)
	_, err = p.s.q.ReserveRun(ctx, gen.ReserveRunParams{OrgID: tenantID, MaxRuns: max})
	if errors.Is(err, pgx.ErrNoRows) {
		return &workflows.LimitError{Limit: "runs per month on the " + plan.Name + " plan", Allowed: max}
	}
	if err != nil {
		return fmt.Errorf("reserve run: %w", err)
	}
	return nil
}

// PlanInfo is a plan's limits; a nil limit is unlimited.
type PlanInfo struct {
	ID, Name        string
	MaxWorkflows    *int64
	MaxSteps        *int64
	MaxRunsPerMonth *int64
}

func planInfo(p gen.Plan) PlanInfo {
	ptr := func(v pgtype.Int8) *int64 {
		if !v.Valid {
			return nil
		}
		return &v.Int64
	}
	return PlanInfo{ID: p.ID, Name: p.Name, MaxWorkflows: ptr(p.MaxWorkflows), MaxSteps: ptr(p.MaxSteps), MaxRunsPerMonth: ptr(p.MaxRunsPerMonth)}
}

// Usage is an organization's plan and what it has used so far.
type Usage struct {
	Plan          PlanInfo
	Workflows     int64
	RunsThisMonth int64
}

// Usage reports the plan and current consumption.
func (s *Service) Usage(ctx context.Context, orgID pgtype.UUID) (Usage, error) {
	plan, err := s.q.GetOrgPlan(ctx, orgID)
	if err != nil {
		return Usage{}, fmt.Errorf("load plan: %w", err)
	}
	count, err := s.q.CountActiveWorkflows(ctx, orgID)
	if err != nil {
		return Usage{}, fmt.Errorf("count workflows: %w", err)
	}
	runsThisMonth, err := s.q.RunsThisMonth(ctx, orgID)
	if err != nil {
		return Usage{}, fmt.Errorf("count runs: %w", err)
	}
	return Usage{Plan: planInfo(plan), Workflows: count, RunsThisMonth: runsThisMonth}, nil
}

// ListPlans returns every plan.
func (s *Service) ListPlans(ctx context.Context) ([]PlanInfo, error) {
	rows, err := s.q.ListPlans(ctx)
	if err != nil {
		return nil, fmt.Errorf("list plans: %w", err)
	}
	out := make([]PlanInfo, 0, len(rows))
	for _, row := range rows {
		out = append(out, planInfo(row))
	}
	return out, nil
}

// OrgSummary is a row of the platform admin's organization list.
type OrgSummary struct {
	ID            pgtype.UUID
	Name, Slug    string
	PlanID        string
	PlanName      string
	Members       int64
	RunsThisMonth int64
}

// ListOrganizations lists organizations for platform administrators.
func (s *Service) ListOrganizations(ctx context.Context, limit int) ([]OrgSummary, error) {
	rows, err := s.q.ListOrganizations(ctx, int32(limit))
	if err != nil {
		return nil, fmt.Errorf("list organizations: %w", err)
	}
	out := make([]OrgSummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, OrgSummary{ID: row.ID, Name: row.Name, Slug: row.Slug, PlanID: row.PlanID, PlanName: row.PlanName,
			Members: row.MemberCount, RunsThisMonth: row.RunsThisMonth})
	}
	return out, nil
}

// SetOrganizationPlan assigns a plan; billing is manual until self sign-up opens.
func (s *Service) SetOrganizationPlan(ctx context.Context, orgID pgtype.UUID, planID string) error {
	plans, err := s.q.ListPlans(ctx)
	if err != nil {
		return fmt.Errorf("list plans: %w", err)
	}
	known := false
	for _, plan := range plans {
		known = known || plan.ID == planID
	}
	if !known {
		return exception.BadRequest("unknown plan " + planID)
	}
	rows, err := s.q.SetOrganizationPlan(ctx, gen.SetOrganizationPlanParams{ID: orgID, PlanID: planID})
	if err != nil {
		return fmt.Errorf("set plan: %w", err)
	}
	if rows == 0 {
		return exception.NotFound("organization")
	}
	return nil
}
