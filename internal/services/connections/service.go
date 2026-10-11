// Package connections manages an organization's connections to external
// services: SMTP and Resend for email, OAuth-based file and mail services, and
// databases (05 §3.1, M14). Settings are stored in clear; credentials are one
// JSON object sealed with the vault master key and bound to the organization
// and connection name, and are write-only through the API. The service also
// gives executors a decrypted connection for one use, refreshing OAuth tokens
// that are about to expire.
package connections

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/internal/core/executor/connectors"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/exception"
	"github.com/kartikey/workflow_service/pkg/netguard"
	"github.com/kartikey/workflow_service/pkg/uid"
	"github.com/kartikey/workflow_service/pkg/vault"
)

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// OAuthApp is an OAuth application the operator registered with a provider.
type OAuthApp struct{ ClientID, ClientSecret string }

// Options configures the service. Everything here is operator-level: tenants
// cannot change an endpoint or an OAuth app.
type Options struct {
	ResendURL       string // default https://api.resend.com
	PublicURL       string // externally visible base URL; OAuth redirects come back to it
	Google          OAuthApp
	Dropbox         OAuthApp
	Microsoft       OAuthApp
	MicrosoftTenant string // default "common"
	Endpoints       connectors.Endpoints
}

// Service is safe for concurrent use. A nil keyring means no credential can be
// stored, so only kinds without secrets could work; in practice every kind needs
// the vault key.
type Service struct {
	pool     *pgxpool.Pool
	q        *gen.Queries
	keyring  *vault.Keyring
	policy   netguard.Policy
	opts     Options
	registry map[string]kind
	refresh  sync.Map // connection ID -> *sync.Mutex, to refresh one token at a time
}

var _ executor.Connections = (*Service)(nil)

func NewService(pool *pgxpool.Pool, keyring *vault.Keyring, policy netguard.Policy, opts Options) *Service {
	if opts.ResendURL == "" {
		opts.ResendURL = "https://api.resend.com"
	}
	opts.ResendURL = strings.TrimRight(opts.ResendURL, "/")
	if opts.MicrosoftTenant == "" {
		opts.MicrosoftTenant = "common"
	}
	s := &Service{pool: pool, q: gen.New(pool), keyring: keyring, policy: policy, opts: opts}
	s.registry = s.kinds()
	return s
}

// endpoint returns a provider URL.
func (s *Service) endpoint(key string) string {
	return s.opts.Endpoints.URL(key, s.opts.MicrosoftTenant)
}

// Connection is a stored connection as callers see it: credentials never.
type Connection struct {
	ID        pgtype.UUID
	Name      string
	Kind      string
	Config    map[string]any
	Hint      string
	Account   string
	ExpiresAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
	CreatedBy string
}

// Input creates or updates a connection.
type Input struct {
	Name     string
	Kind     string
	Config   map[string]any
	Secret   map[string]string
	SkipTest bool
}

func aad(orgID pgtype.UUID, name string) []byte {
	return vault.AAD(uid.String(orgID), "connection:"+name)
}

func (s *Service) seal(orgID pgtype.UUID, name string, secret map[string]string) (ciphertext, nonce []byte, version int, err error) {
	if s.keyring == nil {
		return nil, nil, 0, exception.New(http.StatusNotImplemented, "vault_not_configured", "Vault is not configured",
			"set VAULT_MASTER_KEY to store connection credentials")
	}
	plaintext, err := json.Marshal(secret)
	if err != nil {
		return nil, nil, 0, err
	}
	return s.keyring.Seal(plaintext, aad(orgID, name))
}

func (s *Service) open(orgID pgtype.UUID, row gen.Connection) (map[string]string, error) {
	if !row.SecretKeyVersion.Valid {
		return map[string]string{}, nil
	}
	if s.keyring == nil {
		return nil, exception.New(http.StatusNotImplemented, "vault_not_configured", "Vault is not configured", "set VAULT_MASTER_KEY")
	}
	plaintext, err := s.keyring.Open(int(row.SecretKeyVersion.Int32), row.SecretNonce, row.SecretCiphertext, aad(orgID, row.Name))
	if err != nil {
		return nil, fmt.Errorf("decrypt connection %q (key version %d): %w", row.Name, row.SecretKeyVersion.Int32, err)
	}
	var secret map[string]string
	if err := json.Unmarshal(plaintext, &secret); err != nil {
		return nil, fmt.Errorf("decode the credentials of connection %q: %w", row.Name, err)
	}
	return secret, nil
}

func decodeConfig(raw []byte) map[string]any {
	config := map[string]any{}
	_ = json.Unmarshal(raw, &config)
	return config
}

func view(row gen.Connection) Connection {
	c := Connection{
		ID: row.ID, Name: row.Name, Kind: row.Kind, Config: decodeConfig(row.Config), Hint: row.SecretHint.String,
		Account: row.Account.String, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time, CreatedBy: row.CreatedBy.String,
	}
	if row.TokenExpiresAt.Valid {
		t := row.TokenExpiresAt.Time
		c.ExpiresAt = &t
	}
	return c
}

func hint(k kind, secret map[string]string) string {
	value := secret[k.hintField]
	if len(value) < 8 {
		return ""
	}
	return "…" + value[len(value)-4:]
}

// validate normalises the input for its kind and returns the kind.
func (s *Service) validate(in *Input) (kind, error) {
	in.Name = strings.TrimSpace(in.Name)
	if !namePattern.MatchString(in.Name) {
		return kind{}, exception.BadRequest("name must be lowercase letters, digits, - or _ (at most 63 characters)")
	}
	k, ok := s.registry[in.Kind]
	if !ok {
		return kind{}, exception.BadRequest(fmt.Sprintf("kind %q is not available on this installation", in.Kind))
	}
	if in.Config == nil {
		in.Config = map[string]any{}
	}
	if in.Secret == nil {
		in.Secret = map[string]string{}
	}
	if k.info.OAuth {
		return kind{}, exception.BadRequest(fmt.Sprintf("%s connections are created by signing in with the provider: POST /be/v1/connections/oauth/start", in.Kind))
	}
	if err := k.check(in.Config, in.Secret); err != nil {
		return kind{}, exception.BadRequest(err.Error())
	}
	return k, nil
}

// TestInput checks settings and credentials that have not been saved.
func (s *Service) TestInput(ctx context.Context, in Input) error {
	k, err := s.validate(&in)
	if err != nil {
		return err
	}
	return s.runTest(ctx, k, in.Config, in.Secret)
}

func (s *Service) runTest(ctx context.Context, k kind, config map[string]any, secret map[string]string) error {
	if k.test == nil {
		return exception.BadRequest("this kind of connection cannot be tested")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := k.test(ctx, s, config, secret); err != nil {
		return testFailure(err, secret)
	}
	return nil
}

// testFailure is a 422 that explains the failure without echoing credentials.
func testFailure(err error, secret map[string]string) error {
	detail := err.Error()
	switch {
	case errors.Is(err, netguard.ErrBlocked):
		detail = "the egress policy blocks that address"
	case errors.Is(err, context.DeadlineExceeded):
		detail = "the service did not answer in time"
	}
	for _, value := range secret {
		if len(value) >= 4 {
			detail = strings.ReplaceAll(detail, value, "***")
		}
	}
	return exception.New(http.StatusUnprocessableEntity, "connection_test_failed", "Connection test failed", detail)
}

// Create tests (unless skipped) and stores a connection; nothing is stored when
// the test fails.
func (s *Service) Create(ctx context.Context, orgID pgtype.UUID, actor string, in Input) (Connection, error) {
	k, err := s.validate(&in)
	if err != nil {
		return Connection{}, err
	}
	if !in.SkipTest {
		if err := s.runTest(ctx, k, in.Config, in.Secret); err != nil {
			return Connection{}, err
		}
	}
	params := gen.CreateConnectionParams{
		ID: uid.New(), OrgID: orgID, Name: in.Name, Kind: in.Kind, CreatedBy: pgtype.Text{String: actor, Valid: actor != ""},
	}
	if params.Config, err = json.Marshal(in.Config); err != nil {
		return Connection{}, err
	}
	if len(in.Secret) > 0 {
		ciphertext, nonce, version, err := s.seal(orgID, in.Name, in.Secret)
		if err != nil {
			return Connection{}, err
		}
		params.SecretCiphertext, params.SecretNonce = ciphertext, nonce
		params.SecretKeyVersion = pgtype.Int4{Int32: int32(version), Valid: true}
		params.SecretHint = pgtype.Text{String: hint(k, in.Secret), Valid: hint(k, in.Secret) != ""}
	}
	row, err := s.q.CreateConnection(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) {
		return Connection{}, exception.Conflict("name_taken", fmt.Sprintf("a connection named %q already exists", in.Name))
	}
	if err != nil {
		return Connection{}, fmt.Errorf("create connection: %w", err)
	}
	return view(row), nil
}

func (s *Service) load(ctx context.Context, orgID pgtype.UUID, name string) (gen.Connection, error) {
	row, err := s.q.GetConnection(ctx, gen.GetConnectionParams{OrgID: orgID, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.Connection{}, exception.NotFound("connection " + name)
	}
	if err != nil {
		return gen.Connection{}, fmt.Errorf("get connection: %w", err)
	}
	return row, nil
}

// Get returns one connection (never its credentials).
func (s *Service) Get(ctx context.Context, orgID pgtype.UUID, name string) (Connection, error) {
	row, err := s.load(ctx, orgID, name)
	if err != nil {
		return Connection{}, err
	}
	return view(row), nil
}

// List returns the organization's connections.
func (s *Service) List(ctx context.Context, orgID pgtype.UUID) ([]Connection, error) {
	rows, err := s.q.ListConnections(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("list connections: %w", err)
	}
	out := make([]Connection, 0, len(rows))
	for _, row := range rows {
		c := view(gen.Connection{ID: row.ID, Name: row.Name, Kind: row.Kind, Config: row.Config, SecretHint: row.SecretHint, Account: row.Account, TokenExpiresAt: row.TokenExpiresAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, CreatedBy: row.CreatedBy})
		out = append(out, c)
	}
	return out, nil
}

// Update replaces a connection's settings and, when new ones are given, its
// credentials. The kind cannot change.
func (s *Service) Update(ctx context.Context, orgID pgtype.UUID, name string, in Input) (Connection, error) {
	current, err := s.load(ctx, orgID, name)
	if err != nil {
		return Connection{}, err
	}
	in.Name, in.Kind = name, current.Kind
	secret := in.Secret
	if len(secret) == 0 { // keep the stored credentials; validate against them
		if secret, err = s.open(orgID, current); err != nil {
			return Connection{}, err
		}
		in.Secret = secret
	}
	k, err := s.validate(&in)
	if err != nil {
		return Connection{}, err
	}
	if !in.SkipTest {
		if err := s.runTest(ctx, k, in.Config, in.Secret); err != nil {
			return Connection{}, err
		}
	}
	params := gen.UpdateConnectionParams{OrgID: orgID, Name: name}
	if params.Config, err = json.Marshal(in.Config); err != nil {
		return Connection{}, err
	}
	if len(in.Secret) > 0 && !sameSecret(in.Secret, secret, current) {
		ciphertext, nonce, version, err := s.seal(orgID, name, in.Secret)
		if err != nil {
			return Connection{}, err
		}
		params.SecretCiphertext, params.SecretNonce = ciphertext, nonce
		params.SecretKeyVersion = pgtype.Int4{Int32: int32(version), Valid: true}
		params.SecretHint = pgtype.Text{String: hint(k, in.Secret), Valid: hint(k, in.Secret) != ""}
	}
	row, err := s.q.UpdateConnection(ctx, params)
	if err != nil {
		return Connection{}, fmt.Errorf("update connection: %w", err)
	}
	return view(row), nil
}

// sameSecret reports whether the credentials are the ones already stored (so
// Update need not reseal them).
func sameSecret(given, stored map[string]string, current gen.Connection) bool {
	if !current.SecretKeyVersion.Valid || len(given) != len(stored) {
		return false
	}
	for k, v := range given {
		if stored[k] != v {
			return false
		}
	}
	return true
}

// Delete removes a connection. Nodes that still name it fail with
// E_CONNECTION_NOT_FOUND.
func (s *Service) Delete(ctx context.Context, orgID pgtype.UUID, name string) error {
	rows, err := s.q.DeleteConnection(ctx, gen.DeleteConnectionParams{OrgID: orgID, Name: name})
	if err != nil {
		return fmt.Errorf("delete connection: %w", err)
	}
	if rows == 0 {
		return exception.NotFound("connection " + name)
	}
	return nil
}

// Test re-tests a saved connection with its stored credentials.
func (s *Service) Test(ctx context.Context, orgID pgtype.UUID, name string) error {
	row, err := s.load(ctx, orgID, name)
	if err != nil {
		return err
	}
	secret, err := s.open(orgID, row)
	if err != nil {
		return err
	}
	k, ok := s.registry[row.Kind]
	if !ok {
		return exception.BadRequest(fmt.Sprintf("kind %q is not available on this installation", row.Kind))
	}
	return s.runTest(ctx, k, decodeConfig(row.Config), secret)
}

// Connection implements executor.Connections: the decrypted connection for one
// use. The kind check stops a node from using, say, an SMTP password as a
// database credential.
func (s *Service) Connection(ctx context.Context, tenantID uuid.UUID, name string, kinds ...string) (executor.Connection, error) {
	orgID := pgtype.UUID{Bytes: [16]byte(tenantID), Valid: true}
	row, err := s.q.GetConnection(ctx, gen.GetConnectionParams{OrgID: orgID, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return executor.Connection{}, executor.ErrConnectionNotFound
	}
	if err != nil {
		return executor.Connection{}, fmt.Errorf("load connection: %w", err)
	}
	if len(kinds) > 0 && !contains(kinds, row.Kind) {
		return executor.Connection{}, fmt.Errorf("%w: %q is a %s connection, not %s", executor.ErrConnectionNotFound, name, row.Kind, strings.Join(kinds, " or "))
	}
	secret, err := s.open(orgID, row)
	if err != nil {
		return executor.Connection{}, err
	}
	if k, ok := s.registry[row.Kind]; ok && k.info.OAuth && row.TokenExpiresAt.Valid && time.Until(row.TokenExpiresAt.Time) < refreshWindow {
		if secret, err = s.refreshToken(ctx, orgID, row, secret); err != nil {
			return executor.Connection{}, err
		}
	}
	return executor.Connection{Name: row.Name, Kind: row.Kind, Config: decodeConfig(row.Config), Secret: secret}, nil
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
