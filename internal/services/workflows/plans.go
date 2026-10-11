package workflows

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
)

// Plans supplies per-organization plan allowances (M11). A nil Plans on the
// Service means unlimited.
type Plans interface {
	PublishLimits(ctx context.Context, tenantID pgtype.UUID) (Limits, error)
	// CheckRun returns a *LimitError when the monthly run allowance is used.
	CheckRun(ctx context.Context, tenantID pgtype.UUID) error
}
