// Package runs is the run service: it owns run state transitions shared by the
// API and the workers (starting runs,
// advancing the DAG, completing runs, cancellation, signals) and the run
// use-cases built on them (plan allowance, reads, retry).
// Every transition runs under the run row lock (03 §5.4).
package runs

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/core/expr"
	"github.com/kartikey/workflow_service/internal/spec"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/cache"
)

// ErrSpecInvalid marks a stored spec that cannot be loaded: deterministic, so
// callers fail permanently instead of re-delivering forever.
var ErrSpecInvalid = errors.New("published spec is invalid")

// SpecCache holds compiled specs by workflow_version_id. Versions are immutable
// (I7), so entries never need invalidation; the LRU bound only caps memory.
type SpecCache struct {
	env   *expr.Env
	cache *cache.LRU[[16]byte, *spec.Compiled]
}

func NewSpecCache(env *expr.Env, capacity int) *SpecCache {
	if capacity <= 0 {
		capacity = 512
	}
	return &SpecCache{env: env, cache: cache.NewLRU[[16]byte, *spec.Compiled](capacity)}
}

// Get returns the compiled spec, loading it with q on a miss. tenantID scopes
// the lookup so one tenant can never load another's version.
func (c *SpecCache) Get(ctx context.Context, q *gen.Queries, versionID, tenantID pgtype.UUID) (*spec.Compiled, error) {
	if compiled, ok := c.cache.Get(versionID.Bytes); ok {
		return compiled, nil
	}
	version, err := q.GetWorkflowVersion(ctx, gen.GetWorkflowVersionParams{ID: versionID, TenantID: tenantID})
	if err != nil {
		return nil, fmt.Errorf("load workflow version: %w", err)
	}
	compiled, err := spec.Load(version.Spec, c.env)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSpecInvalid, err)
	}
	c.cache.Add(versionID.Bytes, compiled)
	return compiled, nil
}
