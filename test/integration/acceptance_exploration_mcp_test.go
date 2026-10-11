//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kartikey/workflow_service/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type toolStep struct {
	name      string
	arguments string
}

// scriptedToolLLM asks for tools, then answers using the accumulated results.
// It exercises the production OpenAI adapter and the actual agent tool loop.
func scriptedToolLLM(t *testing.T, steps []toolStep) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var captured []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			io.WriteString(w, `{"data":[{"id":"fake-model"}]}`)
			return
		}
		var request struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name        string `json:"name"`
					Description string `json:"description"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		var results []string
		for _, message := range request.Messages {
			if message.Role == "tool" {
				results = append(results, message.Content)
			}
		}
		mu.Lock()
		captured = append([]string(nil), results...)
		mu.Unlock()
		if len(results) < len(steps) {
			step := steps[len(results)]
			if strings.HasPrefix(step.name, "$connector:") {
				ref := strings.TrimPrefix(step.name, "$connector:")
				for _, tool := range request.Tools {
					if strings.Contains(tool.Function.Description, ref+".") {
						step.name = tool.Function.Name
						break
					}
				}
			}
			if strings.HasPrefix(step.name, "$remote") {
				wanted := strings.TrimPrefix(step.name, "$remote:")
				for _, tool := range request.Tools {
					if strings.HasPrefix(tool.Function.Name, "mcp_") && (step.name == "$remote" || strings.Contains(tool.Function.Description, "tool "+wanted+":")) {
						step.name = tool.Function.Name
						break
					}
				}
			}
			fmt.Fprintf(w, `{"model":"fake-model","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":3}}`, fmt.Sprintf("call_%d", len(results)), step.name, step.arguments)
			return
		}
		summary, _ := json.Marshal(map[string]any{"summary": strings.Join(results, "\n")})
		fmt.Fprintf(w, `{"model":"fake-model","choices":[{"finish_reason":"stop","message":{"content":%q}}],"usage":{"prompt_tokens":10,"completion_tokens":3}}`, string(summary))
	}))
	t.Cleanup(server.Close)
	return server, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), captured...) }
}

func TestExplorationAgent(t *testing.T) {
	eachDatabase(t, func(t *testing.T, s *stack, owner *person, b backend, tb tables, conn string) {
		describe, _ := json.Marshal(map[string]any{"table": tb.events})
		query, _ := json.Marshal(map[string]any{"sql": "select count(*) as n from " + tb.events, "max_rows": 1})
		capped, _ := json.Marshal(map[string]any{"sql": b.series(100), "max_rows": 2})
		injected, _ := json.Marshal(map[string]any{"sql": "select count(*) as n from " + tb.events + " where name = " + placeholder(b, 1), "params": []any{"first' OR 1=1 --"}})
		write, _ := json.Marshal(map[string]any{"sql": "delete from " + tb.events})
		model, results := scriptedToolLLM(t, []toolStep{{"list_tables", "{}"}, {"describe_table", string(describe)}, {"run_query", string(query)}, {"run_query", string(write)}, {"run_query", string(capped)}, {"run_query", string(injected)}})
		owner.mustOK("POST", "/be/v1/providers", map[string]any{"name": "explorer-provider", "provider": "openai_compatible", "base_url": model.URL, "api_key": "llm-test-key"}, 201)
		body := agentBody("explorer", "explorer-provider")
		body["database_connection"] = conn
		body["tools"] = []string{"list_tables", "describe_table", "run_query"}
		body["max_iterations"] = 8
		created := owner.mustOK("POST", "/be/v1/agents", body, 201)
		if created["database_connection"] != conn {
			t.Fatalf("missing binding: %v", created)
		}
		fetched := owner.mustOK("GET", "/be/v1/agents/explorer", nil, 200)
		if fetched["database_connection"] != conn {
			t.Fatalf("lost binding: %v", fetched)
		}
		tested := owner.mustOK("POST", "/be/v1/agents/explorer/test", map[string]any{"input": map[string]any{"question": "How many events are there?"}}, 200)
		if tested["ok"] != true {
			t.Fatalf("exploration: %v", tested)
		}
		got := results()
		if len(got) != 6 || !strings.Contains(got[0], strings.TrimPrefix(tb.events, strings.Split(tb.events, ".")[0]+".")) || !strings.Contains(got[1], `"primary_key":true`) || !strings.Contains(got[2], `"n":1`) || !strings.Contains(got[3], "only read statements") || !strings.Contains(got[4], `"truncated":true`) || !strings.Contains(got[4], `"row_count":2`) || !strings.Contains(got[5], `"n":0`) {
			t.Fatalf("tool results = %v", got)
		}
		// An attempted write did not remove the row.
		owner.mustOK("POST", "/be/v1/agents/explorer/test", map[string]any{"input": map[string]any{}}, 200)
		if !strings.Contains(results()[2], `"n":1`) {
			t.Fatal("agent changed the database")
		}
		bad := agentBody("unbound", "explorer-provider")
		bad["tools"] = []string{"run_query"}
		if status, _ := owner.call("POST", "/be/v1/agents", bad); status != 400 {
			t.Fatalf("unbound tool status %d", status)
		}
		bad["database_connection"] = "missing"
		if status, _ := owner.call("POST", "/be/v1/agents", bad); status != 400 {
			t.Fatalf("unknown DB status %d", status)
		}
		other := s.owner(t, "Other exploration org")
		otherBody := agentBody("steal", "")
		otherBody["database_connection"] = conn
		otherBody["tools"] = []string{"run_query"}
		if status, _ := other.call("POST", "/be/v1/agents", otherBody); status != 400 {
			t.Fatalf("foreign database status %d", status)
		}
		// Workflow execution uses the same saved definition and binding.
		if status, resp := owner.publish("apiVersion: workflow/v1\nname: explore\nnodes:\n  - id: answer\n    type: agent\n    config: {agent: explorer, input: {question: count events}}\n"); status != 201 {
			t.Fatalf("publish %d %v", status, resp)
		}
		if _, run := runToEnd(t, owner, "explore"); run["status"] != "succeeded" {
			t.Fatalf("workflow %v", run)
		}
	})
}

func TestMCPConnectionsNodesAndAgents(t *testing.T) {
	for _, transport := range []string{"streamable_http", "sse"} {
		t.Run(transport, func(t *testing.T) {
			ctx := context.Background()
			s := newStack(t, ctx)
			owner := s.owner(t, "MCP Co "+transport)
			var calls atomic.Int32
			server := mcp.NewServer(&mcp.Implementation{Name: "acceptance", Version: "1"}, nil)
			server.AddTool(&mcp.Tool{Name: "echo", Description: "Echo input", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				calls.Add(1)
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(req.Params.Arguments)}}}, nil
			})
			server.AddTool(&mcp.Tool{Name: "fail", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "expected failure"}}}, nil
			})
			var handler http.Handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{JSONResponse: true})
			if transport == "sse" {
				handler = mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return server }, nil)
			}
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer mcp-test-secret" {
					w.WriteHeader(401)
					return
				}
				handler.ServeHTTP(w, r)
			}))
			defer remote.Close()
			body := map[string]any{"name": "knowledge", "kind": "mcp", "config": map[string]any{"url": remote.URL, "transport": transport}, "secret": map[string]any{"token": "mcp-test-secret"}}
			created := owner.mustOK("POST", "/be/v1/connections", body, 201)
			if calls.Load() != 0 {
				t.Fatal("test-before-save executed a tool")
			}
			if strings.Contains(toJSON(created), "mcp-test-secret") {
				t.Fatal("API exposed MCP token")
			}
			source := dbWorkflow("remote-call", "mcp.call", `{connection: knowledge, tool: echo, arguments: {text: '${input.since}'}}`)
			if status, resp := owner.publish(source); status != 201 {
				t.Fatalf("publish %d %v", status, resp)
			}
			_, started := owner.call("POST", "/be/v1/runs", map[string]any{"workflow": "remote-call", "input": map[string]any{"since": "hello"}})
			if run := owner.waitRun(started["id"].(string)); run["status"] != "succeeded" {
				t.Fatalf("MCP run %v", run)
			}
			var output string
			if err := s.h.pool.QueryRow(ctx, `select output::text from node_executions where run_id=$1 and node_id='act'`, mustParse(t, started["id"].(string))).Scan(&output); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output, "hello") || calls.Load() != 1 {
				t.Fatalf("MCP output %s, calls %d", output, calls.Load())
			}
			for _, tt := range []struct{ name, tool, code string }{{"failed-tool", "fail", "mcp_tool_error"}, {"unknown-tool", "missing", "mcp_protocol"}} {
				if status, resp := owner.publish(dbWorkflow(tt.name, "mcp.call", fmt.Sprintf("{connection: knowledge, tool: %s}", tt.tool))); status != 201 {
					t.Fatalf("publish %d %v", status, resp)
				}
				id, run := runToEnd(t, owner, tt.name)
				attempts := s.h.attempts(mustParse(t, id), "act")
				if run["status"] != "failed" || len(attempts) != 1 || attempts[0]["code"] != tt.code {
					t.Fatalf("%s: %v %v", tt.name, run, attempts)
				}
			}
			model, results := scriptedToolLLM(t, []toolStep{{"$remote", `{"text":"agent hello"}`}})
			owner.mustOK("POST", "/be/v1/providers", map[string]any{"name": "remote-provider", "provider": "openai_compatible", "base_url": model.URL, "api_key": "llm-test-key"}, 201)
			agent := agentBody("remote-agent", "remote-provider")
			agent["tools"] = []string{"mcp:knowledge"}
			owner.mustOK("POST", "/be/v1/agents", agent, 201)
			tested := owner.mustOK("POST", "/be/v1/agents/remote-agent/test", map[string]any{"input": map[string]any{}}, 200)
			if tested["ok"] != true || len(results()) != 1 || !strings.Contains(results()[0], "agent hello") {
				t.Fatalf("MCP agent = %v %v", tested, results())
			}
			other := s.owner(t, "Other MCP org")
			bad := agentBody("foreign", "")
			bad["tools"] = []string{"mcp:knowledge"}
			if status, _ := other.call("POST", "/be/v1/agents", bad); status != 400 {
				t.Fatalf("foreign MCP status %d", status)
			}
			before := calls.Load()
			if status, resp := other.publish(dbWorkflow("foreign-call", "mcp.call", `{connection: knowledge, tool: echo}`)); status != 201 {
				t.Fatalf("publish %d %v", status, resp)
			}
			id, run := runToEnd(t, other, "foreign-call")
			attempts := s.h.attempts(mustParse(t, id), "act")
			if run["status"] != "failed" || len(attempts) != 1 || attempts[0]["code"] != "E_CONNECTION_NOT_FOUND" || calls.Load() != before {
				t.Fatalf("foreign call %v %v", run, attempts)
			}
			if n := s.h.occurrences("mcp-test-secret"); n != 0 {
				t.Fatalf("MCP token in %d persisted rows", n)
			}
		})
	}
}

func TestMCPEgressGuard(t *testing.T) {
	s := newStack(t, context.Background(), func(c *config.Config) { c.Egress.AllowPrivate = false })
	owner := s.owner(t, "Blocked MCP")
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer target.Close()
	body := map[string]any{"name": "blocked", "kind": "mcp", "config": map[string]any{"url": target.URL}}
	if status, _ := owner.call("POST", "/be/v1/connections", body); status != 422 || hits.Load() != 0 {
		t.Fatalf("egress: status %d hits %d", status, hits.Load())
	}
	body["skip_test"] = true
	owner.mustOK("POST", "/be/v1/connections", body, 201)
	if status, resp := owner.publish(dbWorkflow("blocked-mcp", "mcp.call", `{connection: blocked, tool: echo}`)); status != 201 {
		t.Fatalf("publish %d %v", status, resp)
	}
	id, run := runToEnd(t, owner, "blocked-mcp")
	attempts := s.h.attempts(mustParse(t, id), "act")
	if run["status"] != "failed" || len(attempts) != 1 || attempts[0]["code"] != "egress_blocked" || hits.Load() != 0 {
		t.Fatalf("blocked call %v %v hits %d", run, attempts, hits.Load())
	}
}

// Opt in explicitly: external availability must not gate CI. The model is
// scripted, while discovery and the read-only call use the live public server.
func TestMCPPublicAgent(t *testing.T) {
	target := os.Getenv("M15_PUBLIC_MCP_URL")
	if target == "" {
		t.Skip("set M15_PUBLIC_MCP_URL to opt into the public MCP smoke test")
	}
	s := newStack(t, context.Background())
	owner := s.owner(t, "Public MCP")
	owner.mustOK("POST", "/be/v1/connections", map[string]any{"name": "public-docs", "kind": "mcp", "config": map[string]any{"url": target}}, 201)
	model, results := scriptedToolLLM(t, []toolStep{{"$remote:read_wiki_structure", `{"repoName":"modelcontextprotocol/go-sdk"}`}})
	owner.mustOK("POST", "/be/v1/providers", map[string]any{"name": "public-provider", "provider": "openai_compatible", "base_url": model.URL, "api_key": "fake-public-key"}, 201)
	body := agentBody("public-reader", "public-provider")
	body["tools"] = []string{"mcp:public-docs"}
	owner.mustOK("POST", "/be/v1/agents", body, 201)
	result := owner.mustOK("POST", "/be/v1/agents/public-reader/test", map[string]any{"input": map[string]any{"question": "List the public SDK documentation topics."}}, 200)
	if result["ok"] != true || len(results()) != 1 || !strings.Contains(results()[0], `"content"`) || strings.Contains(results()[0], `"isError":true`) {
		t.Fatalf("public tool call: %v %v", result, results())
	}
}
