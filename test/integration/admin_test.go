//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func adminHeaders() []string { return []string{"X-Platform-Admin", "true"} }

func (h *harness) nodeExecutionID(runID pgtype.UUID, nodeID string, attempt int) string {
	h.t.Helper()
	var id string
	err := h.pool.QueryRow(context.Background(), `
		select id::text from node_executions where run_id = $1 and node_id = $2 and attempt = $3`,
		runID, nodeID, attempt).Scan(&id)
	if err != nil {
		h.t.Fatalf("find %s attempt %d: %v", nodeID, attempt, err)
	}
	return id
}

func (h *harness) attemptStatus(runID pgtype.UUID, nodeID string, attempt int) string {
	h.t.Helper()
	var status string
	err := h.pool.QueryRow(context.Background(), `
		select status from node_executions where run_id = $1 and node_id = $2 and attempt = $3`,
		runID, nodeID, attempt).Scan(&status)
	if err != nil {
		h.t.Fatalf("status of %s attempt %d: %v", nodeID, attempt, err)
	}
	return status
}

func (h *harness) eventTypes(runID pgtype.UUID) []string {
	h.t.Helper()
	rows, err := h.pool.Query(context.Background(), `select type from run_events where run_id = $1 order by seq`, runID)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var eventType string
		if err := rows.Scan(&eventType); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, eventType)
	}
	return out
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func TestAdminRoutesNeedPlatformAdmin(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	c := apiClient{t: t, base: h.apiServer(nil), tenant: h.tenant}
	const missing = "00000000-0000-7000-8000-000000000001"
	for _, path := range []string{
		"/v1/admin/node-executions/" + missing + "/retry",
		"/v1/admin/node-executions/" + missing + "/skip",
		"/v1/admin/workers/w1/release-leases",
	} {
		// An organization owner is not enough.
		if status, body := c.json("POST", path, nil); status != 403 {
			t.Fatalf("%s as owner = %d %v, want 403", path, status, body)
		}
	}
	if status, _ := c.json("POST", "/v1/admin/node-executions/"+missing+"/retry", nil, adminHeaders()...); status != 404 {
		t.Fatalf("retry of an unknown node = %d, want 404", status)
	}
	if status, _ := c.json("POST", "/v1/admin/node-executions/not-a-uuid/skip", nil, adminHeaders()...); status != 404 {
		t.Fatalf("skip of a malformed id = %d, want 404", status)
	}
}

func TestAdminRetryNode(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(`
apiVersion: workflow/v1
name: retry-me
nodes:
  - {id: a, type: script, config: {}}
  - {id: b, type: script, needs: [a], config: {}}
`)
	c := apiClient{t: t, base: h.apiServer(nil), tenant: h.tenant}
	runID := h.start("retry-me", nil) // no workers yet: a is ready
	first := h.nodeExecutionID(runID, "a", 1)

	status, body := c.json("POST", "/v1/admin/node-executions/"+first+"/retry", nil, adminHeaders()...)
	if status != 200 || body["attempt"] != float64(2) || body["status"] != "ready" {
		t.Fatalf("retry = %d %v", status, body)
	}
	if got := h.attemptStatus(runID, "a", 1); got != "cancelled" {
		t.Fatalf("attempt 1 = %s, want cancelled", got)
	}
	if got := h.attemptStatus(runID, "a", 2); got != "ready" {
		t.Fatalf("attempt 2 = %s, want ready", got)
	}
	// The same node cannot be retried twice: a later attempt exists.
	if status, body := c.json("POST", "/v1/admin/node-executions/"+first+"/retry", nil, adminHeaders()...); status != 409 {
		t.Fatalf("second retry = %d %v, want 409", status, body)
	}
	if !contains(h.eventTypes(runID), "admin.node_retried") {
		t.Fatalf("events = %v, want admin.node_retried", h.eventTypes(runID))
	}

	h.startWorkers(1)
	h.waitStatus(runID, 10*time.Second, "succeeded")
	if h.script.callCount("a") != 1 {
		t.Fatalf("a ran %d times, want exactly the retried attempt", h.script.callCount("a"))
	}
	// A finished run cannot be retried node by node.
	if status, body := c.json("POST", "/v1/admin/node-executions/"+h.nodeExecutionID(runID, "a", 2)+"/retry", nil, adminHeaders()...); status != 409 {
		t.Fatalf("retry in a finished run = %d %v, want 409", status, body)
	}
	h.checkInvariants(runID)
}

func TestAdminSkipNode(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(`
apiVersion: workflow/v1
name: skip-me
nodes:
  - {id: a, type: script, config: {}}
  - {id: b, type: script, config: {}}
  - {id: c, type: script, needs: [a, b], join: any, config: {}}
`)
	c := apiClient{t: t, base: h.apiServer(nil), tenant: h.tenant}
	runID := h.start("skip-me", nil)
	a := h.nodeExecutionID(runID, "a", 1)

	status, body := c.json("POST", "/v1/admin/node-executions/"+a+"/skip", nil, adminHeaders()...)
	if status != 200 || body["status"] != "skipped" {
		t.Fatalf("skip = %d %v", status, body)
	}
	if status, body := c.json("POST", "/v1/admin/node-executions/"+a+"/skip", nil, adminHeaders()...); status != 409 {
		t.Fatalf("skipping a skipped node = %d %v, want 409", status, body)
	}
	if !contains(h.eventTypes(runID), "admin.node_skipped") {
		t.Fatalf("events = %v, want admin.node_skipped", h.eventTypes(runID))
	}

	h.startWorkers(1)
	h.waitStatus(runID, 10*time.Second, "succeeded")
	if got := h.nodeStatuses(runID); got["a"] != "skipped" || got["b"] != "succeeded" || got["c"] != "succeeded" {
		t.Fatalf("nodes = %v", got)
	}
	if h.script.callCount("a") != 0 {
		t.Fatal("a ran although it was skipped")
	}
	h.checkInvariants(runID)
}

func TestAdminReleaseWorkerLeases(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(`
apiVersion: workflow/v1
name: orphaned
nodes:
  - {id: a, type: script, config: {}}
`)
	c := apiClient{t: t, base: h.apiServer(nil), tenant: h.tenant}
	runID := h.start("orphaned", nil)
	// Simulate a crashed worker: the node is running under a long lease.
	if _, err := h.pool.Exec(ctx, `
		update node_executions
		   set status = 'running', lease_owner = 'dead-worker', lease_token = gen_random_uuid(),
		       lease_expires_at = now() + interval '1 hour'
		 where run_id = $1`, runID); err != nil {
		t.Fatal(err)
	}
	status, body := c.json("POST", "/v1/admin/workers/dead-worker/release-leases", nil, adminHeaders()...)
	if status != 200 || body["released"] != float64(1) {
		t.Fatalf("release = %d %v", status, body)
	}
	if got := h.attemptStatus(runID, "a", 1); got != "ready" {
		t.Fatalf("a = %s, want ready after release", got)
	}
	status, body = c.json("POST", "/v1/admin/workers/dead-worker/release-leases", nil, adminHeaders()...)
	if status != 200 || body["released"] != float64(0) {
		t.Fatalf("second release = %d %v", status, body)
	}
	h.startWorkers(1)
	h.waitStatus(runID, 10*time.Second, "succeeded")
	h.checkInvariants(runID)
}
