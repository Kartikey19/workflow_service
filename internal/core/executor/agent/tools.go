package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
	"uuid"
)

// Call identifies the node execution on whose behalf a tool runs, so a tool
// can scope its work to the tenant and attribute it to a run.
type Call struct {
	TenantID uuid.UUID
	RunID    uuid.UUID
	NodeID   string
	// AllowedHosts is the agent's allow-list for tools that reach the network.
	AllowedHosts []string
}

// Tool is a function an agent may call. Tools run inside the agent's attempt:
// they share its timeout and must be safe to run again if the attempt retries.
type Tool interface {
	Name() string
	Description() string
	// InputSchema is the JSON Schema object the model fills in.
	InputSchema() json.RawMessage
	// Call returns the JSON result shown to the model. An error is reported to
	// the model as a failed tool call; it does not fail the node.
	Call(ctx context.Context, call Call, input json.RawMessage) (json.RawMessage, error)
}

// Tools is the registry that `tools:` in an agent node refers to. It is built
// at startup and read-only afterwards.
type Tools struct{ byName map[string]Tool }

// NewTools builds a registry; a duplicate name is a programming error.
func NewTools(tools ...Tool) (*Tools, error) {
	registry := &Tools{byName: make(map[string]Tool, len(tools))}
	for _, tool := range tools {
		if tool.Name() == "" {
			return nil, fmt.Errorf("agent tool has no name")
		}
		if _, exists := registry.byName[tool.Name()]; exists {
			return nil, fmt.Errorf("agent tool %q is registered twice", tool.Name())
		}
		registry.byName[tool.Name()] = tool
	}
	return registry, nil
}

// Get returns the named tool.
func (t *Tools) Get(name string) (Tool, bool) {
	if t == nil {
		return nil, false
	}
	tool, ok := t.byName[name]
	return tool, ok
}

// Names lists the registered tool names, sorted.
func (t *Tools) Names() []string {
	if t == nil {
		return nil
	}
	names := make([]string, 0, len(t.byName))
	for name := range t.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// CurrentTime is a harmless built-in tool: models cannot know the date.
type CurrentTime struct{ Now func() time.Time }

func (CurrentTime) Name() string { return "current_time" }
func (CurrentTime) Description() string {
	return "Returns the current date and time in UTC (RFC 3339)."
}
func (CurrentTime) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (c CurrentTime) Call(context.Context, Call, json.RawMessage) (json.RawMessage, error) {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	return json.Marshal(map[string]string{"now": now().UTC().Format(time.RFC3339)})
}
