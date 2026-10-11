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

// ErrConnectionNotFound means a named provider connection does not exist.
var ErrConnectionNotFound = errors.New("provider connection not found")

// Resolved is the provider chosen for one agent call.
type Resolved struct {
	Client   llm.Client
	Provider string // metric label, e.g. "anthropic"
	Pricing  llm.Pricing
}

// ProviderRef says which provider to call: a named connection of the
// organization, or (Connection empty) the platform's own provider.
type ProviderRef struct {
	Connection string
	Model      string
}

// Clients picks the provider client for a tenant. Platform-wide keys are the
// simple case; per-organization connections plug in through Router.
type Clients interface {
	Client(ctx context.Context, tenantID uuid.UUID, ref ProviderRef) (Resolved, error)
}

type static struct{ resolved Resolved }

// NewStatic always returns the same client, for platform-managed keys.
func NewStatic(provider string, client llm.Client, pricing llm.Pricing) Clients {
	return static{Resolved{Client: client, Provider: provider, Pricing: pricing}}
}

func (s static) Client(context.Context, uuid.UUID, ProviderRef) (Resolved, error) {
	return s.resolved, nil
}

// Unconfigured fails every lookup with ErrNotConfigured, so workflows with
// agent nodes still publish and lint on an installation without a model key.
type Unconfigured struct{}

func (Unconfigured) Client(context.Context, uuid.UUID, ProviderRef) (Resolved, error) {
	return Resolved{}, ErrNotConfigured
}

// Router sends a named connection to Named and everything else to Default.
type Router struct {
	Named   Clients
	Default Clients
}

func (r Router) Client(ctx context.Context, tenantID uuid.UUID, ref ProviderRef) (Resolved, error) {
	if ref.Connection != "" && r.Named != nil {
		return r.Named.Client(ctx, tenantID, ref)
	}
	if r.Default == nil {
		return Resolved{}, ErrNotConfigured
	}
	return r.Default.Client(ctx, tenantID, ref)
}
