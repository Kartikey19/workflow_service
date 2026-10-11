//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/pkg/uid"
)

var (
	buildOnce sync.Once
	binDir    string
	buildErr  error
)

// binaries builds cmd/api and cmd/worker once per test process.
func binaries(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		binDir, buildErr = os.MkdirTemp("", "wf-bin-")
		if buildErr != nil {
			return
		}
		_, file, _, _ := runtime.Caller(0)
		root := filepath.Join(filepath.Dir(file), "../..")
		for _, name := range []string{"api", "worker"} {
			cmd := exec.Command("go", "build", "-o", filepath.Join(binDir, name), "./cmd/"+name)
			cmd.Dir = root
			if output, err := cmd.CombinedOutput(); err != nil {
				buildErr = fmt.Errorf("build %s: %v\n%s", name, err, output)
				return
			}
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binDir
}

// schemaURL points DATABASE_URL at the harness's isolated schema.
func (h *harness) schemaURL() string {
	h.t.Helper()
	var schema string
	if err := h.pool.QueryRow(context.Background(), "select current_schema()").Scan(&schema); err != nil {
		h.t.Fatal(err)
	}
	parsed, err := url.Parse(os.Getenv("DATABASE_URL"))
	if err != nil {
		h.t.Fatal("DATABASE_URL must be a URL for process tests")
	}
	params := parsed.Query()
	params.Set("search_path", schema)
	parsed.RawQuery = params.Encode()
	return parsed.String()
}

type process struct {
	cmd    *exec.Cmd
	output *bytes.Buffer
	done   chan struct{} // closed when the process has exited
}

// spawn starts a service binary; the test cleanup kills it if still running.
func (h *harness) spawn(binary string, env ...string) *process {
	h.t.Helper()
	cmd := exec.Command(filepath.Join(binaries(h.t), binary))
	cmd.Env = append(os.Environ(),
		"DATABASE_URL="+h.schemaURL(), "APP_ENV=testing", "LOG_LEVEL=error",
		"WORKER_LEASE_TTL=2s", "WORKER_POLL_INTERVAL=50ms", "WORKER_SWEEP_INTERVAL=200ms",
		"WORKER_CONCURRENCY=8", "WORKER_BATCH_SIZE=4", "SHUTDOWN_GRACE=300ms", "EGRESS_ALLOW_PRIVATE=true",
		"DB_MAX_CONNS=8")
	cmd.Env = append(cmd.Env, env...)
	p := &process{cmd: cmd, output: &bytes.Buffer{}, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = p.output, p.output
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	h.t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-p.done
		if h.t.Failed() {
			h.t.Logf("%s output:\n%s", binary, p.output.String())
		}
	})
	return p
}

func (p *process) signal(t *testing.T, sig syscall.Signal) {
	t.Helper()
	if err := p.cmd.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("process did not exit after %v", sig)
	}
}

// blockingUpstream holds requests open until released, reporting each arrival.
func (h *harness) blockingUpstream() (url string, arrived chan string, release func()) {
	arrived = make(chan string, 16)
	gate := make(chan struct{})
	var once sync.Once
	_, url = h.upstream(func(_ int32, w http.ResponseWriter, r *http.Request) {
		arrived <- r.Header.Get("Idempotency-Key")
		select {
		case <-gate:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		case <-r.Context().Done():
		}
	})
	return url, arrived, func() { once.Do(func() { close(gate) }) }
}

func nodeWorkers(t *testing.T, h *harness, runID pgtype.UUID) []string {
	t.Helper()
	rows, err := h.pool.Query(context.Background(), `select payload->>'worker_id' from run_events where run_id = $1 and type = 'node.started' order by seq`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

// M2 crash test / v1 DoD: SIGKILL a worker holding a lease. After the lease
// TTL the sweeper re-delivers the same row and another worker completes the
// run. attempt does not increment: lease expiry is re-delivery, not a retry.
func TestKill9WorkerMidNodeIsRedelivered(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	url, arrived, release := h.blockingUpstream()
	h.mustPublish(httpWorkflow("crash", url+"/charge", "{max_attempts: 1}"))
	victim := h.spawn("worker", "WORKER_ID=victim")
	runID := h.start("crash", nil)
	firstKey := receive(t, arrived, "the victim never called the upstream")
	victim.signal(t, syscall.SIGKILL)

	var owner pgtype.Text
	var status string
	if err := h.pool.QueryRow(ctx, `select status, lease_owner from node_executions where run_id = $1`, runID).Scan(&status, &owner); err != nil {
		t.Fatal(err)
	}
	if status != "running" || owner.String != "victim" {
		t.Fatalf("after kill: %s owned by %v; the dead worker should still hold the lease", status, owner)
	}
	release()
	survivor := h.spawn("worker", "WORKER_ID=survivor")
	_ = survivor
	if got := h.waitStatus(runID, 15*time.Second, "succeeded", "failed"); got != "succeeded" {
		t.Fatalf("run = %s", got)
	}
	secondKey := receive(t, arrived, "the survivor never called the upstream")
	var rows, attempt int
	if err := h.pool.QueryRow(ctx, `select count(*), max(attempt) from node_executions where run_id = $1`, runID).Scan(&rows, &attempt); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || attempt != 1 || firstKey != secondKey {
		t.Fatalf("rows = %d attempt = %d keys %q/%q; want one row, attempt 1, same key", rows, attempt, firstKey, secondKey)
	}
	if workers := nodeWorkers(t, h, runID); len(workers) != 2 || workers[0] != "victim" || workers[1] != "survivor" {
		t.Fatalf("node.started by %v, want victim then survivor", workers)
	}
	h.checkInvariants(runID)
}

// M10 / M6: SIGTERM on a worker mid-node releases its lease, so another
// replica continues within a second instead of after a lease TTL.
func TestSIGTERMHandsNodeToAnotherReplicaQuickly(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	url, arrived, release := h.blockingUpstream()
	h.mustPublish(httpWorkflow("handoff", url+"/work", "{max_attempts: 1}"))
	leaving := h.spawn("worker", "WORKER_ID=leaving")
	runID := h.start("handoff", nil)
	receive(t, arrived, "the leaving worker never called the upstream")
	h.spawn("worker", "WORKER_ID=staying")
	time.Sleep(200 * time.Millisecond) // the second replica is up and polling
	stopped := time.Now()
	leaving.signal(t, syscall.SIGTERM)
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the other replica never picked the node up")
	}
	if handoff := time.Since(stopped); handoff > time.Second {
		t.Fatalf("hand-off took %v, want under 1s (lease TTL is 2s)", handoff)
	}
	release()
	if got := h.waitStatus(runID, 10*time.Second, "succeeded", "failed"); got != "succeeded" {
		t.Fatalf("run = %s", got)
	}
	var attempt int
	if err := h.pool.QueryRow(ctx, `select max(attempt) from node_executions where run_id = $1`, runID).Scan(&attempt); err != nil || attempt != 1 {
		t.Fatalf("attempt = %d (%v), want 1: a release is re-delivery, not a retry", attempt, err)
	}
	h.checkInvariants(runID)
}

// M10: the API and two worker replicas run as separate processes against one
// database; both replicas take work and nothing executes twice.
func TestSeparateAPIAndWorkerProcessesShareLoad(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	h.spawn("api", "HTTP_ADDRESS="+address)
	h.spawn("worker", "WORKER_ID=replica-a")
	h.spawn("worker", "WORKER_ID=replica-b")
	base := "http://" + address
	deadline := time.Now().Add(15 * time.Second)
	for {
		res, err := http.Get(base + "/readyz")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("API process never became ready")
		}
		time.Sleep(50 * time.Millisecond)
	}
	c := apiClient{t: t, base: base, tenant: h.tenant}
	if status, body := c.yaml("/v1/workflows", `
apiVersion: workflow/v1
name: fan
nodes:
  - {id: a, type: transform, config: {value: 1}}
  - {id: b, type: transform, needs: [a], config: {value: 2}}
  - {id: c, type: transform, needs: [a], config: {value: 3}}
  - {id: d, type: transform, needs: [b, c], config: {value: '${nodes.b.output + nodes.c.output}'}}
outputs: {sum: '${nodes.d.output}'}
`); status != http.StatusCreated {
		t.Fatalf("publish = %d %v", status, body)
	}
	const runs = 40
	runIDs := make([]pgtype.UUID, 0, runs)
	for range runs {
		status, body := c.json(http.MethodPost, "/v1/runs", map[string]any{"workflow": "fan"})
		if status != http.StatusCreated {
			t.Fatalf("create run = %d %v", status, body)
		}
		runIDs = append(runIDs, mustParse(t, body["id"].(string)))
	}
	seen := map[string]int{}
	for _, id := range runIDs {
		if got := h.waitStatus(id, 20*time.Second, "succeeded", "failed"); got != "succeeded" {
			t.Fatalf("run %s = %s", uid.String(id), got)
		}
		output, _ := h.runOutput(id)
		if output["sum"] != float64(5) {
			t.Fatalf("output = %v", output)
		}
		for _, worker := range nodeWorkers(t, h, id) {
			seen[worker]++
		}
		h.checkInvariants(id)
	}
	if seen["replica-a"] == 0 || seen["replica-b"] == 0 || seen["replica-a"]+seen["replica-b"] != runs*4 {
		t.Fatalf("node executions by worker = %v, want both replicas and %d total", seen, runs*4)
	}
	res, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var metrics bytes.Buffer
	_, _ = metrics.ReadFrom(res.Body)
	if !strings.Contains(metrics.String(), "wf_http_request_duration_seconds") {
		t.Fatal("API /metrics is missing request metrics")
	}
	_ = json.Valid
}

func receive(t *testing.T, ch <-chan string, failure string) string {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(15 * time.Second):
		t.Fatal(failure)
		return ""
	}
}
