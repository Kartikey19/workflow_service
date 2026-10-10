package spec

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

var (
	workflowNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}[a-z0-9]$`)
	nodeIDPattern       = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
)

// Validate collects structural diagnostics without modifying the document.
// Call ApplyDefaults first. Graph, expression, input-schema, and registered
// executor/config validation belong to later compiler stages.
func Validate(document *Document) []Diagnostic {
	var diagnostics []Diagnostic
	add := func(code, message, nodeID, path string) {
		diagnostics = append(diagnostics, Diagnostic{
			Severity: "error", Code: code, Message: message, NodeID: nodeID, Path: path,
		})
	}
	if document == nil {
		add("E_DOCUMENT", "workflow document is required", "", "")
		return diagnostics
	}
	if document.APIVersion != "workflow/v1" {
		add("E_API_VERSION", "apiVersion must be workflow/v1", "", "apiVersion")
	}
	if !workflowNamePattern.MatchString(document.Name) {
		add("E_NAME_FORMAT", "name must contain 2–64 lowercase letters, digits or hyphens, and start and end with a letter or digit", "", "name")
	}
	if len(document.Nodes) == 0 {
		add("E_NO_NODES", "workflow must contain at least one node", "", "nodes")
	}

	runTimeout, runOK := positiveDuration(document.Timeout)
	if !runOK {
		add("E_TIMEOUT_RANGE", "run timeout must be a positive Go duration", "", "timeout")
	}
	checkTimeout := func(value *string, nodeID, path string) {
		if value == nil {
			return
		}
		duration, ok := positiveDuration(value)
		if !ok {
			add("E_TIMEOUT_RANGE", "timeout must be a positive Go duration", nodeID, path)
		} else if runOK && duration > runTimeout {
			add("E_TIMEOUT_RANGE", "timeout must not exceed the run timeout", nodeID, path)
		}
	}
	checkTimeout(document.Defaults.Timeout, "", "defaults.timeout")
	diagnostics = append(diagnostics, validateRetry(document.Defaults.Retry, "", "defaults.retry")...)

	// Collect all IDs before resolving references, including forward references.
	known := make(map[string]bool)
	roots := 0
	for i, node := range document.Nodes {
		path := fmt.Sprintf("nodes[%d]", i)
		if !nodeIDPattern.MatchString(node.ID) {
			add("E_NODE_ID_FORMAT", "node ID must start with a lowercase letter and contain at most 63 lowercase letters, digits or underscores", node.ID, path+".id")
		}
		if known[node.ID] {
			add("E_DUP_NODE_ID", fmt.Sprintf("duplicate node ID %q", node.ID), node.ID, path+".id")
		}
		known[node.ID] = true
		if len(node.Needs) == 0 {
			roots++
		}
		if strings.TrimSpace(node.Type) == "" {
			add("E_UNKNOWN_TYPE", "node type is required", node.ID, path+".type")
		}
		if node.Config == nil {
			add("E_CONFIG", "config is required; use {} for empty configuration", node.ID, path+".config")
		}
		if node.Join == nil || (*node.Join != "all" && *node.Join != "any") {
			add("E_JOIN", "join must be all or any", node.ID, path+".join")
		}
		checkTimeout(node.Timeout, node.ID, path+".timeout")
		if node.Type == "http" && node.Timeout == nil {
			diagnostics = append(diagnostics, Diagnostic{
				Severity: "warning", Code: "W_NO_TIMEOUT", Message: "HTTP node has no timeout",
				NodeID: node.ID, Path: path + ".timeout",
			})
		}
		diagnostics = append(diagnostics, validateRetry(node.Retry, node.ID, path+".retry")...)
	}
	if len(document.Nodes) > 0 && roots == 0 {
		add("E_NO_ROOT", "workflow must have a node with no dependencies", "", "nodes")
	}

	for i, node := range document.Nodes {
		path := fmt.Sprintf("nodes[%d]", i)
		checkReference := func(target, field string) {
			if !known[target] {
				add("E_UNKNOWN_NODE", fmt.Sprintf("referenced node %q does not exist", target), node.ID, field)
			}
		}
		seen := make(map[string]bool)
		for j, dependency := range node.Needs {
			field := fmt.Sprintf("%s.needs[%d]", path, j)
			checkReference(dependency, field)
			if seen[dependency] {
				add("E_DUP_NEED", fmt.Sprintf("dependency %q is repeated", dependency), node.ID, field)
			}
			seen[dependency] = true
		}
		if node.OnFailure != "" {
			checkReference(node.OnFailure, path+".on_failure")
		}
		if node.OnTimeout != "" {
			checkReference(node.OnTimeout, path+".on_timeout")
			if node.Type != "wait_event" && node.Type != "sleep" {
				add("E_ON_TIMEOUT", "on_timeout is only supported by wait_event and sleep", node.ID, path+".on_timeout")
			}
		}
	}
	return diagnostics
}

func positiveDuration(value *string) (time.Duration, bool) {
	if value == nil {
		return 0, false
	}
	duration, err := time.ParseDuration(*value)
	return duration, err == nil && duration > 0
}

func validateRetry(retry RetryPolicy, nodeID, path string) []Diagnostic {
	var diagnostics []Diagnostic
	add := func(field, message string) {
		diagnostics = append(diagnostics, Diagnostic{
			Severity: "error", Code: "E_RETRY_RANGE", Message: message,
			NodeID: nodeID, Path: path + "." + field,
		})
	}
	if retry.MaxAttempts != nil && (*retry.MaxAttempts < 1 || *retry.MaxAttempts > 20) {
		add("max_attempts", "max_attempts must be between 1 and 20")
	}
	if retry.Backoff != nil {
		switch *retry.Backoff {
		case "exponential", "linear", "fixed":
		default:
			add("backoff", "backoff must be exponential, linear or fixed")
		}
	}
	initial, initialOK := positiveDuration(retry.Initial)
	maximum, maximumOK := positiveDuration(retry.Max)
	if retry.Initial != nil && !initialOK {
		add("initial", "initial must be a positive Go duration")
	}
	if retry.Max != nil && !maximumOK {
		add("max", "max must be a positive Go duration")
	}
	if initialOK && maximumOK && initial > maximum {
		add("initial", "initial must not exceed max")
	}
	if retry.Jitter != nil && (math.IsNaN(*retry.Jitter) || *retry.Jitter < 0 || *retry.Jitter > 1) {
		add("jitter", "jitter must be between 0 and 1")
	}
	return diagnostics
}
