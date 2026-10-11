package api

import (
	"encoding/json"
	"fmt"
	"github.com/kartikey/workflow_service/pkg/httpx"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kartikey/workflow_service/internal/services/runs"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/auth"
	"github.com/kartikey/workflow_service/pkg/exception"
	"github.com/kartikey/workflow_service/pkg/uid"
	"go.uber.org/zap"
)

// ssePoll is the follow fallback. NOTIFY only shortens the wait: the poll is
// the correctness guarantee, as in the claim loop (04 §3.5).
const ssePoll = 2 * time.Second

// runEvents is GET /be/v1/runs/{id}/events (04 §3.5). seq is gap-free (I6), so a
// consumer polling ?after=<last seq> misses nothing. With Accept:
// text/event-stream it replays everything after `after`, then follows until
// the run is terminal and fully delivered.
func (h *Handler) runEvents(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	row, err := h.loadRun(r, p)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	after := int64(0)
	if text := r.URL.Query().Get("after"); text != "" {
		if after, err = strconv.ParseInt(text, 10, 32); err != nil || after < 0 {
			h.writeError(w, r, exception.BadRequest("after must be a non-negative sequence number"))
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
		h.writeError(w, r, err)
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		h.streamEvents(w, r, row.Run, int32(after))
		return
	}
	events, err := h.services.Runs.Events(r.Context(), p.TenantID, row.Run.ID, int32(after), int32(limit))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(events))
	for _, e := range events {
		items = append(items, eventView(e))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "run_status": row.Run.Status})
}

func (h *Handler) streamEvents(w http.ResponseWriter, r *http.Request, run gen.Run, after int32) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		h.writeError(w, r, exception.BadRequest("streaming is not supported by this connection"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	wake, unsubscribe := h.events.Subscribe(uid.String(run.ID))
	defer unsubscribe()
	ticker := time.NewTicker(ssePoll)
	defer ticker.Stop()
	svc := h.services.Runs
	for {
		events, err := svc.Events(r.Context(), run.TenantID, run.ID, after, 500)
		if err != nil {
			if r.Context().Err() == nil {
				h.logger.Warn("stream run events", zap.Error(err))
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
			status, err := svc.Status(r.Context(), run.TenantID, run.ID)
			if err != nil {
				return
			}
			if runs.IsTerminalRun(status) {
				// Terminal events were written in the same transaction as the
				// status, so one more read cannot miss any.
				more, err := svc.Events(r.Context(), run.TenantID, run.ID, after, 1)
				if err == nil && len(more) == 0 {
					_, _ = fmt.Fprintf(w, "event: end\ndata: {\"status\":%q}\n\n", status)
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
		case <-h.closing:
			return
		case <-wake:
		case <-ticker.C:
			_, _ = fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		}
	}
}
