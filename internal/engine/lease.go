package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"go.uber.org/zap"
)

var errLeaseLost = errors.New("node execution lease lost")

func (d *Dispatcher) startHeartbeat(parent context.Context, ne gen.NodeExecution) (context.Context, func(), error) {
	interval := d.cfg.LeaseTTL / 3
	if d.cfg.ID == "" || interval < time.Microsecond {
		return parent, nil, errors.New("heartbeat requires a worker ID and lease TTL of at least 3 microseconds")
	}
	ctx, cancel := context.WithCancel(parent)
	if err := d.extendLease(ctx, ne, interval); err != nil {
		cancel()
		return parent, nil, err
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := d.extendLease(ctx, ne, interval); err != nil {
					d.logLeaseError(ctx, ne, err)
					return
				}
			}
		}
	}()

	return ctx, func() {
		cancel()
		<-done
	}, nil
}

func (d *Dispatcher) extendLease(ctx context.Context, ne gen.NodeExecution, timeout time.Duration) error {
	queryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	rows, err := d.store.ExtendLease(queryCtx, gen.ExtendLeaseParams{
		ID:       ne.ID,
		WorkerID: pgtype.Text{String: d.cfg.ID, Valid: true},
		LeaseTtl: pgtype.Interval{Microseconds: d.cfg.LeaseTTL.Microseconds(), Valid: true},
	})
	if err != nil {
		return fmt.Errorf("extend node execution lease: %w", err)
	}
	if rows == 0 {
		return errLeaseLost
	}
	return nil
}

func (d *Dispatcher) logLeaseError(ctx context.Context, ne gen.NodeExecution, err error) {
	if ctx.Err() != nil {
		return
	}
	fields := []zap.Field{
		zap.Error(err),
		zap.Any("node_execution_id", ne.ID),
		zap.String("worker_id", d.cfg.ID),
	}
	if errors.Is(err, errLeaseLost) {
		d.logger.Warn("discard stale node execution", fields...)
		return
	}
	d.logger.Error("node execution heartbeat failed", fields...)
}
