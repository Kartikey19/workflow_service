package engine

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/kartikey/workflow_service/internal/flow"
	"github.com/kartikey/workflow_service/internal/obs"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"go.uber.org/zap"
)

// sweeper runs on every worker; each pass is safe to run concurrently (03 §8).
type sweeper struct {
	store    *gen.Queries
	flow     *flow.Flow
	log      *zap.Logger
	interval time.Duration
}

func (s *sweeper) Run(ctx context.Context) {
	for {
		s.pass(ctx)
		// Jitter keeps sweepers on many workers from aligning.
		delay := s.interval + time.Duration(rand.Int64N(int64(s.interval/5)+1))
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func (s *sweeper) pass(ctx context.Context) {
	// Bound each pass so a slow database cannot wedge the sweeper.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// 1. Lease expiry is re-delivery, not a retry: same row, same attempt.
	reclaimed, err := s.store.ReclaimExpiredLeases(ctx)
	if err != nil && ctx.Err() == nil {
		s.log.Error("reclaim expired node leases", zap.Error(err))
	} else if len(reclaimed) > 0 {
		obs.LeaseExpired.Add(float64(len(reclaimed)))
		s.log.Warn("reclaimed expired node leases", zap.Int("count", len(reclaimed)))
	}
	// 2. Wake elapsed sleeps and time out waits.
	if woken, err := s.flow.WakeDue(ctx); err != nil && ctx.Err() == nil {
		s.log.Error("wake due waiting nodes", zap.Error(err))
	} else if woken > 0 {
		s.log.Info("woke waiting nodes", zap.Int("count", woken))
	}
	// 3. Fail runs past their deadline.
	if failed, err := s.flow.FailPastDeadline(ctx); err != nil && ctx.Err() == nil {
		s.log.Error("fail runs past deadline", zap.Error(err))
	} else if failed > 0 {
		s.log.Info("failed runs past deadline", zap.Int("count", failed))
	}
}
