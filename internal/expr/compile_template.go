package expr

import (
	"errors"
	"fmt"
	"strconv"
)

// Issue is independent of workflow diagnostics so expr does not depend on spec.
type Issue struct{ Code, Message, Path string }

// CompileTemplateDiagnostics collects failures across independent fields and
// segments. No partially compiled template is returned if there are errors.
func (e *Env) CompileTemplateDiagnostics(value any, path string) (*Template, []Issue) {
	result := &Template{}
	var issues []Issue
	add := func(err error) {
		code := "E_EXPR_SYNTAX"
		var compileErr *CompileError
		if errors.As(err, &compileErr) {
			code = compileErr.Code
		}
		issues = append(issues, Issue{code, err.Error(), path})
	}
	switch v := value.(type) {
	case map[string]any:
		result.object = make(map[string]*Template, len(v))
		for _, key := range sortedKeys(v) {
			child, found := e.CompileTemplateDiagnostics(v[key], path+"["+strconv.Quote(key)+"]")
			issues = append(issues, found...)
			result.object[key] = child
		}
	case []any:
		result.list = make([]*Template, len(v))
		for i, item := range v {
			child, found := e.CompileTemplateDiagnostics(item, fmt.Sprintf("%s[%d]", path, i))
			issues = append(issues, found...)
			result.list[i] = child
		}
	case string:
		segments, err := ParseTemplate(v)
		if err != nil {
			add(err)
			break
		}
		for _, segment := range segments {
			part := templatePart{text: segment.Text}
			if segment.IsExpression {
				part.program, err = e.Compile(segment.Text)
				if err != nil {
					add(err)
				}
			}
			result.parts = append(result.parts, part)
		}
	default:
		result.literal = value
	}
	if len(issues) != 0 {
		return nil, issues
	}
	return result, nil
}
