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

// DynamicTools turns an agent's references into tools for one attempt, and owns
// all sessions opened while discovering them.
// The returned cleanup must close them, including on a discovery failure.
type DynamicTools interface {
	Resolve(context.Context, uuid.UUID, []string) ([]Tool, func(), error)
}

func (e *Executor) WithDynamicTools(source DynamicTools) *Executor { e.dynamic = source; return e }

// resolveTools builds one attempt's tools from the agent's references.
func (e *Executor) resolveTools(ctx context.Context, tenant uuid.UUID, refs []string) (*Tools, []string, func(), error) {
	if err := validateReferences(refs); err != nil {
		return nil, nil, func() {}, err
	}
	if len(refs) == 0 {
		return &Tools{}, nil, func() {}, nil
	}
	if e.dynamic == nil {
		return nil, nil, func() {}, errors.New("connector tools are unavailable on this worker")
	}
	tools, cleanup, err := e.dynamic.Resolve(ctx, tenant, refs)
	if cleanup == nil {
		cleanup = func() {}
	}
	if err != nil {
		cleanup()
		return nil, nil, func() {}, err
	}
	registry, err := NewTools(tools...)
	if err != nil {
		cleanup()
		return nil, nil, func() {}, err
	}
	return registry, registry.Names(), cleanup, nil
}

func validateReferences(names []string) error {
	if len(names) > 64 {
		return errors.New("at most 64 tool references per agent")
	}
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
