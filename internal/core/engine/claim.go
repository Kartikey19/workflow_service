package engine

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/obs"
	"go.uber.org/zap"
)

// claimLoop claims only what it has capacity to run (03 §9): leases start
// ticking at claim time, so idle claimed rows would waste lease TTL.
type claimLoop struct {
	store    *gen.Queries
	cfg      Config
	log      *zap.Logger
	dispatch func(context.Context, gen.NodeExecution)
	wake     <-chan struct{}

	slots    chan struct{}
	inFlight sync.WaitGroup
}

// Run claims until claimCtx ends. Dispatches run on executionCtx, which outlives
// claimCtx so in-flight nodes can finish during graceful shutdown.
func (c *claimLoop) Run(claimCtx, executionCtx context.Context) {
	ticker := time.NewTicker(c.cfg.PollInterval)
	defer ticker.Stop()
	for claimCtx.Err() == nil {
		if free := cap(c.slots) - len(c.slots); free > 0 {
			limit := min(free, c.cfg.BatchSize)
			nodes, err := c.store.ClaimReady(claimCtx, gen.ClaimReadyParams{
				WorkerID:  pgtype.Text{String: c.cfg.ID, Valid: true},
				LeaseTtl:  pgtype.Interval{Microseconds: c.cfg.LeaseTTL.Microseconds(), Valid: true},
				BatchSize: int32(limit),
			})
			if err != nil {
				if claimCtx.Err() != nil {
					return
				}
				c.log.Error("claim ready nodes", zap.Error(err))
			}
			obs.ClaimBatch.Observe(float64(len(nodes)))
			for _, node := range nodes {
				c.slots <- struct{}{}
				c.inFlight.Go(func() {
					defer func() { <-c.slots }()
					c.dispatch(executionCtx, node)
				})
			}
			if len(nodes) == limit {
				continue // a full batch suggests more work: skip the sleep
			}
		}
		select {
		case <-claimCtx.Done():
			return
		case <-ticker.C:
		case <-c.wake:
		}
	}
}
