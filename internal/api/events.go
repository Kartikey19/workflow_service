package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kartikey/workflow_service/internal/api/problem"
	"github.com/kartikey/workflow_service/internal/auth"
	"github.com/kartikey/workflow_service/internal/flow"
	"github.com/kartikey/workflow_service/internal/ids"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"go.uber.org/zap"
)

// ssePoll is the follow fallback. NOTIFY only shortens the wait: the poll is
// the correctness guarantee, as in the claim loop (04 §3.5).
const ssePoll = 2 * time.Second

func eventView(e gen.RunEvent) map[string]any {
	return map[string]any{
		"seq": e.Seq, "type": e.Type, "node_id": textJSON(e.NodeID), "attempt": attemptJSON(e),
		"payload": rawJSON(e.Payload), "created_at": timeJSON(e.CreatedAt),
	}
}

func attemptJSON(e gen.RunEvent) *int32 {
	if !e.Attempt.Valid {
		return nil
	}
	return &e.Attempt.Int32
}

// runEvents is GET /v1/runs/{id}/events (04 §3.5). seq is gap-free (I6), so a
// consumer polling ?after=<last seq> misses nothing. With Accept:
// text/event-stream it replays everything after `after`, then follows until
// the run is terminal and fully delivered.
func (s *Server) runEvents(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	row, err := s.loadRun(r, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	after := int64(0)
	if text := r.URL.Query().Get("after"); text != "" {
		if after, err = strconv.ParseInt(text, 10, 32); err != nil || after < 0 {
			s.writeError(w, r, problem.BadRequest("after must be a non-negative sequence number"))
			return
		}
	}
	if text := r.Header.Get("Last-Event-ID"); text != "" {
		if parsed, err := strconv.ParseInt(text, 10, 32); err == nil && parsed > after {
			after = parsed
		}
	}
	limit, err := queryLimit(r, 100, 1000)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.streamEvents(w, r, row.Run, int32(after))
		return
	}
	events, err := s.services.Flow.Q.ListRunEvents(r.Context(), gen.ListRunEventsParams{
		RunID: row.Run.ID, TenantID: p.TenantID, AfterSeq: int32(after), Lim: int32(limit),
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(events))
	for _, e := range events {
		items = append(items, eventView(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "run_status": row.Run.Status})
}

func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request, run gen.Run, after int32) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, r, problem.BadRequest("streaming is not supported by this connection"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	wake, unsubscribe := s.events.subscribe(ids.String(run.ID))
	defer unsubscribe()
	ticker := time.NewTicker(ssePoll)
	defer ticker.Stop()
	q := s.services.Flow.Q
	for {
		events, err := q.ListRunEvents(r.Context(), gen.ListRunEventsParams{
			RunID: run.ID, TenantID: run.TenantID, AfterSeq: after, Lim: 500,
		})
		if err != nil {
			if r.Context().Err() == nil {
				s.logger.Warn("stream run events", zap.Error(err))
			}
			return
		}
		for _, e := range events {
			data, _ := json.Marshal(eventView(e))
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Type, data); err != nil {
				return
			}
			after = e.Seq
		}
		if len(events) > 0 {
			flusher.Flush()
		}
		if len(events) < 500 {
			current, err := q.GetRun(r.Context(), gen.GetRunParams{ID: run.ID, TenantID: run.TenantID})
			if err != nil {
				return
			}
			if flow.IsTerminalRun(current.Status) {
				// Terminal events were written in the same transaction as the
				// status, so one more read cannot miss any.
				more, err := q.ListRunEvents(r.Context(), gen.ListRunEventsParams{RunID: run.ID, TenantID: run.TenantID, AfterSeq: after, Lim: 1})
				if err == nil && len(more) == 0 {
					_, _ = fmt.Fprintf(w, "event: end\ndata: {\"status\":%q}\n\n", current.Status)
					flusher.Flush()
					return
				}
				continue
			}
		} else {
			continue
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.closing:
			return
		case <-wake:
		case <-ticker.C:
			_, _ = fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		}
	}
}

// eventHub shares one LISTEN wf_events connection among all SSE streams, so a
// stream costs a goroutine and not a pooled database connection.
type eventHub struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
}

func newEventHub() *eventHub {
	return &eventHub{subs: make(map[string]map[chan struct{}]struct{})}
}

func (h *eventHub) subscribe(runID string) (<-chan struct{}, func()) {
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

func (h *eventHub) publish(runID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[runID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// run listens until ctx ends, reconnecting after failures.
func (h *eventHub) run(ctx context.Context, pool *pgxpool.Pool, log *zap.Logger) {
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
