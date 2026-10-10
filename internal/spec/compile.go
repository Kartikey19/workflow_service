package spec

import (
	"errors"
	"fmt"
	"sort"

	"github.com/kartikey/workflow_service/internal/executor"
	"github.com/kartikey/workflow_service/internal/expr"
)

// Options configures the compiler stages that depend on the running binary.
type Options struct {
	// Registry, when set, rejects unregistered node types (E_UNKNOWN_TYPE) and
	// runs each executor's ValidateConfig on the raw config (E_CONFIG).
	Registry *executor.Registry
}

// Compile prepares workflow structure and expressions without executing them.
// Diagnostics retain original YAML node indexes; normalized nodes are sorted only
// after validation. It does not check node types; publication uses CompileWith.
func Compile(source []byte) (*Compiled, []Diagnostic) {
	return CompileWith(source, Options{})
}

// CompileWith is Compile plus the executor stage (02 §6.1 step 6).
func CompileWith(source []byte, opts Options) (*Compiled, []Diagnostic) {
	document, err := Decode(source)
	if err != nil {
		return nil, []Diagnostic{{Severity: "error", Code: "E_YAML", Message: err.Error()}}
	}
	ApplyDefaults(document)
	diagnostics := Validate(document)
	// A no-root graph is still traversed to report the actual cycle path.
	for _, d := range diagnostics {
		if d.Severity == "error" && d.Code != "E_NO_ROOT" {
			return nil, diagnostics
		}
	}
	graph, graphDiagnostics := BuildGraph(document)
	diagnostics = append(diagnostics, graphDiagnostics...)
	if hasErrors(diagnostics) {
		return nil, diagnostics
	}
	diagnostics = append(diagnostics, validateInputSchema(document.Inputs)...)
	diagnostics = append(diagnostics, validateTargets(document)...)
	diagnostics = append(diagnostics, validateExecutors(document, opts.Registry)...)
	diagnostics = append(diagnostics, branchJoinWarnings(document, graph)...)
	environment, err := expr.NewEnv()
	if err != nil {
		diagnostics = append(diagnostics, Diagnostic{Severity: "error", Code: "E_COMPILER", Message: err.Error()})
		return nil, diagnostics
	}
	programs := Programs{Nodes: make(map[string]NodePrograms)}
	compileValue := func(env *expr.Env, value any, nodeID, path string) *expr.Template {
		template, issues := env.CompileTemplateDiagnostics(value, path)
		for _, issue := range issues {
			diagnostics = append(diagnostics, Diagnostic{Severity: "error", Code: issue.Code, Message: issue.Message, NodeID: nodeID, Path: issue.Path})
		}
		return template
	}
	for i, node := range document.Nodes {
		scoped := environment.WithScope(expr.Scope{NodeID: node.ID, AllowedNodes: graph.Transitive[node.ID]})
		path := fmt.Sprintf("nodes[%d]", i)
		compiled := NodePrograms{Config: compileValue(scoped, node.Config, node.ID, path+".config")}
		if node.If != "" {
			compiled.If, err = scoped.CompileCondition(node.If)
			if err != nil {
				code := "E_EXPR_TYPE"
				var compileErr *expr.CompileError
				if errors.As(err, &compileErr) {
					code = compileErr.Code
				}
				diagnostics = append(diagnostics, Diagnostic{Severity: "error", Code: code, Message: err.Error(), NodeID: node.ID, Path: path + ".if"})
			}
		}
		programs.Nodes[node.ID] = compiled
	}
	if document.Correlation != "" {
		programs.Correlation = compileValue(environment.WithScope(expr.Scope{}), document.Correlation, "", "correlation")
	}
	programs.Outputs = compileValue(environment.WithScope(expr.Scope{AllowedNodes: graph.Order}), document.Outputs, "", "outputs")
	if hasErrors(diagnostics) {
		return nil, diagnostics
	}

	timeout, _ := positiveDuration(document.Timeout)
	compiled := &Compiled{Programs: programs, Spec: Spec{
		APIVersion: document.APIVersion, Name: document.Name, Description: document.Description,
		Inputs: document.Inputs, TimeoutNS: int64(timeout), Correlation: document.Correlation,
		Edges: graph.Children, ReverseEdges: graph.Dependencies, Roots: graph.Roots,
		TopoOrder: graph.Order, Outputs: document.Outputs, Nodes: make([]NodeSpec, 0, len(document.Nodes)),
	}}
	for _, node := range document.Nodes {
		initial, _ := positiveDuration(node.Retry.Initial)
		maximum, _ := positiveDuration(node.Retry.Max)
		normalized := NodeSpec{
			ID: node.ID, Type: node.Type, Needs: graph.Dependencies[node.ID], Join: *node.Join,
			If: node.If, Config: node.Config, OnFailure: node.OnFailure, OnTimeout: node.OnTimeout,
			Retry: RetrySpec{MaxAttempts: *node.Retry.MaxAttempts, Backoff: *node.Retry.Backoff,
				InitialNS: int64(initial), MaxNS: int64(maximum), Jitter: *node.Retry.Jitter, RetryOn: node.Retry.RetryOn},
		}
		if node.Timeout != nil {
			duration, _ := positiveDuration(node.Timeout)
			ns := int64(duration)
			normalized.TimeoutNS = &ns
		}
		compiled.Spec.Nodes = append(compiled.Spec.Nodes, normalized)
	}
	sort.Slice(compiled.Spec.Nodes, func(i, j int) bool { return compiled.Spec.Nodes[i].ID < compiled.Spec.Nodes[j].ID })
	return compiled, diagnostics
}

func hasErrors(diagnostics []Diagnostic) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == "error" {
			return true
		}
	}
	return false
}
