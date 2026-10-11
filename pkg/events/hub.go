// Package events fans PostgreSQL run-event notifications out to in-process
// subscribers such as server-sent-event streams.
package events

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// Hub shares one LISTEN wf_events connection among all SSE streams, so a
// stream costs a goroutine and not a pooled database connection.
type Hub struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
}

// NewHub returns an empty hub; call Run to start listening.
func NewHub() *Hub {
	return &Hub{subs: make(map[string]map[chan struct{}]struct{})}
}

// Subscribe wakes the returned channel when runID has new events. The returned
// function unsubscribes.
func (h *Hub) Subscribe(runID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.subs[runID] == nil {
		h.subs[runID] = make(map[chan struct{}]struct{})
	}
	h.subs[runID][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs[runID], ch)
		if len(h.subs[runID]) == 0 {
			delete(h.subs, runID)
		}
		h.mu.Unlock()
	}
}

func (h *Hub) publish(runID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[runID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Run listens until ctx ends, reconnecting after failures.
func (h *Hub) Run(ctx context.Context, pool *pgxpool.Pool, log *zap.Logger) {
	for ctx.Err() == nil {
		err := func() error {
			conn, err := pool.Acquire(ctx)
			if err != nil {
				return err
			}
			defer conn.Release()
			if _, err := conn.Exec(ctx, "listen wf_events"); err != nil {
				return err
			}
			defer func() {
				cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_, _ = conn.Exec(cleanup, "unlisten *")
			}()
			for {
				notification, err := conn.Conn().WaitForNotification(ctx)
				if err != nil {
					return err
				}
				h.publish(notification.Payload)
			}
		}()
		if err != nil && ctx.Err() == nil {
			log.Warn("event listener disconnected; streams keep polling", zap.Error(err))
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}
