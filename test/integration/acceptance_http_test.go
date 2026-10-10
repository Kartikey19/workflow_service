//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// upstream records requests and answers from a per-path script.
type upstream struct {
	mu    sync.Mutex
	keys  []string
	calls atomic.Int32
}

func (h *harness) upstream(handler func(call int32, w http.ResponseWriter, r *http.Request)) (*upstream, string) {
	u := &upstream{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.keys = append(u.keys, r.Header.Get("Idempotency-Key"))
		u.mu.Unlock()
		handler(u.calls.Add(1), w, r)
	}))
	h.t.Cleanup(server.Close)
	return u, server.URL
}

func (h *harness) attempts(runID any, nodeID string) []map[string]any {
	h.t.Helper()
	rows, err := h.pool.Query(context.Background(), `
		select attempt, status, coalesce(error->>'code', ''), idempotency_key, run_after, created_at, octet_length(coalesce(output::text, ''))
		  from node_executions where run_id = $1 and node_id = $2 order by attempt`, runID, nodeID)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var attempt, outputBytes int
		var status, code, key string
		var runAfter, created time.Time
		if err := rows.Scan(&attempt, &status, &code, &key, &runAfter, &created, &outputBytes); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, map[string]any{"attempt": attempt, "status": status, "code": code, "key": key,
			"delay": runAfter.Sub(created), "output_bytes": outputBytes})
	}
	return out
}

func httpWorkflow(name, url, retry string) string {
	return `
apiVersion: workflow/v1
name: ` + name + `
nodes:
  - id: call
    type: http
    timeout: 5s
    retry: ` + retry + `
    config: {method: POST, url: "` + url + `", body: {charge: 100}}
`
}

// M3: 503 is retried with the same key until success; 400 is permanent and
// creates no retry rows.
func TestHTTPRetryClassificationAndStableKeys(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	flaky, flakyURL := h.upstream(func(call int32, w http.ResponseWriter, _ *http.Request) {
		if call <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"charged":true}`))
	})
	_, badURL := h.upstream(func(_ int32, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadRequest) })
	h.mustPublish(httpWorkflow("flaky-charge", flakyURL, "{max_attempts: 5, backoff: fixed, initial: 10ms, max: 10ms}"))
	h.mustPublish(httpWorkflow("bad-charge", badURL, "{max_attempts: 5, backoff: fixed, initial: 10ms, max: 10ms}"))
	h.startWorkers(2)

	ok := h.start("flaky-charge", nil)
	if status := h.waitStatus(ok, 10*time.Second, "succeeded", "failed"); status != "succeeded" {
		t.Fatalf("flaky run = %s", status)
	}
	rows := h.attempts(ok, "call")
	if len(rows) != 3 || rows[0]["code"] != "upstream_5xx" || rows[1]["code"] != "upstream_5xx" || rows[2]["status"] != "succeeded" {
		t.Fatalf("attempts = %v", rows)
	}
	// I5 at the wire: every attempt sent a byte-identical Idempotency-Key.
	flaky.mu.Lock()
	keys := append([]string(nil), flaky.keys...)
	flaky.mu.Unlock()
	if len(keys) != 3 || keys[0] == "" || keys[0] != keys[1] || keys[1] != keys[2] || keys[0] != rows[0]["key"] {
		t.Fatalf("Idempotency-Key headers = %q, stored key %v", keys, rows[0]["key"])
	}
	h.checkInvariants(ok)

	bad := h.start("bad-charge", nil)
	if status := h.waitStatus(bad, 10*time.Second, "succeeded", "failed"); status != "failed" {
		t.Fatalf("400 run = %s", status)
	}
	if rows := h.attempts(bad, "call"); len(rows) != 1 || rows[0]["code"] != "upstream_4xx" {
		t.Fatalf("400 attempts = %v, want exactly one permanent failure", rows)
	}
	h.checkInvariants(bad)
}

// M3: Retry-After on a 429 overrides a much shorter computed backoff.
func TestHTTPRetryAfterOverridesBackoff(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	_, url := h.upstream(func(_ int32, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	h.mustPublish(httpWorkflow("rate-limited", url, "{max_attempts: 3, backoff: fixed, initial: 10ms, max: 10ms}"))
	h.startWorkers(1)
	runID := h.start("rate-limited", nil)
	deadline := time.Now().Add(10 * time.Second)
	for len(h.attempts(runID, "call")) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("retry was never scheduled: %v", h.attempts(runID, "call"))
		}
		time.Sleep(20 * time.Millisecond)
	}
	rows := h.attempts(runID, "call")
	if rows[0]["code"] != "rate_limited" || rows[1]["status"] != "ready" {
		t.Fatalf("attempts = %v", rows)
	}
	if delay := rows[1]["delay"].(time.Duration); delay < 29*time.Second || delay > 31*time.Second {
		t.Fatalf("retry scheduled %v after its creation, want ~30s from Retry-After", delay)
	}
	h.checkInvariants(runID)
}

// M3: max_attempts exhausted fails the node and the run, with no extra rows.
func TestHTTPMaxAttemptsExhausted(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	_, url := h.upstream(func(_ int32, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	h.mustPublish(httpWorkflow("always-down", url, "{max_attempts: 3, backoff: exponential, initial: 10ms, max: 40ms}"))
	h.startWorkers(2)
	runID := h.start("always-down", nil)
	if status := h.waitStatus(runID, 10*time.Second, "succeeded", "failed"); status != "failed" {
		t.Fatalf("run = %s", status)
	}
	time.Sleep(200 * time.Millisecond) // a stray retry row would appear by now
	rows := h.attempts(runID, "call")
	if len(rows) != 3 {
		t.Fatalf("attempts = %v, want exactly 3", rows)
	}
	for _, row := range rows {
		if row["status"] != "failed" || row["code"] != "upstream_5xx" {
			t.Fatalf("attempts = %v", rows)
		}
	}
	_, runErr := h.runOutput(runID)
	if runErr["node_id"] != "call" || runErr["node_code"] != "upstream_5xx" {
		t.Fatalf("run error = %v", runErr)
	}
	h.checkInvariants(runID)
}

// M3: an oversized response fails with a typed error and stores no giant row.
func TestHTTPOversizedResponseFailsCleanly(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	_, url := h.upstream(func(_ int32, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`"` + strings.Repeat("x", 300<<10) + `"`))
	})
	h.mustPublish(httpWorkflow("huge", url, "{max_attempts: 3}"))
	h.startWorkers(1)
	runID := h.start("huge", nil)
	if status := h.waitStatus(runID, 10*time.Second, "succeeded", "failed"); status != "failed" {
		t.Fatalf("run = %s", status)
	}
	rows := h.attempts(runID, "call")
	if len(rows) != 1 || rows[0]["code"] != "response_too_large" || rows[0]["output_bytes"].(int) > 1024 {
		t.Fatalf("attempts = %v, want one permanent response_too_large with no stored body", rows)
	}
	h.checkInvariants(runID)
}
