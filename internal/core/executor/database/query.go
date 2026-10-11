package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/pkg/dbx"
	"github.com/kartikey/workflow_service/pkg/netguard"
)

// Query runs one read-only statement and returns its rows.
type Query struct{ base }

var _ executor.Executor = (*Query)(nil)

func NewQuery(conns executor.Connections, policy netguard.Policy) *Query {
	return &Query{base{conns, policy}}
}

func (*Query) Type() string { return "db.query" }

func (*Query) Describe() executor.Description {
	return executor.Description{
		Title: "Query a database", Category: "integration",
		Description: "Runs one read statement (SELECT, WITH, SHOW, DESCRIBE or EXPLAIN) against a PostgreSQL or MySQL connection, in a read-only " +
			"transaction with a statement timeout and a row cap. Values go in `params` as $1, $2 (PostgreSQL) or ? (MySQL); `sql` itself cannot " +
			"contain ${...}, so a workflow value can never become SQL text.",
		Example: `- id: recent_orders
  type: db.query
  config:
    connection: warehouse
    sql: "select id, total from orders where customer_id = $1 and created_at > $2 order by id limit 100"
    params: ['${input.customer_id}', '${input.since}']
    max_rows: 100`,
		Output: `{"columns": ["id", "total"], "rows": [{"id": 1, "total": "9.99"}], "row_count": 1, "truncated": false}`,
	}
}

func (*Query) ValidateConfig(raw json.RawMessage) error {
	if err := executor.CheckFields(raw, []executor.Field{
		{Name: "connection", Kind: "string", Required: true},
		{Name: "sql", Kind: "string", Required: true},
		{Name: "params", Kind: "array"},
		{Name: "max_rows", Kind: "integer"},
		{Name: "timeout", Kind: "duration"},
		{Name: "fail_on_truncate", Kind: "bool"},
	}); err != nil {
		return err
	}
	var c map[string]any
	_ = json.Unmarshal(raw, &c)
	if text, _ := c["sql"].(string); executor.IsExpression(text) {
		return errors.New("sql must not contain ${...}: pass values with params so they cannot become SQL text")
	}
	return literalInt(c, "max_rows", 1, dbx.MaxRowsLimit)
}

func (q *Query) Execute(ctx context.Context, req executor.Request) (executor.Result, error) {
	var c struct {
		Connection     string `json:"connection"`
		SQL            string `json:"sql"`
		Params         []any  `json:"params"`
		MaxRows        int    `json:"max_rows"`
		Timeout        string `json:"timeout"`
		FailOnTruncate bool   `json:"fail_on_truncate"`
	}
	if err := executor.DecodeConfig(req.Config, &c); err != nil {
		return executor.Result{}, err
	}
	timeout, err := timeoutOf(c.Timeout)
	if err != nil {
		return executor.Result{}, executor.Permanent("bad_config", err)
	}
	if c.MaxRows < 0 || c.MaxRows > dbx.MaxRowsLimit {
		return executor.Result{}, executor.Permanent("bad_config", fmt.Errorf("max_rows must be between 1 and %d", dbx.MaxRowsLimit))
	}
	if !plainIdentifier(c.SQL) {
		return executor.Result{}, executor.Permanent("bad_config", errors.New("sql is required"))
	}
	db, err := q.open(ctx, req, c.Connection)
	if err != nil {
		return executor.Result{}, err
	}
	defer db.Close()
	result, err := db.Query(ctx, c.SQL, c.Params, dbx.QueryOptions{MaxRows: c.MaxRows, Timeout: timeout})
	if err != nil {
		return executor.Result{}, classify(ctx, err)
	}
	if result.Truncated && c.FailOnTruncate {
		return executor.Result{}, executor.Permanent("too_many_rows", errors.New("the query returned more rows than max_rows (or more data than fits in a node output)"))
	}
	columns, rows := result.Columns, result.Rows
	if columns == nil {
		columns = []string{}
	}
	if rows == nil {
		rows = []map[string]any{}
	}
	output, err := executor.JSON(map[string]any{"columns": columns, "rows": rows, "row_count": len(rows), "truncated": result.Truncated})
	if err != nil {
		return executor.Result{}, err
	}
	return executor.Result{Output: output, Metrics: map[string]any{"rows": len(rows), "truncated": result.Truncated}}, nil
}
