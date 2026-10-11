// Package mcp implements direct and agent-driven calls through saved MCP connections.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"uuid"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/internal/core/executor/agent"
	"github.com/kartikey/workflow_service/pkg/mcpclient"
	"github.com/kartikey/workflow_service/pkg/netguard"
)

type Executor struct {
	conns  executor.Connections
	policy netguard.Policy
}

func New(conns executor.Connections, policy netguard.Policy) *Executor {
	return &Executor{conns: conns, policy: policy}
}
func (*Executor) Type() string { return "mcp.call" }
func (*Executor) Describe() executor.Description {
	return executor.Description{Title: "Call an MCP tool", Category: "integration", Description: "Initializes a saved MCP server connection and calls one tool over streamable HTTP or legacy SSE. Remote tools may have side effects; retries require an idempotent tool.", Example: `- id: search
  type: mcp.call
  config:
    connection: knowledge
    tool: search
    arguments: {query: '${input.question}'}
    timeout: 30s`, Output: `MCP result: {content: [...], structuredContent: ..., isError: false}`}
}
func (*Executor) ValidateConfig(raw json.RawMessage) error {
	return executor.CheckFields(raw, []executor.Field{{Name: "connection", Kind: "string", Required: true}, {Name: "tool", Kind: "string", Required: true}, {Name: "arguments", Kind: "object"}, {Name: "timeout", Kind: "duration"}})
}
func (e *Executor) open(ctx context.Context, tenant uuid.UUID, name string) (*mcpclient.Session, func(), error) {
	noop := func() {}
	if name == "" || e.conns == nil {
		return nil, noop, executor.Permanent("bad_config", errors.New("a saved MCP connection is required"))
	}
	conn, err := e.conns.Connection(ctx, tenant, name, "mcp")
	if errors.Is(err, executor.ErrConnectionNotFound) {
		return nil, noop, executor.Permanent("E_CONNECTION_NOT_FOUND", fmt.Errorf("no MCP connection named %q", name))
	}
	if err != nil {
		return nil, noop, executor.Retryable("connection_lookup", err)
	}
	target, _ := conn.Config["url"].(string)
	transport, _ := conn.Config["transport"].(string)
	cfg := mcpclient.Config{URL: target, Transport: transport, Token: conn.Secret["token"]}
	if err := mcpclient.Validate(cfg); err != nil {
		return nil, noop, executor.Permanent("bad_config", err)
	}
	client := e.policy.Client(2 * time.Minute)
	session, err := mcpclient.Connect(ctx, cfg, client)
	if err != nil {
		client.CloseIdleConnections()
		return nil, noop, classify(ctx, err)
	}
	return session, func() { session.Close(); client.CloseIdleConnections() }, nil
}
func (e *Executor) Execute(ctx context.Context, req executor.Request) (executor.Result, error) {
	var c struct {
		Connection string          `json:"connection"`
		Tool       string          `json:"tool"`
		Arguments  json.RawMessage `json:"arguments"`
		Timeout    string          `json:"timeout"`
	}
	if err := executor.DecodeConfig(req.Config, &c); err != nil {
		return executor.Result{}, err
	}
	timeout := 30 * time.Second
	if c.Timeout != "" {
		d, err := time.ParseDuration(c.Timeout)
		if err != nil || d <= 0 {
			return executor.Result{}, executor.Permanent("bad_config", errors.New("timeout must be positive"))
		}
		timeout = min(d, 2*time.Minute)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var arguments map[string]any
	if c.Tool == "" || len(c.Tool) > 128 || len(c.Arguments) > mcpclient.MaxBytes {
		return executor.Result{}, executor.Permanent("bad_config", errors.New("invalid MCP tool or arguments"))
	}
	if len(c.Arguments) > 0 && (json.Unmarshal(c.Arguments, &arguments) != nil || arguments == nil) {
		return executor.Result{}, executor.Permanent("bad_config", errors.New("arguments must be an object"))
	}
	session, closeSession, err := e.open(ctx, req.TenantID, c.Connection)
	if err != nil {
		return executor.Result{}, err
	}
	defer closeSession()
	output, failed, err := session.Call(ctx, c.Tool, c.Arguments)
	if err != nil {
		return executor.Result{}, classify(ctx, err)
	}
	if failed {
		return executor.Result{Output: output}, executor.Permanent("mcp_tool_error", mcpclient.ToolError())
	}
	return executor.Result{Output: output}, nil
}
func classify(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, netguard.ErrBlocked) {
		return executor.Permanent("egress_blocked", err)
	}
	if mcpclient.IsPermanent(err) {
		return executor.Permanent("mcp_protocol", err)
	}
	// Calls may have committed a side effect before a transport failed. The
	// workflow author chooses retries explicitly, as with HTTP nodes.
	return executor.Retryable("mcp_transport", err)
}

// Resolve implements agent.DynamicTools. Sessions are scoped to one attempt;
// tools cannot choose a connection or impersonate another tenant.
func (e *Executor) Resolve(ctx context.Context, tenant uuid.UUID, refs []string) ([]agent.Tool, func(), error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	var closers []func()
	cleanup := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
		cancel()
	}
	var out []agent.Tool
	for index, ref := range refs {
		name := strings.TrimPrefix(ref, "mcp:")
		session, closeSession, err := e.open(ctx, tenant, name)
		if err != nil {
			return nil, cleanup, err
		}
		closers = append(closers, closeSession)
		tools, err := session.ListTools(ctx)
		if err != nil {
			return nil, cleanup, classify(ctx, err)
		}
		seen := map[string]bool{}
		for toolIndex, tool := range tools {
			if tool.Name == "" || len(tool.Name) > 128 || seen[tool.Name] {
				return nil, cleanup, executor.Permanent("mcp_protocol", errors.New("invalid or duplicate MCP tool name"))
			}
			seen[tool.Name] = true
			alias := fmt.Sprintf("mcp_%d_tool_%d", index, toolIndex)
			out = append(out, remoteTool{alias: alias, server: name, tool: tool, session: session, tenant: tenant})
		}
		if len(out) > mcpclient.MaxTools {
			return nil, cleanup, executor.Permanent("bad_config", errors.New("agent has more than 128 MCP tools"))
		}
	}
	return out, cleanup, nil
}

type remoteTool struct {
	alias, server string
	tool          mcpclient.Tool
	session       *mcpclient.Session
	tenant        uuid.UUID
}

func (t remoteTool) Name() string { return t.alias }
func (t remoteTool) Description() string {
	return fmt.Sprintf("MCP server %s, tool %s: %s", t.server, t.tool.Name, t.tool.Description)
}
func (t remoteTool) InputSchema() json.RawMessage { return t.tool.InputSchema }
func (t remoteTool) Call(ctx context.Context, call agent.Call, input json.RawMessage) (json.RawMessage, error) {
	if call.TenantID != t.tenant {
		return nil, errors.New("MCP tool tenant mismatch")
	}
	output, failed, err := t.session.Call(ctx, t.tool.Name, input)
	if err != nil {
		return nil, err
	}
	if failed {
		return nil, fmt.Errorf("MCP tool failed: %s", output)
	}
	return output, nil
}
