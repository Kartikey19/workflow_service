package database

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/pkg/dbx"
	"github.com/kartikey/workflow_service/pkg/netguard"
)

// Insert adds rows to a table.
type Insert struct{ base }

var _ executor.Executor = (*Insert)(nil)

func NewInsert(conns executor.Connections, policy netguard.Policy) *Insert {
	return &Insert{base{conns, policy}}
}

func (*Insert) Type() string { return "db.insert" }

func (*Insert) Describe() executor.Description {
	return executor.Description{
		Title: "Insert into a database", Category: "integration",
		Description: "Inserts one row (`row`) or up to 1000 rows (`rows`) into a table of a PostgreSQL or MySQL connection, in one transaction. " +
			"Table and column names are validated and quoted; values are bound parameters. A retried attempt would insert again, so for " +
			"repeatable runs give the table a unique key and set `ignore_conflicts: true`. Use a database user that can only write where it should.",
		Example: `- id: record
  type: db.insert
  needs: [score]
  config:
    connection: warehouse
    table: analytics.scores
    row: {order_id: '${input.order_id}', score: '${nodes.score.output.value}', source: workflow}
    ignore_conflicts: true`,
		Output: `{"inserted": 1}`,
	}
}

func (*Insert) ValidateConfig(raw json.RawMessage) error {
	if err := executor.CheckFields(raw, []executor.Field{
		{Name: "connection", Kind: "string", Required: true},
		{Name: "table", Kind: "string", Required: true},
		{Name: "row", Kind: "object"},
		{Name: "rows", Kind: "array"},
		{Name: "ignore_conflicts", Kind: "bool"},
		{Name: "timeout", Kind: "duration"},
	}); err != nil {
		return err
	}
	var c map[string]any
	_ = json.Unmarshal(raw, &c)
	_, hasRow := c["row"]
	_, hasRows := c["rows"]
	if hasRow == hasRows {
		return errors.New("set exactly one of row and rows")
	}
	return nil
}

func (i *Insert) Execute(ctx context.Context, req executor.Request) (executor.Result, error) {
	var c struct {
		Connection      string           `json:"connection"`
		Table           string           `json:"table"`
		Row             map[string]any   `json:"row"`
		Rows            []map[string]any `json:"rows"`
		IgnoreConflicts bool             `json:"ignore_conflicts"`
		Timeout         string           `json:"timeout"`
	}
	if err := executor.DecodeConfig(req.Config, &c); err != nil {
		return executor.Result{}, err
	}
	rows := c.Rows
	if c.Row != nil {
		rows = []map[string]any{c.Row}
	}
	if len(rows) == 0 {
		return executor.Result{}, executor.Permanent("bad_config", errors.New("there are no rows to insert"))
	}
	timeout, err := timeoutOf(c.Timeout)
	if err != nil {
		return executor.Result{}, executor.Permanent("bad_config", err)
	}
	db, err := i.open(ctx, req, c.Connection)
	if err != nil {
		return executor.Result{}, err
	}
	defer db.Close()
	n, err := db.Insert(ctx, c.Table, rows, dbx.InsertOptions{Ignore: c.IgnoreConflicts, Timeout: timeout})
	if err != nil {
		return executor.Result{}, classify(ctx, err)
	}
	output, _ := json.Marshal(map[string]any{"inserted": n})
	return executor.Result{Output: output, Metrics: map[string]any{"rows": n, "requested": len(rows)}}, nil
}
