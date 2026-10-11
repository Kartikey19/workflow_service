package agent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/internal/core/executor/connectors"
	"github.com/kartikey/workflow_service/pkg/dbx"
	"github.com/kartikey/workflow_service/pkg/netguard"
)

// DatabaseTools expose reads against the connection bound by the agent author.
// The model never supplies a connection name or credentials.
func DatabaseTools(conns executor.Connections, policy netguard.Policy) []Tool {
	return DatabaseToolsWithCatalog(connectors.NewCatalog(conns, policy, nil, ""))
}
func DatabaseToolsWithCatalog(c *connectors.Catalog) []Tool {
	out := []Tool{}
	for _, name := range []string{"list_tables", "describe_table", "run_query"} {
		out = append(out, databaseTool{name: name, catalog: c})
	}
	return out
}

type databaseTool struct {
	name    string
	catalog *connectors.Catalog
}

func (t databaseTool) Name() string { return t.name }
func (t databaseTool) Description() string {
	switch t.name {
	case "list_tables":
		return "Lists up to 500 tables and views visible on this agent's database connection."
	case "describe_table":
		return "Describes the columns and primary keys of a table on this agent's database connection."
	default:
		return "Runs one read-only SQL statement on this agent's database connection with bound parameters, a row cap and a timeout."
	}
}
func (t databaseTool) InputSchema() json.RawMessage {
	switch t.name {
	case "list_tables":
		return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
	case "describe_table":
		return json.RawMessage(`{"type":"object","required":["table"],"properties":{"table":{"type":"string"}},"additionalProperties":false}`)
	default:
		return json.RawMessage(`{"type":"object","required":["sql"],"properties":{"sql":{"type":"string"},"params":{"type":"array"},"max_rows":{"type":"integer","minimum":1,"maximum":1000},"timeout":{"type":"string"}},"additionalProperties":false}`)
	}
}
func (t databaseTool) Call(ctx context.Context, call Call, input json.RawMessage) (output json.RawMessage, err error) {
	action := map[string]string{"list_tables": "db.list_tables", "describe_table": "db.describe_table", "run_query": "db.query"}[t.name]
	return invokeConnector(ctx, t.catalog, call, action, call.DatabaseConnection, t.InputSchema(), input)
}

// Preserve valid JSON and the truncated flag within the model's tool budget.
func databaseResult(result dbx.Result) (json.RawMessage, error) {
	for {
		raw, err := json.Marshal(map[string]any{"columns": result.Columns, "rows": result.Rows, "row_count": len(result.Rows), "truncated": result.Truncated})
		if err != nil {
			return nil, err
		}
		if len(raw) <= maxToolResultBytes {
			return raw, nil
		}
		if len(result.Rows) == 0 {
			return nil, errors.New("database column metadata exceeds the tool result limit")
		}
		result.Rows = result.Rows[:len(result.Rows)/2]
		result.Truncated = true
	}
}
