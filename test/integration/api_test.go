//go:build integration

package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"

	"github.com/kartikey/workflow_service/internal/api"
	"github.com/kartikey/workflow_service/internal/server"
	"github.com/kartikey/workflow_service/internal/services/tasks"
	"github.com/kartikey/workflow_service/pkg/auth"
	"github.com/kartikey/workflow_service/pkg/uid"
)

type apiClient struct {
	t      *testing.T
	base   string
	tenant pgtype.UUID
}

func (h *harness) apiServer(authenticator auth.Authenticator) string {
	h.t.Helper()
	if authenticator == nil {
		authenticator = auth.DevHeader{}
	}
	srv, err := server.New(server.Config{Address: "127.0.0.1:0", API: &api.Services{
		Pool: h.pool, Workflows: h.publish, Runs: h.flow, Executors: h.registry, Tasks: tasks.NewService(h.flow), Admin: h.flow, Auth: authenticator,
	}}, h.pool, zap.NewNop())
	if err != nil {
		h.t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	h.t.Cleanup(ts.Close)
	return ts.URL
}

func (c apiClient) do(method, path, contentType string, body []byte, headers ...string) (int, map[string]any, http.Header) {
	c.t.Helper()
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("X-Tenant-ID", uid.String(c.tenant))
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	var decoded map[string]any
	_ = json.Unmarshal(data, &decoded)
	if decoded == nil {
		decoded = map[string]any{"_raw": string(data)}
	}
	return res.StatusCode, decoded, res.Header
}

func (c apiClient) json(method, path string, body any, headers ...string) (int, map[string]any) {
	c.t.Helper()
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	status, decoded, _ := c.do(method, path, "application/json", data, headers...)
	return status, decoded
}

func (c apiClient) yaml(path, source string) (int, map[string]any) {
	c.t.Helper()
	status, decoded, _ := c.do(http.MethodPost, path, "application/x-yaml", []byte(source))
	return status, decoded
}

const apiWorkflow = `
apiVersion: workflow/v1
name: api-flow
inputs:
  order: {type: string, required: true}
correlation: '${input.order}'
nodes:
  - id: shape
    type: transform
    config: {value: {order: '${input.order}'}}
  - id: approve
    type: wait_event
    needs: [shape]
    config:
      key: 'approve:${input.order}'
      timeout: 1h
      schema: {type: object, required: [by], properties: {by: {type: string}}}
  - id: done
    type: transform
    needs: [approve]
    config: {value: '${nodes.approve.output.by}'}
outputs:
  approved_by: '${nodes.done.output}'
`

func TestAPIPublishDryRunAndUnchanged(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	c := apiClient{t: t, base: h.apiServer(nil), tenant: h.tenant}

	status, body := c.yaml("/be/v1/workflows?dry_run=true", apiWorkflow)
	if status != http.StatusOK || body["valid"] != true {
		t.Fatalf("dry run = %d %v", status, body)
	}
	var versions int
	if err := h.pool.QueryRow(ctx, `select count(*) from workflow_versions`).Scan(&versions); err != nil || versions != 0 {
		t.Fatalf("dry run persisted %d versions (%v)", versions, err)
	}

	status, body = c.yaml("/be/v1/workflows", apiWorkflow)
	if status != http.StatusCreated || body["version"] != float64(1) || body["unchanged"] != false {
		t.Fatalf("publish = %d %v", status, body)
	}
	// Same YAML with different formatting compiles to the same spec.
	status, body = c.yaml("/be/v1/workflows", strings.ReplaceAll(apiWorkflow, "timeout: 1h", "timeout: 1h   # comment"))
	if status != http.StatusOK || body["unchanged"] != true || body["version"] != float64(1) {
		t.Fatalf("republish = %d %v", status, body)
	}
	if err := h.pool.QueryRow(ctx, `select count(*) from workflow_versions`).Scan(&versions); err != nil || versions != 1 {
		t.Fatalf("versions after republish = %d (%v), want 1", versions, err)
	}

	// The broken workflow reports every error in one response.
	status, body = c.yaml("/be/v1/workflows", `
apiVersion: workflow/v2
name: Broken_Name
nodes:
  - {id: a, type: teleport, config: {}}
  - {id: b, type: noop, needs: [reserv_stock], config: {}}
  - {id: c, type: transform, needs: [a], config: {value: '${nodes.b.output}'}}
  - {id: d, type: noop, retry: {max_attempts: 50}, config: {}}
  - {id: e, type: noop, join: some, config: {}}
`)
	errorsList, _ := body["errors"].([]any)
	if status != http.StatusBadRequest || body["code"] != "spec_invalid" || len(errorsList) < 6 {
		t.Fatalf("broken publish = %d, %d errors: %v", status, len(errorsList), body)
	}

	status, raw, headers := c.do(http.MethodGet, "/be/v1/workflows/api-flow/versions/1?format=yaml", "", nil)
	if status != http.StatusOK || raw["_raw"] != apiWorkflow || !strings.Contains(headers.Get("Content-Type"), "yaml") {
		t.Fatalf("yaml round trip = %d %v", status, raw)
	}
	status, body = c.json(http.MethodGet, "/be/v1/workflows/api-flow", nil)
	if status != http.StatusOK || len(body["versions"].([]any)) != 1 || body["latest"] == nil {
		t.Fatalf("detail = %d %v", status, body)
	}
	status, body = c.json(http.MethodGet, "/be/v1/executors", nil)
	if status != http.StatusOK || len(body["items"].([]any)) < 6 {
		t.Fatalf("executors = %d %v", status, body)
	}
}

func TestAPIRunsSignalsEventsAndCancel(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	c := apiClient{t: t, base: h.apiServer(nil), tenant: h.tenant}
	if status, body := c.yaml("/be/v1/workflows", apiWorkflow); status != http.StatusCreated {
		t.Fatalf("publish = %d %v", status, body)
	}
	h.startWorkers(2)

	create := map[string]any{"workflow": "api-flow", "input": map[string]any{"order": "O-1"}}
	status, run := c.json(http.MethodPost, "/be/v1/runs", create, "Idempotency-Key", "k-1")
	if status != http.StatusCreated || run["correlation_id"] != "O-1" {
		t.Fatalf("create = %d %v", status, run)
	}
	runID := run["id"].(string)
	status, again := c.json(http.MethodPost, "/be/v1/runs", create, "Idempotency-Key", "k-1")
	if status != http.StatusOK || again["id"] != runID {
		t.Fatalf("idempotent create = %d %v", status, again)
	}
	create["input"] = map[string]any{"order": "O-2"}
	if status, body := c.json(http.MethodPost, "/be/v1/runs", create, "Idempotency-Key", "k-1"); status != http.StatusConflict {
		t.Fatalf("conflicting create = %d %v", status, body)
	}
	if status, body := c.json(http.MethodPost, "/be/v1/runs", map[string]any{"workflow": "api-flow", "input": map[string]any{}}); status != http.StatusBadRequest || body["code"] != "input_invalid" {
		t.Fatalf("invalid input = %d %v", status, body)
	}

	// SSE follows the run while the signal is delivered.
	streamed := make(chan []string, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, c.base+"/be/v1/runs/"+runID+"/events", nil)
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("X-Tenant-ID", uid.String(h.tenant))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			streamed <- nil
			return
		}
		defer res.Body.Close()
		var types []string
		scanner := bufio.NewScanner(res.Body)
		for scanner.Scan() {
			if name, ok := strings.CutPrefix(scanner.Text(), "event: "); ok {
				types = append(types, name)
			}
		}
		streamed <- types
	}()

	waitNode(t, h, runID, "approve", "waiting")
	if status, body := c.json(http.MethodPost, "/be/v1/signals", map[string]any{"key": "approve:nobody", "payload": map[string]any{}}); status != http.StatusNotFound || body["code"] != "no_waiter" {
		t.Fatalf("no waiter = %d %v", status, body)
	}
	if status, body := c.json(http.MethodPost, "/be/v1/signals", map[string]any{"key": "approve:O-1", "payload": map[string]any{"by": 7}}); status != http.StatusUnprocessableEntity || body["code"] != "payload_invalid" {
		t.Fatalf("invalid payload = %d %v", status, body)
	}
	status, signal := c.json(http.MethodPost, "/be/v1/signals", map[string]any{"key": "approve:O-1", "payload": map[string]any{"by": "grace"}})
	if status != http.StatusOK || signal["run_id"] != runID || signal["node_id"] != "approve" || signal["duplicate"] != false {
		t.Fatalf("signal = %d %v", status, signal)
	}
	status, duplicate := c.json(http.MethodPost, "/be/v1/signals", map[string]any{"key": "approve:O-1", "payload": map[string]any{"by": "grace"}})
	if status != http.StatusOK || duplicate["duplicate"] != true || duplicate["run_id"] != runID {
		t.Fatalf("duplicate signal = %d %v", status, duplicate)
	}
	h.waitStatus(mustParse(t, runID), 10*time.Second, "succeeded")

	select {
	case types := <-streamed:
		joined := strings.Join(types, ",")
		if !strings.HasPrefix(joined, "run.created,run.started") || !strings.HasSuffix(joined, "run.succeeded,end") || !strings.Contains(joined, "node.signalled") {
			t.Fatalf("SSE events = %v", types)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SSE stream did not end after the run finished")
	}

	status, detail := c.json(http.MethodGet, "/be/v1/runs/"+runID, nil)
	progress, _ := detail["progress"].(map[string]any)
	if status != http.StatusOK || detail["output"].(map[string]any)["approved_by"] != "grace" || progress["succeeded"] != float64(3) || progress["total"] != float64(3) {
		t.Fatalf("run detail = %d %v", status, detail)
	}
	status, events := c.json(http.MethodGet, "/be/v1/runs/"+runID+"/events?after=2&limit=3", nil)
	items := events["items"].([]any)
	if status != http.StatusOK || len(items) != 3 || items[0].(map[string]any)["seq"] != float64(3) {
		t.Fatalf("events page = %d %v", status, events)
	}
	status, nodes := c.json(http.MethodGet, "/be/v1/runs/"+runID+"/nodes?include=input", nil)
	if status != http.StatusOK || len(nodes["items"].([]any)) != 3 {
		t.Fatalf("nodes = %d %v", status, nodes)
	}
	if status, body := c.json(http.MethodPost, "/be/v1/runs/"+runID+"/cancel", map[string]any{"reason": "late"}); status != http.StatusConflict {
		t.Fatalf("cancel finished run = %d %v", status, body)
	}

	// A second run is cancelled while waiting, then retried as a new run.
	status, second := c.json(http.MethodPost, "/be/v1/runs", map[string]any{"workflow": "api-flow", "input": map[string]any{"order": "O-3"}})
	if status != http.StatusCreated {
		t.Fatalf("second run = %d %v", status, second)
	}
	secondID := second["id"].(string)
	waitNode(t, h, secondID, "approve", "waiting")
	if status, body := c.json(http.MethodPost, "/be/v1/runs/"+secondID+"/cancel", map[string]any{"reason": "customer cancelled"}); status != http.StatusAccepted {
		t.Fatalf("cancel = %d %v", status, body)
	}
	h.checkInvariants(mustParse(t, secondID))
	status, retried := c.json(http.MethodPost, "/be/v1/runs/"+secondID+"/retry", nil)
	if status != http.StatusCreated || retried["parent_run_id"] != secondID || retried["id"] == secondID {
		t.Fatalf("retry = %d %v", status, retried)
	}
	status, list := c.json(http.MethodGet, "/be/v1/runs?workflow=api-flow&limit=2", nil)
	if status != http.StatusOK || len(list["items"].([]any)) != 2 || list["next"] == nil {
		t.Fatalf("list page 1 = %d %v", status, list)
	}
	status, page2 := c.json(http.MethodGet, "/be/v1/runs?workflow=api-flow&limit=2&after="+list["next"].(string), nil)
	if status != http.StatusOK || len(page2["items"].([]any)) != 1 || page2["next"] != nil {
		t.Fatalf("list page 2 = %d %v", status, page2)
	}

	// Archived workflows reject new runs with 409; existing runs are untouched.
	if status, body := c.json(http.MethodDelete, "/be/v1/workflows/api-flow", nil); status != http.StatusOK {
		t.Fatalf("archive = %d %v", status, body)
	}
	if status, body := c.json(http.MethodPost, "/be/v1/runs", map[string]any{"workflow": "api-flow", "input": map[string]any{"order": "O-9"}}); status != http.StatusConflict || body["code"] != "workflow_archived" {
		t.Fatalf("run archived = %d %v", status, body)
	}
	h.checkInvariants(mustParse(t, runID))
}

func TestAPITenantIsolation(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	base := h.apiServer(nil)
	a := apiClient{t: t, base: base, tenant: h.tenant}
	b := apiClient{t: t, base: base, tenant: newUUID(t)}
	if status, _ := a.yaml("/be/v1/workflows", apiWorkflow); status != http.StatusCreated {
		t.Fatal("publish failed")
	}
	_, run := a.json(http.MethodPost, "/be/v1/runs", map[string]any{"workflow": "api-flow", "input": map[string]any{"order": "O-1"}})
	runID := run["id"].(string)
	h.startWorkers(1)
	waitNode(t, h, runID, "approve", "waiting")

	for _, path := range []string{"/be/v1/runs/" + runID, "/be/v1/runs/" + runID + "/nodes", "/be/v1/runs/" + runID + "/events", "/be/v1/workflows/api-flow", "/be/v1/workflows/api-flow/versions/1"} {
		if status, body := b.json(http.MethodGet, path, nil); status != http.StatusNotFound {
			t.Errorf("tenant B GET %s = %d %v, want 404", path, status, body)
		}
	}
	if status, _ := b.json(http.MethodPost, "/be/v1/runs/"+runID+"/cancel", nil); status != http.StatusNotFound {
		t.Errorf("tenant B cancel = %d, want 404", status)
	}
	if status, _ := b.json(http.MethodPost, "/be/v1/runs/"+runID+"/retry", nil); status != http.StatusNotFound {
		t.Errorf("tenant B retry = %d, want 404", status)
	}
	if status, _ := b.json(http.MethodPost, "/be/v1/runs", map[string]any{"workflow": "api-flow", "input": map[string]any{"order": "O-1"}}); status != http.StatusNotFound {
		t.Errorf("tenant B start = %d, want 404", status)
	}
	if status, _ := b.json(http.MethodPost, "/be/v1/signals", map[string]any{"key": "approve:O-1", "payload": map[string]any{"by": "x"}}); status != http.StatusNotFound {
		t.Errorf("tenant B signal = %d, want 404", status)
	}
	for _, path := range []string{"/be/v1/runs", "/be/v1/workflows"} {
		if status, body := b.json(http.MethodGet, path, nil); status != http.StatusOK || len(body["items"].([]any)) != 0 {
			t.Errorf("tenant B list %s = %d %v, want empty", path, status, body)
		}
	}
	if status, body := a.json(http.MethodGet, "/be/v1/runs/"+runID, nil); status != http.StatusOK || body["status"] != "waiting" {
		t.Errorf("tenant A still sees its run: %d %v", status, body)
	}
	unauthenticated := apiClient{t: t, base: base}
	if status, _ := unauthenticated.json(http.MethodGet, "/be/v1/runs", nil); status != http.StatusUnauthorized {
		t.Errorf("no tenant = %d, want 401", status)
	}
}

func waitNode(t *testing.T, h *harness, runID, nodeID, status string) {
	t.Helper()
	id := mustParse(t, runID)
	deadline := time.Now().Add(10 * time.Second)
	for h.nodeStatuses(id)[nodeID] != status {
		if time.Now().After(deadline) {
			t.Fatalf("node %s never reached %s: %v", nodeID, status, h.nodeStatuses(id))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func mustParse(t *testing.T, text string) pgtype.UUID {
	t.Helper()
	id, err := uid.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
