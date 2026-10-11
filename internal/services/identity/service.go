// Package identity is the application service for users, organizations,
// memberships, invites, sessions, API keys and plan limits (05 §0.1, M11). An
// organization is the tenant. Passwords are bcrypt hashes; session, invite and
// API-key tokens are random and stored only as SHA-256 hashes.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/auth"
	"github.com/kartikey/workflow_service/pkg/exception"
	"golang.org/x/crypto/bcrypt"
)

// Config tunes the service; zero values select the defaults.
type Config struct {
	SessionTTL      time.Duration // default 14 days, extended while the session is used
	InviteTTL       time.Duration // default 7 days
	BcryptCost      int           // default 12
	MaxFailedLogins int           // default 5 in a row locks the account
	LockFor         time.Duration // default 15 minutes
	// CookieSecure marks the session cookie Secure; leave it on behind HTTPS.
	CookieSecure bool
}

const (
	defaultSessionTTL = 14 * 24 * time.Hour
	defaultInviteTTL  = 7 * 24 * time.Hour
	minPasswordLength = 10
	maxPasswordBytes  = 72 // bcrypt ignores everything after this
	// SessionCookie is the name of the same-origin session cookie.
	SessionCookie = "wf_session"
	// touchAfter bounds how often a used session is written back.
	touchAfter = 5 * time.Minute
)

// Service is safe for concurrent use.
type Service struct {
	pool *pgxpool.Pool
	q    *gen.Queries
	cfg  Config
	// dummyHash lets a login for an unknown email cost the same as a real one.
	dummyHash []byte
}

func NewService(pool *pgxpool.Pool, cfg Config) (*Service, error) {
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = defaultSessionTTL
	}
	if cfg.InviteTTL <= 0 {
		cfg.InviteTTL = defaultInviteTTL
	}
	if cfg.BcryptCost == 0 {
		cfg.BcryptCost = 12
	}
	if cfg.MaxFailedLogins <= 0 {
		cfg.MaxFailedLogins = 5
	}
	if cfg.LockFor <= 0 {
		cfg.LockFor = 15 * time.Minute
	}
	dummy, err := bcrypt.GenerateFromPassword([]byte("not a real password"), cfg.BcryptCost)
	if err != nil {
		return nil, fmt.Errorf("prepare password hashing: %w", err)
	}
	return &Service{pool: pool, q: gen.New(pool), cfg: cfg, dummyHash: dummy}, nil
}

// Config returns the effective configuration (cookie handling needs it).
func (s *Service) Config() Config { return s.cfg }

// newToken returns a random token with the given prefix and its storage hash.
func newToken(prefix string) (token, hash string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("generate token: %w", err)
	}
	token = prefix + base64.RawURLEncoding.EncodeToString(raw)
	return token, hashToken(token), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func normalizeEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email || !strings.Contains(email, "@") {
		return "", exception.BadRequest("email is not a valid address")
	}
	return email, nil
}

func checkPassword(password string) error {
	switch {
	case len(password) < minPasswordLength:
		return exception.BadRequest(fmt.Sprintf("password must be at least %d characters", minPasswordLength))
	case len(password) > maxPasswordBytes:
		return exception.BadRequest(fmt.Sprintf("password must be at most %d bytes", maxPasswordBytes))
	}
	return nil
}

func (s *Service) hashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), s.cfg.BcryptCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(hash), nil
}

// Membership is a user's role in one organization.
type Membership struct {
	OrgID   pgtype.UUID
	OrgName string
	OrgSlug string
	Role    string
}

// User is the account fields exposed to callers.
type User struct {
	ID            pgtype.UUID
	Email         string
	Name          string
	PlatformAdmin bool
}

// Session is a new login: the raw token goes to the client once, in a cookie.
type Session struct {
	Token       string
	ExpiresAt   time.Time
	User        User
	Current     Membership
	Memberships []Membership
}

func userView(u gen.User) User {
	return User{ID: u.ID, Email: u.Email, Name: u.Name, PlatformAdmin: u.PlatformAdmin}
}

func (s *Service) memberships(ctx context.Context, q *gen.Queries, userID pgtype.UUID) ([]Membership, error) {
	rows, err := q.ListMembershipsForUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list memberships: %w", err)
	}
	out := make([]Membership, 0, len(rows))
	for _, row := range rows {
		out = append(out, Membership{OrgID: row.OrgID, OrgName: row.OrgName, OrgSlug: row.OrgSlug, Role: row.Role})
	}
	return out, nil
}

// startSession creates a session in org and returns it with the user's memberships.
func (s *Service) startSession(ctx context.Context, q *gen.Queries, user gen.User, org pgtype.UUID) (Session, error) {
	memberships, err := s.memberships(ctx, q, user.ID)
	if err != nil {
		return Session{}, err
	}
	var current *Membership
	for i := range memberships {
		if memberships[i].OrgID == org {
			current = &memberships[i]
		}
	}
	if current == nil {
		return Session{}, exception.Forbidden("you are not a member of any organization")
	}
	token, hash, err := newToken("")
	if err != nil {
		return Session{}, err
	}
	expires := time.Now().Add(s.cfg.SessionTTL)
	if err := q.CreateSession(ctx, gen.CreateSessionParams{
		ID: newID(), UserID: user.ID, OrgID: org, TokenHash: hash,
		ExpiresAt: pgtype.Timestamptz{Time: expires, Valid: true},
	}); err != nil {
		return Session{}, fmt.Errorf("create session: %w", err)
	}
	return Session{Token: token, ExpiresAt: expires, User: userView(user), Current: *current, Memberships: memberships}, nil
}

// Login checks credentials and starts a session in the user's first
// organization. Unknown emails, wrong passwords and disabled accounts all look
// the same, and repeated failures lock the account for a while.
func (s *Service) Login(ctx context.Context, email, password string) (Session, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	invalid := exception.New(401, "invalid_credentials", "Invalid credentials", "the email or password is wrong")
	user, err := s.q.GetUserByEmail(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword(s.dummyHash, []byte(password))
		return Session{}, invalid
	}
	if err != nil {
		return Session{}, fmt.Errorf("find user: %w", err)
	}
	if user.LockedUntil.Valid && user.LockedUntil.Time.After(time.Now()) {
		return Session{}, exception.New(429, "account_locked", "Account locked",
			"too many failed sign-in attempts; try again later")
	}
	if user.DisabledAt.Valid || bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)) != nil {
		if !user.DisabledAt.Valid {
			if err := s.q.RecordLoginFailure(ctx, gen.RecordLoginFailureParams{
				ID: user.ID, MaxFailures: int32(s.cfg.MaxFailedLogins),
				LockFor: pgtype.Interval{Microseconds: s.cfg.LockFor.Microseconds(), Valid: true},
			}); err != nil {
				return Session{}, fmt.Errorf("record login failure: %w", err)
			}
		}
		return Session{}, invalid
	}
	if user.FailedLogins > 0 || user.LockedUntil.Valid {
		if err := s.q.ResetLoginFailures(ctx, user.ID); err != nil {
			return Session{}, fmt.Errorf("reset login failures: %w", err)
		}
	}
	memberships, err := s.memberships(ctx, s.q, user.ID)
	if err != nil {
		return Session{}, err
	}
	if len(memberships) == 0 {
		return Session{}, exception.Forbidden("this account does not belong to any organization")
	}
	return s.startSession(ctx, s.q, user, memberships[0].OrgID)
}

// Logout ends the session.
func (s *Service) Logout(ctx context.Context, sessionID pgtype.UUID) error {
	if err := s.q.DeleteSession(ctx, sessionID); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// Me describes the caller for the UI: the user, the current organization and
// every organization they can switch to.
func (s *Service) Me(ctx context.Context, p auth.Principal) (User, Membership, []Membership, error) {
	if !p.UserID.Valid {
		return User{}, Membership{}, nil, exception.Forbidden("this endpoint is for signed-in users, not API keys")
	}
	user, err := s.q.GetUserByID(ctx, p.UserID)
	if err != nil {
		return User{}, Membership{}, nil, fmt.Errorf("load user: %w", err)
	}
	memberships, err := s.memberships(ctx, s.q, user.ID)
	if err != nil {
		return User{}, Membership{}, nil, err
	}
	var current Membership
	for _, m := range memberships {
		if m.OrgID == p.TenantID {
			current = m
		}
	}
	return userView(user), current, memberships, nil
}

// SwitchOrg points the session at another organization the user belongs to.
func (s *Service) SwitchOrg(ctx context.Context, p auth.Principal, orgID pgtype.UUID) (Membership, error) {
	if !p.SessionID.Valid {
		return Membership{}, exception.Forbidden("only signed-in sessions can switch organization")
	}
	memberships, err := s.memberships(ctx, s.q, p.UserID)
	if err != nil {
		return Membership{}, err
	}
	for _, m := range memberships {
		if m.OrgID != orgID {
			continue
		}
		if _, err := s.q.SetSessionOrg(ctx, gen.SetSessionOrgParams{ID: p.SessionID, UserID: p.UserID, OrgID: orgID}); err != nil {
			return Membership{}, fmt.Errorf("switch organization: %w", err)
		}
		return m, nil
	}
	return Membership{}, exception.NotFound("organization")
}

// Bootstrap creates the first platform admin, with an organization of their
// own, when no user exists yet. With users present it does nothing, so it is
// safe to run on every start.
func (s *Service) Bootstrap(ctx context.Context, email, password string) (bool, error) {
	count, err := s.q.CountUsers(ctx)
	if err != nil {
		return false, fmt.Errorf("count users: %w", err)
	}
	if count > 0 {
		return false, nil
	}
	email, err = normalizeEmail(email)
	if err != nil {
		return false, fmt.Errorf("BOOTSTRAP_ADMIN_EMAIL: %w", err)
	}
	if err := checkPassword(password); err != nil {
		return false, fmt.Errorf("BOOTSTRAP_ADMIN_PASSWORD: %w", err)
	}
	hash, err := s.hashPassword(password)
	if err != nil {
		return false, err
	}
	err = pgxTx(ctx, s.pool, func(q *gen.Queries) error {
		user, err := q.CreateUser(ctx, gen.CreateUserParams{ID: newID(), Email: email, Name: "Platform admin", PasswordHash: hash, PlatformAdmin: true})
		if err != nil {
			return fmt.Errorf("create bootstrap admin: %w", err)
		}
		org, err := s.createOrganization(ctx, q, "Platform", "team")
		if err != nil {
			return err
		}
		return q.UpsertMembership(ctx, gen.UpsertMembershipParams{OrgID: org.ID, UserID: user.ID, Role: auth.RoleOwner})
	})
	if err != nil {
		return false, err
	}
	return true, nil
}
