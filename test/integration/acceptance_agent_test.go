//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kartikey/workflow_service/internal/core/executor/agent"
	"github.com/kartikey/workflow_service/internal/services/workflows"
	"github.com/kartikey/workflow_service/pkg/llm"
	"github.com/kartikey/workflow_service/pkg/llm/anthropic"
)

// fakeLLM answers model calls from a per-test function and records them.
type fakeLLM struct {
	mu       sync.Mutex
	handler  func(call int, req llm.Request) (llm.Response, error)
	requests []llm.Request
}

func (f *fakeLLM) set(handler func(call int, req llm.Request) (llm.Response, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = handler
	f.requests = nil
}

func (f *fakeLLM) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	call := len(f.requests)
	handler := f.handler
	f.mu.Unlock()
	if handler == nil {
		return llm.Response{}, errors.New("fake model has no handler")
	}
	return handler(call, req)
}

func (f *fakeLLM) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// kb stands in for the knowledge-base search tool the spec example presumes.
type kb struct{}

func (kb) Name() string        { return "search_kb" }
func (kb) Description() string { return "Searches the knowledge base." }
func (kb) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`)
}
func (kb) Call(context.Context, agent.Call, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{"articles":["KB-1"]}`), nil
}

func newTestAgent(t *testing.T, model llm.Client) *agent.Executor {
	t.Helper()
	tools, err := agent.NewTools(agent.CurrentTime{}, kb{})
	if err != nil {
		t.Fatal(err)
	}
	pricing := llm.Pricing{"claude-sonnet": {InputPerMTok: 3, OutputPerMTok: 15}}
	return agent.New(agent.NewStatic("test", model, pricing), tools, nil)
}

func reply(text string) llm.Response {
	return llm.Response{
		Model: "claude-sonnet-5-5", StopReason: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 1000, OutputTokens: 200},
		Content: []llm.Block{{Type: llm.BlockText, Text: text}},
	}
}

func wantTool(id, name string) llm.Response {
	return llm.Response{
		Model: "claude-sonnet-5-5", StopReason: llm.StopToolUse, Usage: llm.Usage{InputTokens: 500, OutputTokens: 20},
		Content: []llm.Block{{Type: llm.BlockToolUse, ToolUseID: id, ToolName: name, ToolInput: json.RawMessage(`{}`)}},
	}
}

func (h *harness) nodeMetrics(runID any, nodeID string) map[string]any {
	h.t.Helper()
	var metrics map[string]any
	err := h.pool.QueryRow(context.Background(), `
		select coalesce(metrics, '{}'::jsonb) from node_executions
		 where run_id = $1 and node_id = $2 order by attempt desc limit 1`, runID, nodeID).Scan(&metrics)
	if err != nil {
		h.t.Fatal(err)
	}
	return metrics
}

const classifyWorkflow = `
apiVersion: workflow/v1
name: classify-ticket
inputs:
  text: {type: string, required: true}
nodes:
  - id: classify
    type: agent
    config:
      model: claude-sonnet-5-5
      system: "Classify the ticket. Respond with JSON only."
      prompt: '${input.text}'
      tools: [current_time]
      max_iterations: 4
      output_schema:
        type: object
        required: [severity, category]
        properties:
          severity: {type: string, enum: [P0, P1, P2]}
          category: {type: string}
  - id: summary
    type: transform
    needs: [classify]
    config:
      value:
        label: '${nodes.classify.output.severity}'
outputs:
  severity: '${nodes.classify.output.severity}'
`

// M7: a tool-using agent produces structured output that downstream nodes
// read, and its spend lands in the node metrics.
func TestAgentToolLoopFeedsDownstreamAndReportsCost(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(classifyWorkflow)
	h.llm.set(func(call int, req llm.Request) (llm.Response, error) {
		if call == 1 {
			return wantTool("tu_1", "current_time"), nil
		}
		last := req.Messages[len(req.Messages)-1].Content[0]
		if last.Type != llm.BlockToolResult || !strings.Contains(last.ToolContent, `"now"`) {
			t.Errorf("model did not receive the tool result: %+v", last)
		}
		return reply("```json\n{\"severity\":\"P0\",\"category\":\"billing\"}\n```"), nil
	})
	h.startWorkers(1)
	runID := h.start("classify-ticket", map[string]any{"text": "charged twice"})
	if status := h.waitStatus(runID, 15*time.Second, "succeeded", "failed"); status != "succeeded" {
		t.Fatalf("run = %s, nodes %v", status, h.nodeStatuses(runID))
	}
	if out, _ := h.runOutput(runID); out["severity"] != "P0" {
		t.Fatalf("outputs = %v", out)
	}
	m := h.nodeMetrics(runID, "classify")
	if m["iterations"] != float64(2) || m["tool_calls"] != float64(1) || m["tokens_in"] != float64(1500) || m["tokens_out"] != float64(220) ||
		m["stop_reason"] != "end_turn" || m["model"] != "claude-sonnet-5-5" {
		t.Fatalf("metrics = %v", m)
	}
	wantCost := (1500*3.0 + 220*15.0) / 1e6
	if cost, _ := m["cost_usd"].(float64); cost < wantCost-1e-9 || cost > wantCost+1e-9 {
		t.Fatalf("cost_usd = %v, want %v", m["cost_usd"], wantCost)
	}

	// The bill query from the plan produces a plausible per-tenant figure.
	var tenant string
	var total float64
	err := h.pool.QueryRow(ctx, `
		select tenant_id::text, sum((metrics->>'cost_usd')::numeric)::float8
		  from node_executions where metrics ? 'cost_usd' group by tenant_id`).Scan(&tenant, &total)
	if err != nil || total < wantCost-1e-9 || total > wantCost+1e-9 {
		t.Fatalf("bill = %v, %v; want %v", total, err, wantCost)
	}
	h.checkInvariants(runID)
}

// M7: a reply that violates output_schema fails once; it is not retried even
// though the workflow allows retries.
func TestAgentSchemaViolationIsPermanentAndNotRetried(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(strings.Replace(classifyWorkflow, "    type: agent\n", "    type: agent\n    retry: {max_attempts: 4, backoff: fixed, initial: 10ms}\n", 1))
	h.llm.set(func(int, llm.Request) (llm.Response, error) {
		return reply(`{"severity":"P9","category":"billing"}`), nil
	})
	h.startWorkers(1)
	runID := h.start("classify-ticket", map[string]any{"text": "x"})
	if status := h.waitStatus(runID, 15*time.Second, "failed"); status != "failed" {
		t.Fatalf("run = %s", status)
	}
	attempts := h.attempts(runID, "classify")
	if len(attempts) != 1 || attempts[0]["status"] != "failed" || attempts[0]["code"] != "invalid_output" {
		t.Fatalf("attempts = %v, want one failed invalid_output attempt", attempts)
	}
	if h.llm.calls() != 1 {
		t.Fatalf("model called %d times, want once", h.llm.calls())
	}
	if m := h.nodeMetrics(runID, "classify"); m["tokens_in"] != float64(1000) {
		t.Fatalf("a failed attempt must still record spend: %v", m)
	}
	h.checkInvariants(runID)
}

// M7: max_iterations is enforced and reported in metrics.iterations.
func TestAgentMaxIterationsEnforced(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	h.mustPublish(strings.Replace(classifyWorkflow, "max_iterations: 4", "max_iterations: 3", 1))
	h.llm.set(func(call int, _ llm.Request) (llm.Response, error) { return wantTool("tu", "current_time"), nil })
	h.startWorkers(1)
	runID := h.start("classify-ticket", map[string]any{"text": "x"})
	h.waitStatus(runID, 15*time.Second, "failed")
	attempts := h.attempts(runID, "classify")
	if len(attempts) != 1 || attempts[0]["code"] != "max_iterations_exceeded" {
		t.Fatalf("attempts = %v", attempts)
	}
	if h.llm.calls() != 3 {
		t.Fatalf("model called %d times, want exactly max_iterations (3)", h.llm.calls())
	}
	if m := h.nodeMetrics(runID, "classify"); m["iterations"] != float64(3) {
		t.Fatalf("metrics = %v", m)
	}
}

// M7: a 429 is retryable and honours Retry-After, using the real Anthropic
// client over HTTP against a fake API.
func TestAgentRateLimitIsRetriedAfterRetryAfter(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	var mu sync.Mutex
	var seen []time.Time
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, time.Now())
		n := len(seen)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"model":"claude-sonnet-5-5","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5},
			"content":[{"type":"text","text":"{\"severity\":\"P2\",\"category\":\"other\"}"}]}`))
	}))
	t.Cleanup(api.Close)
	wire := anthropic.New("test-key", api.URL, nil)
	h.llm.set(func(_ int, req llm.Request) (llm.Response, error) { return wire.Complete(ctx, req) })

	h.mustPublish(strings.Replace(classifyWorkflow, "    type: agent\n", "    type: agent\n    retry: {max_attempts: 3, backoff: fixed, initial: 10ms}\n", 1))
	h.startWorkers(1)
	runID := h.start("classify-ticket", map[string]any{"text": "x"})
	if status := h.waitStatus(runID, 20*time.Second, "succeeded", "failed"); status != "succeeded" {
		t.Fatalf("run = %s, nodes %v", status, h.nodeStatuses(runID))
	}
	attempts := h.attempts(runID, "classify")
	if len(attempts) != 2 || attempts[0]["code"] != "rate_limited" {
		t.Fatalf("attempts = %v", attempts)
	}
	if delay := attempts[1]["delay"].(time.Duration); delay < 900*time.Millisecond {
		t.Fatalf("retry delay = %v, want at least the 1s Retry-After", delay)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("API calls = %d, want 2", len(seen))
	}
}

// A spec without max_iterations does not publish.
func TestAgentWithoutMaxIterationsFailsToPublish(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ctx)
	source := strings.Replace(classifyWorkflow, "      max_iterations: 4\n", "", 1)
	_, err := h.publish.Publish(ctx, workflows.PublishParams{TenantID: h.tenant, Source: []byte(source)})
	var compileErr *workflows.CompileError
	if !errors.As(err, &compileErr) {
		t.Fatalf("err = %v, want a compile error", err)
	}
	found := false
	for _, d := range compileErr.Diagnostics {
		if d.Severity == "error" && strings.Contains(d.Message, "max_iterations is required") {
			found = true
		}
	}
	if !found {
		t.Fatalf("diagnostics = %v", compileErr.Diagnostics)
	}
}

// triageWorkflow is 02 §5.2 pointed at one test server.
func triageWorkflow(base string) string {
	return strings.NewReplacer("BASE", base).Replace(`
apiVersion: workflow/v1
name: support-triage
inputs:
  ticket_id: {type: string, required: true}
correlation: ${input.ticket_id}
nodes:
  - id: fetch_ticket
    type: http
    config:
      method: GET
      url: "BASE/desk/tickets/${input.ticket_id}"
  - id: classify
    type: agent
    needs: [fetch_ticket]
    config:
      model: claude-sonnet-5-5
      system: "Classify the ticket. Respond with JSON only."
      prompt: ${nodes.fetch_ticket.output.body.description}
      tools: [search_kb]
      max_iterations: 6
      output_schema:
        type: object
        required: [severity, category]
        properties:
          severity: {type: string, enum: [P0, P1, P2]}
          category: {type: string}
  - id: route
    type: branch
    needs: [classify]
    config:
      cases:
        - when: ${nodes.classify.output.severity == "P0"}
          goto: page_oncall
      default: auto_reply
  - id: page_oncall
    type: http
    needs: [route]
    config: {method: POST, url: "BASE/pager/page", body: {ticket: '${input.ticket_id}'}}
  - id: auto_reply
    type: agent
    needs: [route]
    config:
      model: claude-sonnet-5-5
      system: "Draft a helpful reply."
      prompt: ${nodes.fetch_ticket.output.body.description}
      max_iterations: 3
  - id: record
    type: http
    needs: [page_oncall, auto_reply]
    join: any
    config:
      method: POST
      url: "BASE/desk/tickets/${input.ticket_id}/triage"
      body: {severity: '${nodes.classify.output.severity}'}
`)
}

// 02 §5.2 end to end: the agent's structured verdict routes the run, and the
// merge node runs after whichever arm was taken.
func TestAgenticExampleRoutesOnTheAgentsVerdict(t *testing.T) {
	for _, tt := range []struct {
		name     string
		severity string
		taken    string
		skipped  string
	}{
		{"P0 pages on-call", "P0", "page_oncall", "auto_reply"},
		{"P2 gets an automatic reply", "P2", "auto_reply", "page_oncall"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			h := newHarness(t, ctx)
			up, base := newDeliveryUpstream(t, map[string]func(http.ResponseWriter){
				"/desk/tickets/T-1": func(w http.ResponseWriter) {
					_, _ = w.Write([]byte(`{"description":"my invoice is wrong"}`))
				},
			})
			h.mustPublish(triageWorkflow(base))
			h.llm.set(func(_ int, req llm.Request) (llm.Response, error) {
				if strings.Contains(req.System, "Classify") {
					if !strings.Contains(req.Messages[0].Content[0].Text, "my invoice is wrong") {
						t.Errorf("classifier prompt = %q", req.Messages[0].Content[0].Text)
					}
					return reply(`{"severity":"` + tt.severity + `","category":"billing"}`), nil
				}
				return reply("Thanks, we are looking into your invoice."), nil
			})
			h.startWorkers(2)
			runID := h.start("support-triage", map[string]any{"ticket_id": "T-1"})
			if status := h.waitStatus(runID, 20*time.Second, "succeeded", "failed"); status != "succeeded" {
				t.Fatalf("run = %s, nodes %v", status, h.nodeStatuses(runID))
			}
			got := h.nodeStatuses(runID)
			if got[tt.taken] != "succeeded" || got[tt.skipped] != "skipped" || got["record"] != "succeeded" {
				t.Fatalf("nodes = %v", got)
			}
			recorded := up.calls("/desk/tickets/T-1/triage")
			if len(recorded) != 1 || recorded[0]["severity"] != tt.severity {
				t.Fatalf("triage records = %v", recorded)
			}
			if (len(up.calls("/pager/page")) == 1) != (tt.severity == "P0") {
				t.Fatalf("pager calls = %d for %s", len(up.calls("/pager/page")), tt.severity)
			}
			h.checkInvariants(runID)
		})
	}
}
