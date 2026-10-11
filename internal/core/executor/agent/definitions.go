package agent

import (
	"context"
	"encoding/json"
	"errors"
	"uuid"
)

// ErrAgentNotFound means a node refers to an agent the organization has not
// defined.
var ErrAgentNotFound = errors.New("agent not found")

// Definition is a stored, reusable agent: which provider connection and model,
// the system prompt, the shape of its input and output, and its tools and
// limits. A workflow node runs it with `config: {agent: <name>, input: {...}}`.
type Definition struct {
	Name          string
	Revision      int
	Connection    string // provider connection name; empty means the platform provider
	Model         string
	System        string
	InputSchema   json.RawMessage // optional
	OutputSchema  json.RawMessage // optional
	Tools         []string        // connectors to search, or mcp:<server>
	MaxIterations int
	MaxTokens     int
	Temperature   *float64
}

// Definitions loads an organization's agents. The executor never imports the
// store; the agents service implements this.
type Definitions interface {
	Definition(ctx context.Context, tenantID uuid.UUID, name string) (Definition, error)
}
