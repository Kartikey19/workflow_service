package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/internal/core/executor/database"
	"github.com/kartikey/workflow_service/pkg/dbx"
	"github.com/kartikey/workflow_service/pkg/netguard"
	"github.com/kartikey/workflow_service/pkg/vault"
)

// DatabaseTools expose reads against the connection bound by the agent author.
// The model never supplies a connection name or credentials.
func DatabaseTools(conns executor.Connections, policy netguard.Policy) []Tool {
	var out []Tool
	for _, name := range []string{"list_tables", "describe_table", "run_query"} {
		out = append(out, databaseTool{name: name, conns: conns, policy: policy})
	}
	return out
}

type databaseTool struct {
	name   string
	conns  executor.Connections
	policy netguard.Policy
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
	var in struct {
		Table   string `json:"table"`
		SQL     string `json:"sql"`
		Params  []any  `json:"params"`
		MaxRows int    `json:"max_rows"`
		Timeout string `json:"timeout"`
	}
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("database tool input: %w", err)
	}
	if call.DatabaseConnection == "" || t.conns == nil {
		return nil, errors.New("this agent has no database_connection")
	}
	timeout := dbx.DefaultTimeout
	if in.Timeout != "" {
		d, err := time.ParseDuration(in.Timeout)
		if err != nil || d <= 0 {
			return nil, errors.New("timeout must be a positive duration")
		}
		timeout = min(d, dbx.DefaultTimeout)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := t.conns.Connection(ctx, call.TenantID, call.DatabaseConnection, database.Kinds...)
	if err != nil {
		return nil, fmt.Errorf("load database connection: %w", err)
	}
	if call.RedactSecrets != nil {
		call.RedactSecrets(conn.Secret["password"])
	}
	defer func() {
		if password := conn.Secret["password"]; password != "" && len(output) > 0 {
			output = vault.RedactJSON(output, password)
		}
		if len(output) > maxToolResultBytes {
			output = nil
			err = errors.New("database tool result exceeds 32 KiB")
		}
	}()
	cfg := database.Config(conn)
	cfg.Dial = t.policy.DialContext
	db, err := dbx.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	switch t.name {
	case "list_tables":
		tables, err := db.Tables(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"tables": tables})
	case "describe_table":
		columns, err := db.Describe(ctx, in.Table)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"columns": columns})
	default:
		if in.SQL == "" {
			return nil, errors.New("sql is required")
		}
		if in.MaxRows < 0 || in.MaxRows > 1000 {
			return nil, errors.New("max_rows must be between 1 and 1000")
		}
		result, err := db.Query(ctx, in.SQL, in.Params, dbx.QueryOptions{MaxRows: in.MaxRows, Timeout: timeout})
		if err != nil {
			return nil, err
		}
		return databaseResult(result)
	}
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
