//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// deliveryUpstream stands in for the inventory, dispatch and notify services of
// the delivery example (02 §5.1). It records every request body by path.
type deliveryUpstream struct {
	mu       sync.Mutex
	bodies   map[string][]map[string]any
	handlers map[string]func(w http.ResponseWriter)
}

func (d *deliveryUpstream) calls(path string) []map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]map[string]any(nil), d.bodies[path]...)
}

func newDeliveryUpstream(t *testing.T, handlers map[string]func(w http.ResponseWriter)) (*deliveryUpstream, string) {
	t.Helper()
	d := &deliveryUpstream{bodies: map[string][]map[string]any{}, handlers: handlers}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		d.mu.Lock()
		d.bodies[r.URL.Path] = append(d.bodies[r.URL.Path], body)
		handler := d.handlers[r.URL.Path]
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if handler != nil {
			handler(w)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(server.Close)
	return d, server.URL
}

// deliveryWorkflow is 02 §5.1 pointed at one test server, with the vault
// header dropped (the vault arrives in M12) and the long durations shortened.
func deliveryWorkflow(base, pickupTimeout string) string {
	return strings.NewReplacer("BASE", base, "PICKUP_TIMEOUT", pickupTimeout).Replace(`
apiVersion: workflow/v1
name: order-fulfilment
inputs:
  order_id: {type: string, required: true}
correlation: ${input.order_id}
timeout: 24h
defaults:
  retry: {max_attempts: 3, backoff: exponential, initial: 50ms, max: 200ms}
  timeout: 15s
nodes:
  - id: reserve_stock
    type: http
    config:
      method: POST
      url: "BASE/inv/reserve"
      body: {order_id: '${input.order_id}'}
  - id: assign_rider
    type: http
    needs: [reserve_stock]
    on_failure: release_stock
    config:
      method: POST
      url: "BASE/dispatch/assign"
      body: {reservation: '${nodes.reserve_stock.output.body.reservation_id}'}
  - id: await_pickup
    type: wait_event
    needs: [assign_rider]
    on_timeout: reassign_rider
    config:
      key: "order.picked_up:${input.order_id}"
      timeout: PICKUP_TIMEOUT
  - id: reassign_rider
    type: http
    needs: [await_pickup]
    join: any
    config:
      method: POST
      url: "BASE/dispatch/reassign"
      body: {order_id: '${input.order_id}'}
  - id: notify_customer
    type: http
    needs: [await_pickup]
    config:
      method: POST
      url: "BASE/notify/sms"
      body:
        to: ${input.order_id}
        text: "Your order is on the way with ${nodes.await_pickup.output.rider}."
  - id: release_stock
    type: http
    needs: [assign_rider]
    join: any
    config:
      method: POST
      url: "BASE/inv/release"
      body: {reservation: '${nodes.reserve_stock.output.body.reservation_id}'}
outputs:
  reservation: ${nodes.reserve_stock.output.body.reservation_id}
`)
}

func reserveOK(w http.ResponseWriter) { _, _ = io.WriteString(w, `{"reservation_id":"R-1"}`) }

// 02 §5.1 happy path against real HTTP upstreams: the signal carries the rider
// into the customer message; neither compensation nor reassignment runs.
func TestDeliveryExampleHappyPath(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	up, base := newDeliveryUpstream(t, map[string]func(http.ResponseWriter){"/inv/reserve": reserveOK})
	h.mustPublish(deliveryWorkflow(base, "1h"))
	h.startWorkers(2)
	runID := h.start("order-fulfilment", map[string]any{"order_id": "O-1"})
	waitNode(t, h, idString(runID), "await_pickup", "waiting")

	if _, err := h.flow.Signal(ctx, h.tenant, "order.picked_up:O-1", json.RawMessage(`{"rider":"Sam"}`)); err != nil {
		t.Fatal(err)
	}
	if status := h.waitStatus(runID, 15*time.Second, "succeeded", "failed"); status != "succeeded" {
		t.Fatalf("run = %s, nodes %v", status, h.nodeStatuses(runID))
	}
	got := h.nodeStatuses(runID)
	for node, want := range map[string]string{
		"reserve_stock": "succeeded", "assign_rider": "succeeded", "await_pickup": "succeeded",
		"notify_customer": "succeeded", "reassign_rider": "skipped", "release_stock": "skipped",
	} {
		if got[node] != want {
			t.Errorf("%s = %s, want %s (all: %v)", node, got[node], want, got)
		}
	}
	sms := up.calls("/notify/sms")
	if len(sms) != 1 || sms[0]["text"] != "Your order is on the way with Sam." {
		t.Fatalf("sms calls = %v", sms)
	}
	if len(up.calls("/inv/release")) != 0 || len(up.calls("/dispatch/reassign")) != 0 {
		t.Fatal("compensation or reassignment ran on the happy path")
	}
	if out, _ := h.runOutput(runID); out["reservation"] != "R-1" {
		t.Fatalf("outputs = %v", out)
	}
	h.checkInvariants(runID)
}

// 02 §5.1 timeout path: nobody picks the order up, so on_timeout hands it to
// the reassignment node and the customer is not notified.
func TestDeliveryExamplePickupTimeoutReassigns(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	up, base := newDeliveryUpstream(t, map[string]func(http.ResponseWriter){"/inv/reserve": reserveOK})
	h.mustPublish(deliveryWorkflow(base, "1s"))
	h.startWorkers(2)
	runID := h.start("order-fulfilment", map[string]any{"order_id": "O-2"})

	if status := h.waitStatus(runID, 20*time.Second, "succeeded", "failed"); status != "succeeded" {
		t.Fatalf("run = %s, nodes %v", status, h.nodeStatuses(runID))
	}
	got := h.nodeStatuses(runID)
	if got["await_pickup"] != "failed" || got["reassign_rider"] != "succeeded" ||
		got["notify_customer"] != "skipped" || got["release_stock"] != "skipped" {
		t.Fatalf("nodes = %v", got)
	}
	if len(up.calls("/dispatch/reassign")) != 1 || len(up.calls("/notify/sms")) != 0 {
		t.Fatalf("reassign %d times, sms %d times", len(up.calls("/dispatch/reassign")), len(up.calls("/notify/sms")))
	}
	h.checkInvariants(runID)
}

// 02 §5.1 compensation path: dispatch rejects the order permanently, so the
// reserved stock is released with the original reservation id and the pickup
// wait is never entered.
func TestDeliveryExampleAssignFailureReleasesStock(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	up, base := newDeliveryUpstream(t, map[string]func(http.ResponseWriter){
		"/inv/reserve": reserveOK,
		"/dispatch/assign": func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"no riders"}`)
		},
	})
	h.mustPublish(deliveryWorkflow(base, "1h"))
	h.startWorkers(2)
	runID := h.start("order-fulfilment", map[string]any{"order_id": "O-3"})

	if status := h.waitStatus(runID, 15*time.Second, "succeeded", "failed"); status != "succeeded" {
		t.Fatalf("run = %s, want succeeded because the failure was handled; nodes %v", status, h.nodeStatuses(runID))
	}
	got := h.nodeStatuses(runID)
	if got["assign_rider"] != "failed" || got["release_stock"] != "succeeded" ||
		got["await_pickup"] != "skipped" || got["notify_customer"] != "skipped" {
		t.Fatalf("nodes = %v", got)
	}
	release := up.calls("/inv/release")
	if len(release) != 1 || release[0]["reservation"] != "R-1" {
		t.Fatalf("release calls = %v", release)
	}
	if n := len(up.calls("/dispatch/assign")); n != 1 {
		t.Fatalf("a 400 must not be retried; assign called %d times", n)
	}
	h.checkInvariants(runID)
}
