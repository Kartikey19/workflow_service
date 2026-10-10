// Package flow owns run state transitions shared by the API and the workers:
// starting runs, advancing the DAG, completing runs, cancellation and signals.
// Every transition runs under the run row lock (03 §5.4).
package flow

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/expr"
	"github.com/kartikey/workflow_service/internal/spec"
	"github.com/kartikey/workflow_service/internal/store/gen"
)

// ErrSpecInvalid marks a stored spec that cannot be loaded: deterministic, so
// callers fail permanently instead of re-delivering forever.
var ErrSpecInvalid = errors.New("published spec is invalid")

// SpecCache holds compiled specs by workflow_version_id. Versions are immutable
// (I7), so entries never need invalidation; the LRU bound only caps memory.
type SpecCache struct {
	env      *expr.Env
	capacity int

	mu    sync.Mutex
	items map[[16]byte]*list.Element
	order *list.List
}

type cacheEntry struct {
	key      [16]byte
	compiled *spec.Compiled
}

func NewSpecCache(env *expr.Env, capacity int) *SpecCache {
	if capacity <= 0 {
		capacity = 512
	}
	return &SpecCache{env: env, capacity: capacity, items: make(map[[16]byte]*list.Element), order: list.New()}
}

// Get returns the compiled spec, loading it with q on a miss. tenantID scopes
// the lookup so one tenant can never load another's version.
func (c *SpecCache) Get(ctx context.Context, q *gen.Queries, versionID, tenantID pgtype.UUID) (*spec.Compiled, error) {
	c.mu.Lock()
	if element, ok := c.items[versionID.Bytes]; ok {
		c.order.MoveToFront(element)
		compiled := element.Value.(*cacheEntry).compiled
		c.mu.Unlock()
		return compiled, nil
	}
	c.mu.Unlock()

	version, err := q.GetWorkflowVersion(ctx, gen.GetWorkflowVersionParams{ID: versionID, TenantID: tenantID})
	if err != nil {
		return nil, fmt.Errorf("load workflow version: %w", err)
	}
	compiled, err := spec.Load(version.Spec, c.env)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSpecInvalid, err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.items[versionID.Bytes]; ok {
		return element.Value.(*cacheEntry).compiled, nil
	}
	c.items[versionID.Bytes] = c.order.PushFront(&cacheEntry{key: versionID.Bytes, compiled: compiled})
	for c.order.Len() > c.capacity {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*cacheEntry).key)
	}
	return compiled, nil
}
