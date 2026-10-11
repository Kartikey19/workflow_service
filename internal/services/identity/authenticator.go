package identity

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/auth"
)

// Authenticator resolves a request to a principal from a bearer API key
// (Authorization: Bearer wfk_...) or the session cookie. It implements
// auth.Authenticator.
type Authenticator struct{ s *Service }

var _ auth.Authenticator = (*Authenticator)(nil)

// Authenticator returns the request authenticator backed by this service.
func (s *Service) Authenticator() *Authenticator { return &Authenticator{s} }

// Authenticate returns auth.ErrUnauthenticated when the request carries no valid
// credential. The reason is deliberately not distinguished.
func (a *Authenticator) Authenticate(r *http.Request) (auth.Principal, error) {
	if header := r.Header.Get("Authorization"); header != "" {
		token, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || !strings.HasPrefix(token, apiKeyPrefix) {
			return auth.Principal{}, auth.ErrUnauthenticated
		}
		return a.apiKey(r.Context(), strings.TrimSpace(token))
	}
	cookie, err := r.Cookie(SessionCookie)
	if err != nil || cookie.Value == "" {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	if !sameOrigin(r) {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	return a.session(r.Context(), cookie.Value)
}

func (a *Authenticator) apiKey(ctx context.Context, token string) (auth.Principal, error) {
	key, err := a.s.q.GetAPIKeyAuth(ctx, hashToken(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	if err != nil {
		return auth.Principal{}, err
	}
	// Best effort: a failed touch must not fail the request.
	_ = a.s.q.TouchAPIKey(ctx, key.ID)
	return auth.Principal{TenantID: key.OrgID, Role: key.Role, Name: key.Name, Via: "api_key"}, nil
}

func (a *Authenticator) session(ctx context.Context, token string) (auth.Principal, error) {
	row, err := a.s.q.GetSessionAuth(ctx, hashToken(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	if err != nil {
		return auth.Principal{}, err
	}
	if row.DisabledAt.Valid || !row.Role.Valid {
		// A removed member's session stops working in that organization at once.
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	if time.Since(row.LastSeenAt.Time) > touchAfter {
		_ = a.s.q.TouchSession(ctx, gen.TouchSessionParams{
			ID: row.SessionID, ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(a.s.cfg.SessionTTL), Valid: true},
		})
	}
	return auth.Principal{
		TenantID: row.OrgID, UserID: row.UserID, Email: row.Email, Role: row.Role.String,
		PlatformAdmin: row.PlatformAdmin, Via: "session", SessionID: row.SessionID,
	}, nil
}

// sameOrigin is the CSRF check for cookie authentication. SameSite=Lax already
// keeps the cookie off cross-site POSTs; this also rejects a state-changing
// request whose Origin names another site. Requests without an Origin header are
// not browser cross-site requests and pass.
func sameOrigin(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && strings.EqualFold(parsed.Host, r.Host)
}
