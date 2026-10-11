package expr

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types/ref"
)

// Env declares the names available to workflow expressions. Dynamic input and
// output fields are checked at evaluation time until schema typing is added.
// WithScope adds static node access checks for the workflow compiler.
type Env struct {
	cel   *cel.Env
	scope *nodeScope
}

// Program is a compiled expression, reusable with different evaluation data.
type Program struct {
	cel        cel.Program
	outputType *cel.Type
}

type NodeView struct {
	Output  any
	Status  string
	Attempt int
	Metrics map[string]any
}

type RunView struct {
	ID            string
	CorrelationID string
	StartedAt     time.Time
	Attempt       int
}

// Bindings supplies evaluation data without reading a clock, environment, or DB.
// Callers must not mutate these maps while an evaluation is using them.
type Bindings struct {
	Input   map[string]any
	Nodes   map[string]NodeView
	Run     RunView
	Secrets map[string]string
	Now     time.Time
	// Item and Index are the current element of a foreach branch; they are nil
	// and 0 elsewhere, where the compiler forbids referring to them.
	Item  any
	Index int
}

func NewEnv() (*Env, error) {
	options := append([]cel.EnvOption{
		cel.Variable("input", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("nodes", cel.MapType(cel.StringType, cel.MapType(cel.StringType, cel.DynType))),
		cel.Variable("run", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("secrets", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("now", cel.TimestampType),
		cel.Variable("item", cel.DynType),
		cel.Variable("index", cel.IntType),
	}, library()...)
	environment, err := cel.NewEnv(options...)
	if err != nil {
		return nil, fmt.Errorf("create CEL environment: %w", err)
	}
	return &Env{cel: environment}, nil
}

// Compile accepts CEL text such as input.count + 1, without ${...} delimiters.
func (e *Env) Compile(source string) (*Program, error) {
	parsed, issues := e.cel.Parse(source)
	if issues.Err() != nil {
		return nil, &CompileError{"E_EXPR_SYNTAX", issues.Err().Error()}
	}
	if e.scope != nil {
		if err := e.scope.check(parsed.Expr()); err != nil {
			return nil, err
		}
	}
	ast, issues := e.cel.Check(parsed)
	if issues.Err() != nil {
		return nil, &CompileError{"E_EXPR_TYPE", issues.Err().Error()}
	}
	program, err := e.cel.Program(ast,
		cel.CostLimit(100_000),
		cel.InterruptCheckFrequency(100),
	)
	if err != nil {
		return nil, fmt.Errorf("build CEL program: %w", err)
	}
	return &Program{cel: program, outputType: ast.OutputType()}, nil
}

// Eval preserves the CEL result type, including lists, maps, timestamps and null.
// Conversion for template substitution belongs to the resolver. Runtime error
// details are suppressed because they may contain values from secret bindings.
func (p *Program) Eval(ctx context.Context, bindings Bindings) (ref.Val, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("evaluate CEL: %w", err)
	}
	nodes := make(map[string]any, len(bindings.Nodes))
	for id, node := range bindings.Nodes {
		nodes[id] = map[string]any{
			"output": node.Output, "status": node.Status,
			"attempt": node.Attempt, "metrics": node.Metrics,
		}
	}
	activation := map[string]any{
		"input": bindings.Input,
		"nodes": nodes,
		"run": map[string]any{
			"id": bindings.Run.ID, "correlation_id": bindings.Run.CorrelationID,
			"started_at": bindings.Run.StartedAt, "attempt": bindings.Run.Attempt,
		},
		"secrets": bindings.Secrets,
		"now":     bindings.Now,
		"item":    bindings.Item,
		"index":   int64(bindings.Index),
	}
	result, _, err := p.cel.ContextEval(ctx, activation)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("evaluate CEL: %w", ctx.Err())
		}
		return nil, errors.New("evaluate CEL expression failed")
	}
	return result, nil
}

// CompileCondition requires a whole-expression template whose result is boolean.
// Dynamic results are accepted at compile time and checked by EvalBool.
func (e *Env) CompileCondition(source string) (*Program, error) {
	parts, err := ParseTemplate(source)
	if err != nil {
		return nil, &CompileError{"E_EXPR_SYNTAX", err.Error()}
	}
	if len(parts) != 1 || !parts[0].IsExpression {
		return nil, &CompileError{"E_EXPR_TYPE", "if must be exactly one ${...} expression"}
	}
	program, err := e.Compile(parts[0].Text)
	if err != nil {
		return nil, err
	}
	if program.outputType != cel.BoolType && program.outputType != cel.DynType {
		return nil, &CompileError{"E_EXPR_TYPE", "if expression must return a boolean"}
	}
	return program, nil
}

func (p *Program) EvalBool(ctx context.Context, bindings Bindings) (bool, error) {
	result, err := p.Eval(ctx, bindings)
	if err != nil {
		return false, err
	}
	value, ok := result.Value().(bool)
	if !ok {
		return false, errors.New("condition did not return a boolean")
	}
	return value, nil
}

// CompileList requires a whole-expression template whose result is a list, as
// foreach.items does. Dynamic results are accepted at compile time and checked
// by EvalList.
func (e *Env) CompileList(source string) (*Program, error) {
	parts, err := ParseTemplate(source)
	if err != nil {
		return nil, &CompileError{"E_EXPR_SYNTAX", err.Error()}
	}
	if len(parts) != 1 || !parts[0].IsExpression {
		return nil, &CompileError{"E_EXPR_TYPE", "items must be exactly one ${...} expression"}
	}
	program, err := e.Compile(parts[0].Text)
	if err != nil {
		return nil, err
	}
	if program.outputType.Kind() != cel.ListKind && program.outputType != cel.DynType {
		return nil, &CompileError{"E_EXPR_TYPE", "items expression must return a list"}
	}
	return program, nil
}

// EvalList evaluates a CompileList program to its elements.
func (p *Program) EvalList(ctx context.Context, bindings Bindings) ([]any, error) {
	result, err := p.Eval(ctx, bindings)
	if err != nil {
		return nil, err
	}
	native, err := nativeValue(result)
	if err != nil {
		return nil, err
	}
	list, ok := native.([]any)
	if !ok {
		return nil, errors.New("expression did not return a list")
	}
	return list, nil
}
