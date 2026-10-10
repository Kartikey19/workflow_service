package expr

import (
	"fmt"

	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
)

// Scope limits node references to statically known workflow dependencies.
type Scope struct {
	NodeID       string
	AllowedNodes []string
}

type CompileError struct{ Code, Message string }

func (e *CompileError) Error() string { return e.Message }

func (e *Env) WithScope(scope Scope) *Env {
	allowed := make(map[string]bool, len(scope.AllowedNodes))
	for _, id := range scope.AllowedNodes {
		allowed[id] = true
	}
	return &Env{cel: e.cel, scope: &nodeScope{current: scope.NodeID, allowed: allowed}}
}

type nodeScope struct {
	current string
	allowed map[string]bool
}

func (s *nodeScope) check(expression *exprpb.Expr) error {
	return s.walk(expression, false)
}

// A local comprehension variable named nodes shadows the global binding only in
// its loop expressions; the range and accumulator initializer use outer scope.
func (s *nodeScope) walk(e *exprpb.Expr, shadowed bool) error {
	if e == nil {
		return nil
	}
	root := func(e *exprpb.Expr) bool {
		name := e.GetIdentExpr().GetName()
		return name == ".nodes" || (!shadowed && name == "nodes")
	}
	checkID := func(id string) error {
		if id == s.current && s.current != "" {
			return &CompileError{"E_SELF_REF", "node cannot reference itself"}
		}
		if !s.allowed[id] {
			return &CompileError{"E_EXPR_SCOPE", fmt.Sprintf("node %q is not available in this expression", id)}
		}
		return nil
	}
	if root(e) {
		return &CompileError{"E_EXPR_SCOPE", "nodes must be accessed using a literal node ID"}
	}
	switch value := e.ExprKind.(type) {
	case *exprpb.Expr_SelectExpr:
		if root(value.SelectExpr.Operand) {
			return checkID(value.SelectExpr.Field)
		}
		return s.walk(value.SelectExpr.Operand, shadowed)
	case *exprpb.Expr_CallExpr:
		call := value.CallExpr
		if call.Function == "_[_]" && len(call.Args) == 2 && root(call.Args[0]) {
			key, ok := call.Args[1].GetConstExpr().GetConstantKind().(*exprpb.Constant_StringValue)
			if !ok {
				return &CompileError{"E_EXPR_SCOPE", "dynamic node lookups are not allowed"}
			}
			return checkID(key.StringValue)
		}
		if err := s.walk(call.Target, shadowed); err != nil {
			return err
		}
		for _, arg := range call.Args {
			if err := s.walk(arg, shadowed); err != nil {
				return err
			}
		}
	case *exprpb.Expr_ListExpr:
		for _, item := range value.ListExpr.Elements {
			if err := s.walk(item, shadowed); err != nil {
				return err
			}
		}
	case *exprpb.Expr_StructExpr:
		for _, entry := range value.StructExpr.Entries {
			if err := s.walk(entry.GetMapKey(), shadowed); err != nil {
				return err
			}
			if err := s.walk(entry.Value, shadowed); err != nil {
				return err
			}
		}
	case *exprpb.Expr_ComprehensionExpr:
		c := value.ComprehensionExpr
		if err := s.walk(c.IterRange, shadowed); err != nil {
			return err
		}
		if err := s.walk(c.AccuInit, shadowed); err != nil {
			return err
		}
		loopShadowed := shadowed || c.IterVar == "nodes" || c.IterVar2 == "nodes" || c.AccuVar == "nodes"
		if err := s.walk(c.LoopCondition, loopShadowed); err != nil {
			return err
		}
		if err := s.walk(c.LoopStep, loopShadowed); err != nil {
			return err
		}
		return s.walk(c.Result, shadowed || c.AccuVar == "nodes")
	}
	return nil
}
