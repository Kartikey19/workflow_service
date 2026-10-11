package expr

import (
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
	"github.com/google/cel-go/ext"
)

// library is the closed function set available to workflow expressions (02 §3.3):
// the CEL standard library, pure extensions and sum(). Nothing here performs I/O,
// reads a clock or the environment; each addition is permanent.
func library() []cel.EnvOption {
	return []cel.EnvOption{
		ext.Strings(),
		ext.Encoders(),
		ext.Math(),
		ext.Lists(),
		ext.Sets(),
		cel.OptionalTypes(),
		cel.Function("sum",
			cel.MemberOverload("list_sum", []*cel.Type{cel.ListType(cel.DynType)}, cel.DynType,
				cel.UnaryBinding(sumList))),
	}
}

// sumList adds numbers; the result is an int unless any element is a double.
func sumList(value ref.Val) ref.Val {
	list, ok := value.(traits.Lister)
	if !ok {
		return types.NewErr("sum() requires a list")
	}
	var intTotal int64
	var floatTotal float64
	isFloat := false
	iterator := list.Iterator()
	for iterator.HasNext() == types.True {
		switch item := iterator.Next().(type) {
		case types.Int:
			intTotal += int64(item)
			floatTotal += float64(item)
		case types.Uint:
			intTotal += int64(item)
			floatTotal += float64(item)
		case types.Double:
			isFloat = true
			floatTotal += float64(item)
		default:
			return types.NewErr("sum() requires a list of numbers")
		}
	}
	if isFloat {
		return types.Double(floatTotal)
	}
	return types.Int(intTotal)
}
