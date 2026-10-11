// Package auth identifies callers and authorises them within an organization.
// An organization is the tenant: tenant_id everywhere is the organization ID.
package auth

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/pkg/uid"
)

// Roles, from least to most privileged (05 §0.1).
const (
	RoleViewer = "viewer"
	RoleEditor = "editor"
	RoleAdmin  = "admin"
	RoleOwner  = "owner"
)

var rank = map[string]int{RoleViewer: 1, RoleEditor: 2, RoleAdmin: 3, RoleOwner: 4}

// ValidRole reports whether role is one of the four membership roles.
func ValidRole(role string) bool { return rank[role] > 0 }

// AtLeast reports whether role grants at least minimum.
func AtLeast(role, minimum string) bool { return rank[role] >= rank[minimum] && rank[minimum] > 0 }

var (
	ErrUnauthenticated = errors.New("authentication required")
	ErrForbidden       = errors.New("not allowed")
)

// Principal is the authenticated caller acting in one organization.
type Principal struct {
	TenantID      pgtype.UUID // the organization
	UserID        pgtype.UUID // invalid for API keys and webhooks
	Email         string
	Role          string
	PlatformAdmin bool
	Via           string // session | api_key | dev
	SessionID     pgtype.UUID
}

// Actor names the caller for created_by columns.
func (p Principal) Actor() string {
	switch {
	case p.Email != "":
		return p.Email
	case p.Via == "api_key":
		return "api_key"
	default:
		return p.Via
	}
}

// Can reports whether the principal's role grants at least minimum.
func (p Principal) Can(minimum string) bool { return AtLeast(p.Role, minimum) }

type principalKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// From returns the principal the authentication middleware attached.
func From(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// Authenticator resolves a request to a principal, or ErrUnauthenticated.
type Authenticator interface {
	Authenticate(r *http.Request) (Principal, error)
}

// DevHeader trusts an X-Tenant-ID header and grants the owner role. It exists
// for local development and tests only; production uses sessions and API keys.
type DevHeader struct{}

func (DevHeader) Authenticate(r *http.Request) (Principal, error) {
	tenant, err := uid.Parse(r.Header.Get("X-Tenant-ID"))
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	return Principal{TenantID: tenant, Role: RoleOwner, Via: "dev"}, nil
}
