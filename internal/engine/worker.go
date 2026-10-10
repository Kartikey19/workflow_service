package engine

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kartikey/workflow_service/internal/flow"
	"github.com/kartikey/workflow_service/internal/obs"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/config"
	"go.uber.org/zap"
)

// Worker owns the claim loop, the ready listener and the sweeper (03 §1).
// Workers are interchangeable: run as many replicas as needed against one DB.
type Worker struct {
	cfg      config.WorkerConfig
	log      *zap.Logger
	pool     *pgxpool.Pool
	store    *gen.Queries
	claim    *claimLoop
	sweeper  *sweeper
	listener *readyListener
}

func NewWorker(cfg config.WorkerConfig, log *zap.Logger, pool *pgxpool.Pool, f *flow.Flow, dispatch func(context.Context, gen.NodeExecution)) *Worker {
	if log == nil {
		log = zap.NewNop()
	}
	wake := make(chan struct{}, 1)
	sweepInterval := cfg.SweepInterval
	if sweepInterval <= 0 {
		sweepInterval = 5 * time.Second
	}
	store := gen.New(pool)
	return &Worker{
		cfg: cfg, log: log, pool: pool, store: store,
		claim: &claimLoop{
			store: store, cfg: cfg, log: log, dispatch: dispatch,
			slots: make(chan struct{}, cfg.Concurrency), wake: wake,
		},
		sweeper:  &sweeper{store: store, flow: f, log: log, interval: sweepInterval},
		listener: &readyListener{pool: pool, log: log, wake: wake},
	}
}

// Run claims and executes nodes until ctx is cancelled, then stops claiming,
// waits up to grace for in-flight nodes, cancels the rest and hands their
// leases back so another worker continues them immediately (03 §9.1).
func (w *Worker) Run(ctx context.Context, grace time.Duration) error {
	executionCtx, cancelExecution := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelExecution()

	var background sync.WaitGroup
	background.Go(func() { w.sweeper.Run(ctx) })
	background.Go(func() { w.listener.Run(ctx) })

	w.claim.Run(ctx, executionCtx)
	background.Wait()

	drained := make(chan struct{})
	go func() {
		w.claim.inFlight.Wait()
		close(drained)
	}()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	var err error
	select {
	case <-drained:
	case <-timer.C:
		w.log.Warn("shutdown grace elapsed; cancelling in-flight nodes")
		cancelExecution()
		<-drained
		err = errors.New("in-flight nodes were cancelled at shutdown")
	}

	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	released, releaseErr := w.store.ReleaseWorkerLeases(releaseCtx, pgtype.Text{String: w.cfg.ID, Valid: true})
	if releaseErr != nil {
		w.log.Error("release leases at shutdown", zap.Error(releaseErr))
	} else if len(released) > 0 {
		w.log.Info("released leases at shutdown", zap.Int("count", len(released)))
		obs.SweeperActions.WithLabelValues("lease_released").Add(float64(len(released)))
	}
	return err
}

// readyListener turns pg_notify('wf_ready') into a claim-loop wakeup. NOTIFY is
// an optimisation only: it is dropped on connection loss, so polling stays.
type readyListener struct {
	pool *pgxpool.Pool
	log  *zap.Logger
	wake chan<- struct{}
}

func (l *readyListener) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := l.listen(ctx); err != nil && ctx.Err() == nil {
			l.log.Warn("ready listener disconnected; polling continues", zap.Error(err))
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (l *readyListener) listen(ctx context.Context) error {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "listen wf_ready"); err != nil {
		return err
	}
	defer func() {
		// Do not return a LISTENing connection to the pool.
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = conn.Exec(cleanup, "unlisten *")
	}()
	for {
		if _, err := conn.Conn().WaitForNotification(ctx); err != nil {
			return err
		}
		select {
		case l.wake <- struct{}{}:
		default:
		}
	}
}
