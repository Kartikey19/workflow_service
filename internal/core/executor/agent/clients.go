package agent

import (
	"context"
	"errors"
	"uuid"

	"github.com/kartikey/workflow_service/pkg/llm"
)

// ErrNotConfigured means no model provider is available for the request; the
// node fails permanently because retrying cannot help.
var ErrNotConfigured = errors.New("no model provider is configured")

// Resolved is the provider chosen for one agent call.
type Resolved struct {
	Client   llm.Client
	Provider string // metric label, e.g. "anthropic"
	Pricing  llm.Pricing
}

// Clients picks the provider client for a tenant and model. Platform-wide keys
// are the simple case; per-organization connections (M13) plug in here.
type Clients interface {
	Client(ctx context.Context, tenantID uuid.UUID, model string) (Resolved, error)
}

type static struct{ resolved Resolved }

// NewStatic always returns the same client, for platform-managed keys.
func NewStatic(provider string, client llm.Client, pricing llm.Pricing) Clients {
	return static{Resolved{Client: client, Provider: provider, Pricing: pricing}}
}

func (s static) Client(context.Context, uuid.UUID, string) (Resolved, error) {
	return s.resolved, nil
}

// Unconfigured fails every lookup with ErrNotConfigured, so workflows with
// agent nodes still publish and lint on an installation without a model key.
type Unconfigured struct{}

func (Unconfigured) Client(context.Context, uuid.UUID, string) (Resolved, error) {
	return Resolved{}, ErrNotConfigured
}
