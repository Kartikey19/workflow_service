package spec

import (
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/kartikey/workflow_service/internal/core/expr"
)

// wholeInputPattern matches an expression that passes the input map around
// whole (for example ${input} or has(input.x) over a dynamic key): every
// declared input then counts as used.
var wholeInputPattern = regexp.MustCompile(`(^|[^.\w])input\s*($|[^.\[\w\s])`)

// unusedInputWarnings reports declared inputs that no expression references
// (W_UNUSED_INPUT). Undeclared inputs still pass through for webhook payloads,
// so this is a warning: the input may be read by a caller, not the workflow.
func unusedInputWarnings(document *Document) []Diagnostic {
	if len(document.Inputs) == 0 {
		return nil
	}
	var expressions []string
	collect := func(value any) {
		walkStrings(value, func(text string) {
			segments, err := expr.ParseTemplate(text)
			if err != nil {
				return
			}
			for _, segment := range segments {
				if segment.IsExpression {
					expressions = append(expressions, segment.Text)
				}
			}
		})
	}
	for _, node := range document.Nodes {
		collect(node.Config)
		if node.If != "" {
			collect(node.If)
		}
	}
	collect(document.Correlation)
	collect(document.Outputs)

	used := make(map[string]bool, len(document.Inputs))
	for _, expression := range expressions {
		if wholeInputPattern.MatchString(expression) {
			return nil
		}
		for name := range document.Inputs {
			reference := regexp.MustCompile(`(^|[^.\w])input\s*(\.\s*` + regexp.QuoteMeta(name) + `\b|\[\s*["']` + regexp.QuoteMeta(name) + `["']\s*\])`)
			if reference.MatchString(expression) {
				used[name] = true
			}
		}
	}
	names := make([]string, 0, len(document.Inputs))
	for name := range document.Inputs {
		if !used[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	diagnostics := make([]Diagnostic, 0, len(names))
	for _, name := range names {
		diagnostics = append(diagnostics, Diagnostic{
			Severity: "warning", Code: "W_UNUSED_INPUT", Path: "inputs." + name,
			Message: fmt.Sprintf("input %q is declared but never referenced", name),
		})
	}
	return diagnostics
}

// longSleepWarnings reports a literal sleep duration longer than the run
// timeout (W_LONG_SLEEP): the run deadline will fail the run first.
func longSleepWarnings(document *Document) []Diagnostic {
	runTimeout, ok := positiveDuration(document.Timeout)
	if !ok {
		return nil
	}
	var diagnostics []Diagnostic
	for i, node := range document.Nodes {
		if node.Type != "sleep" {
			continue
		}
		text, ok := node.Config["duration"].(string)
		if !ok {
			continue
		}
		duration, err := time.ParseDuration(text)
		if err != nil || duration <= runTimeout {
			continue
		}
		diagnostics = append(diagnostics, Diagnostic{
			Severity: "warning", Code: "W_LONG_SLEEP", NodeID: node.ID,
			Path:    fmt.Sprintf("nodes[%d].config.duration", i),
			Message: fmt.Sprintf("sleep of %s is longer than the run timeout %s; the run deadline fails the run first", duration, runTimeout),
		})
	}
	return diagnostics
}

func walkStrings(value any, visit func(string)) {
	switch v := value.(type) {
	case string:
		visit(v)
	case map[string]any:
		for _, item := range v {
			walkStrings(item, visit)
		}
	case []any:
		for _, item := range v {
			walkStrings(item, visit)
		}
	}
}
