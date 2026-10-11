package expr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
)

// Template is a compiled configuration value, reusable across workflow runs.
// Only source strings are parsed; values returned by CEL are never parsed again.
type Template struct {
	literal any
	parts   []templatePart
	object  map[string]*Template
	list    []*Template
}
type templatePart struct {
	text    string
	program *Program
}

// CompileTemplate parses and compiles strings throughout a decoded configuration.
// Object keys are literal. Syntax errors are reported before any evaluation.
func (e *Env) CompileTemplate(value any) (*Template, error) {
	result, issues := e.CompileTemplateDiagnostics(value, "")
	if len(issues) != 0 {
		return nil, fmt.Errorf("%s: %w", issues[0].Path, &CompileError{issues[0].Code, issues[0].Message})
	}
	return result, nil
}

// Resolve evaluates a previously compiled template without modifying its input.
// Whole expressions preserve their types. Interpolation uses strings directly,
// compact JSON for other JSON values, and textual timestamps/durations.
func (t *Template) Resolve(ctx context.Context, bindings Bindings) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if t.object != nil {
		out := make(map[string]any, len(t.object))
		keys := make([]string, 0, len(t.object))
		for key := range t.object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value, err := t.object[key].Resolve(ctx, bindings)
			if err != nil {
				return nil, fmt.Errorf("field %q: %w", key, err)
			}
			out[key] = value
		}
		return out, nil
	}
	if t.list != nil {
		out := make([]any, len(t.list))
		for i, item := range t.list {
			value, err := item.Resolve(ctx, bindings)
			if err != nil {
				return nil, fmt.Errorf("element %d: %w", i, err)
			}
			out[i] = value
		}
		return out, nil
	}
	if t.parts == nil {
		return t.literal, nil
	}
	if len(t.parts) == 1 && t.parts[0].program != nil {
		result, err := t.parts[0].program.Eval(ctx, bindings)
		if err != nil {
			return nil, err
		}
		return nativeValue(result)
	}
	var out strings.Builder
	for _, part := range t.parts {
		if part.program == nil {
			out.WriteString(part.text)
			continue
		}
		result, err := part.program.Eval(ctx, bindings)
		if err != nil {
			return nil, err
		}
		value, err := nativeValue(result)
		if err != nil {
			return nil, err
		}
		switch v := value.(type) {
		case string:
			out.WriteString(v)
		case time.Time:
			out.WriteString(v.Format(time.RFC3339Nano))
		case time.Duration:
			out.WriteString(v.String())
		default:
			encoded, err := json.Marshal(v)
			if err != nil {
				return nil, errors.New("expression result cannot be interpolated as JSON")
			}
			out.Write(encoded)
		}
	}
	return out.String(), nil
}

// Resolve is convenient for one-off values. For repeated runs, compile once with
// Env.CompileTemplate and reuse Template.Resolve instead.
func Resolve(ctx context.Context, value any, env *Env, bindings Bindings) (any, error) {
	template, err := env.CompileTemplate(value)
	if err != nil {
		return nil, err
	}
	return template.Resolve(ctx, bindings)
}

func sortedKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Convert CEL containers recursively; Value() alone can retain CEL wrappers.
func nativeValue(value ref.Val) (any, error) {
	switch v := value.(type) {
	case types.Null:
		return nil, nil
	case types.Bool, types.Int, types.Uint, types.Double, types.String, types.Timestamp, types.Duration:
		return v.Value(), nil
	case types.Bytes:
		return append([]byte{}, v...), nil
	case traits.Lister:
		out := []any{}
		iterator := v.Iterator()
		for iterator.HasNext() == types.True {
			item, err := nativeValue(iterator.Next())
			if err != nil {
				return nil, err
			}
			out = append(out, item)
		}
		return out, nil
	case traits.Mapper:
		out := make(map[string]any)
		iterator := v.Iterator()
		for iterator.HasNext() == types.True {
			key := iterator.Next()
			text, ok := key.(types.String)
			if !ok {
				return nil, errors.New("expression object keys must be strings")
			}
			item, err := nativeValue(v.Get(key))
			if err != nil {
				return nil, err
			}
			out[string(text)] = item
		}
		return out, nil
	default:
		return nil, errors.New("unsupported expression result type")
	}
}
