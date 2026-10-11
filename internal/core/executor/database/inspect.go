package database

import (
	"context"
	"encoding/json"
	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/pkg/netguard"
)

type Inspect struct {
	base
	describe bool
}

func NewInspect(conns executor.Connections, policy netguard.Policy, describe bool) *Inspect {
	return &Inspect{base{conns, policy}, describe}
}
func (i *Inspect) Type() string {
	if i.describe {
		return "db.describe_table"
	}
	return "db.list_tables"
}
func (i *Inspect) Describe() executor.Description {
	return executor.Description{Title: i.Type(), Category: "integration", Description: "Reads database schema through a saved PostgreSQL or MySQL connector.", Output: "Database tables or columns"}
}
func (i *Inspect) ValidateConfig(raw json.RawMessage) error {
	fields := []executor.Field{{Name: "connection", Kind: "string", Required: true}}
	if i.describe {
		fields = append(fields, executor.Field{Name: "table", Kind: "string", Required: true})
	}
	return executor.CheckFields(raw, fields)
}
func (i *Inspect) Execute(ctx context.Context, req executor.Request) (executor.Result, error) {
	var c struct {
		Connection string
		Table      string
	}
	if err := executor.DecodeConfig(req.Config, &c); err != nil {
		return executor.Result{}, err
	}
	db, err := i.open(ctx, req, c.Connection)
	if err != nil {
		return executor.Result{}, err
	}
	defer db.Close()
	var output json.RawMessage
	if i.describe {
		columns, err := db.Describe(ctx, c.Table)
		if err != nil {
			return executor.Result{}, classify(ctx, err)
		}
		output, err = json.Marshal(map[string]any{"columns": columns})
		if err != nil {
			return executor.Result{}, err
		}
	} else {
		tables, err := db.Tables(ctx)
		if err != nil {
			return executor.Result{}, classify(ctx, err)
		}
		output, err = json.Marshal(map[string]any{"tables": tables})
		if err != nil {
			return executor.Result{}, err
		}
	}
	return executor.Result{Output: output}, nil
}
