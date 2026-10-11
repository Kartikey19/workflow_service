package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"uuid"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/pkg/dbx"
)

type dynamicSource struct {
	closed int
	err    error
	tool   Tool
}

func (d *dynamicSource) Resolve(context.Context, uuid.UUID, []string) ([]Tool, func(), error) {
	return []Tool{d.tool}, func() { d.closed++ }, d.err
}
func TestDynamicSessionsCloseAndToolsRemainScoped(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "model reply", true: "discovery error"}[fail], func(t *testing.T) {
			tool := &echoTool{}
			client := &scripted{replies: []any{toolUse("1", "echo", `{}`), text("done", 1, 1)}}
			e := New(NewStatic("anthropic", client, nil), nil, nil) // echo is deliberately absent from the static registry
			source := &dynamicSource{tool: tool}
			if fail {
				source.err = executor.Permanent("mcp_protocol", errors.New("discovery failed"))
			}
			e.WithDynamicTools(source)
			_, err := run(t, e, `{"model":"m","prompt":"p","tools":["mcp:server"],"max_iterations":3}`)
			if fail && err == nil || !fail && err != nil {
				t.Fatalf("result: %v", err)
			}
			if source.closed != 1 {
				t.Fatalf("closed %d times", source.closed)
			}
			if _, ok := e.tools.Get("echo"); ok {
				t.Fatal("discovered tool leaked into shared registry")
			}
			if !fail && len(tool.calls) != 1 {
				t.Fatalf("calls: %v", tool.calls)
			}
		})
	}
	// The model also cannot use a registered tool absent from its definition.
	registry, _ := NewTools(&echoTool{})
	result := runTool(context.Background(), registry, Call{}, nil, toolUse("1", "echo", `{}`).Content[0])
	if !result.IsError {
		t.Fatal("ungranted tool executed")
	}
}
func TestDatabaseResultPreservesJSONAndTruncation(t *testing.T) {
	result := dbx.Result{Columns: []string{"text"}, Rows: []map[string]any{{"text": strings.Repeat("x", maxToolResultBytes)}, {"text": "second"}}}
	raw, err := databaseResult(result)
	if err != nil || len(raw) > maxToolResultBytes || !json.Valid(raw) || !strings.Contains(string(raw), `"truncated":true`) {
		t.Fatalf("result: %s %v", raw, err)
	}
}

func TestToolErrorsAreBounded(t *testing.T) {
	tool := &echoTool{err: errors.New(strings.Repeat("x", maxToolResultBytes*2))}
	registry, _ := NewTools(tool)
	result := runTool(context.Background(), registry, Call{}, []string{"echo"}, toolUse("1", "echo", `{}`).Content[0])
	if !result.IsError || len(result.ToolContent) > maxToolResultBytes+32 || !strings.Contains(result.ToolContent, "[truncated]") {
		t.Fatalf("unbounded tool error, length %d", len(result.ToolContent))
	}
}
