package agent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"uuid"
)

var mcpReference = regexp.MustCompile(`^mcp:[a-z0-9][a-z0-9_-]{0,62}$`)

// IsMCPReference identifies a saved server whose tools are discovered per attempt.
func IsMCPReference(name string) bool { return mcpReference.MatchString(name) }

// DynamicTools owns all sessions opened while discovering an attempt's tools.
// The returned cleanup must close them, including on a discovery failure.
type DynamicTools interface {
	Resolve(context.Context, uuid.UUID, []string) ([]Tool, func(), error)
}

func (e *Executor) WithDynamicTools(source DynamicTools) *Executor { e.dynamic = source; return e }

func (e *Executor) resolveTools(ctx context.Context, tenant uuid.UUID, refs []string) (*Tools, []string, func(), error) {
	var selected []Tool
	var remote []string
	seen := map[string]bool{}
	for _, ref := range refs {
		if seen[ref] {
			return nil, nil, func() {}, fmt.Errorf("duplicate tool reference %q", ref)
		}
		seen[ref] = true
		if IsMCPReference(ref) {
			remote = append(remote, ref)
			continue
		}
		tool, ok := e.tools.Get(ref)
		if !ok {
			return nil, nil, func() {}, fmt.Errorf("unknown tool %q", ref)
		}
		selected = append(selected, tool)
	}
	cleanup := func() {}
	if len(remote) > 0 {
		if e.dynamic == nil {
			return nil, nil, cleanup, errors.New("MCP tools are unavailable on this worker")
		}
		if len(remote) > 8 {
			return nil, nil, cleanup, errors.New("at most 8 MCP servers per agent")
		}
		tools, closeTools, err := e.dynamic.Resolve(ctx, tenant, remote)
		if closeTools != nil {
			cleanup = closeTools
		}
		if err != nil {
			cleanup()
			return nil, nil, func() {}, err
		}
		selected = append(selected, tools...)
	}
	registry, err := NewTools(selected...)
	if err != nil {
		cleanup()
		return nil, nil, func() {}, err
	}
	return registry, registry.Names(), cleanup, nil
}

func validateDatabaseTools(names []string, connection string) error {
	for _, name := range names {
		if (name == "list_tables" || name == "describe_table" || name == "run_query") && connection == "" {
			return errors.New("database tools require database_connection")
		}
	}
	return nil
}

func validateReferences(names []string) error {
	seen := map[string]bool{}
	remote := 0
	for _, name := range names {
		if seen[name] {
			return fmt.Errorf("duplicate tool reference %q", name)
		}
		seen[name] = true
		if IsMCPReference(name) {
			remote++
		}
	}
	if remote > 8 {
		return errors.New("at most 8 MCP servers per agent")
	}
	return nil
}
