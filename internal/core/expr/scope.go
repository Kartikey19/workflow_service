package expr

import (
	"fmt"

	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
)

// Scope limits node references to statically known workflow dependencies.
type Scope struct {
	NodeID       string
	AllowedNodes []string
	// Foreach permits item and index, which only exist inside a foreach node.
	Foreach bool
}

type CompileError struct{ Code, Message string }

func (e *CompileError) Error() string { return e.Message }

func (e *Env) WithScope(scope Scope) *Env {
	allowed := make(map[string]bool, len(scope.AllowedNodes))
	for _, id := range scope.AllowedNodes {
		allowed[id] = true
	}
	return &Env{cel: e.cel, scope: &nodeScope{current: scope.NodeID, allowed: allowed, foreach: scope.Foreach}}
}

type nodeScope struct {
	current string
	allowed map[string]bool
	foreach bool
}

func (s *nodeScope) check(expression *exprpb.Expr) error {
	if !s.foreach {
		if err := checkForeachNames(expression, nil); err != nil {
			return err
		}
	}
	return s.walk(expression, false)
}

// checkForeachNames rejects item and index outside a foreach node. A
// comprehension variable of the same name (xs.map(item, item.id)) is a local
// binding, not the foreach element, so it shadows only within its own loop.
func checkForeachNames(e *exprpb.Expr, shadow map[string]bool) error {
	if e == nil {
		return nil
	}
	walk := func(child *exprpb.Expr) error { return checkForeachNames(child, shadow) }
	switch value := e.ExprKind.(type) {
	case *exprpb.Expr_IdentExpr:
		name := value.IdentExpr.Name
		if (name == "item" || name == "index") && !shadow[name] {
			return &CompileError{"E_EXPR_SCOPE", fmt.Sprintf("%s is only available in a foreach node", name)}
		}
	case *exprpb.Expr_SelectExpr:
		return walk(value.SelectExpr.Operand)
	case *exprpb.Expr_CallExpr:
		if err := walk(value.CallExpr.Target); err != nil {
			return err
		}
		for _, arg := range value.CallExpr.Args {
			if err := walk(arg); err != nil {
				return err
			}
		}
	case *exprpb.Expr_ListExpr:
		for _, element := range value.ListExpr.Elements {
			if err := walk(element); err != nil {
				return err
			}
		}
	case *exprpb.Expr_StructExpr:
		for _, entry := range value.StructExpr.Entries {
			if err := walk(entry.GetMapKey()); err != nil {
				return err
			}
			if err := walk(entry.Value); err != nil {
				return err
			}
		}
	case *exprpb.Expr_ComprehensionExpr:
		c := value.ComprehensionExpr
		if err := walk(c.IterRange); err != nil {
			return err
		}
		if err := walk(c.AccuInit); err != nil {
			return err
		}
		inner := make(map[string]bool, len(shadow)+3)
		for name := range shadow {
			inner[name] = true
		}
		for _, name := range []string{c.IterVar, c.IterVar2, c.AccuVar} {
			inner[name] = true
		}
		for _, part := range []*exprpb.Expr{c.LoopCondition, c.LoopStep, c.Result} {
			if err := checkForeachNames(part, inner); err != nil {
				return err
			}
		}
	}
	return nil
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
