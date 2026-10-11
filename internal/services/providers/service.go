// Package providers manages an organization's LLM provider connections: which
// provider, optional base URL and an API key that is encrypted at rest and
// write-only through the API. A connection is tested (by listing the models the
// key can use) before it is saved, and the service hands the agent executor a
// ready client for a named connection. Every outbound request goes through the
// egress guard, since the base URL of an OpenAI-compatible server is chosen by
// the tenant.
package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kartikey/workflow_service/internal/core/executor/agent"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/exception"
	"github.com/kartikey/workflow_service/pkg/llm"
	"github.com/kartikey/workflow_service/pkg/llm/anthropic"
	"github.com/kartikey/workflow_service/pkg/llm/gemini"
	"github.com/kartikey/workflow_service/pkg/llm/openai"
	"github.com/kartikey/workflow_service/pkg/netguard"
	"github.com/kartikey/workflow_service/pkg/uid"
	"github.com/kartikey/workflow_service/pkg/vault"
)

// Provider kinds with special handling; Catalog lists them all.
const (
	Anthropic        = "anthropic"
	OpenAI           = "openai"
	OpenAICompatible = "openai_compatible"
	Gemini           = "gemini"
	Ollama           = "ollama"
)

const (
	callTimeout = 5 * time.Minute  // one model call, bounded further by the node timeout
	testTimeout = 20 * time.Second // a connection test
	maxModels   = 200
)

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// Service is safe for concurrent use. A nil keyring means API keys cannot be
// stored: only key-less OpenAI-compatible connections (a local Ollama) work.
type Service struct {
	q       *gen.Queries
	keyring *vault.Keyring
	policy  netguard.Policy
	pricing llm.Pricing
	baseURL map[string]string // operator overrides per provider kind
}

var _ agent.Clients = (*Service)(nil)

func NewService(pool *pgxpool.Pool, keyring *vault.Keyring, policy netguard.Policy, pricing llm.Pricing) *Service {
	return &Service{q: gen.New(pool), keyring: keyring, policy: policy, pricing: pricing}
}

// WithBaseURLs sets operator-chosen endpoints for the built-in providers (a
// corporate proxy, for example). Tenants cannot set these; an empty entry keeps
// the provider's public endpoint.
func (s *Service) WithBaseURLs(urls map[string]string) *Service {
	s.baseURL = urls
	return s
}

// Connection is a stored connection; the key is never part of it.
type Connection struct {
	ID        pgtype.UUID
	Name      string
	Provider  string
	BaseURL   string
	KeyHint   string
	Agents    int64
	CreatedAt time.Time
	CreatedBy string
}

// TestInput describes a connection that has not been saved.
type TestInput struct {
	Provider string
	BaseURL  string
	APIKey   string
}

// TestResult is a successful connection test.
type TestResult struct {
	Models []string
}

// CreateInput is a new connection. The connection is tested first unless
// SkipTest is set.
type CreateInput struct {
	Name     string
	Provider string
	BaseURL  string
	APIKey   string
	SkipTest bool
}

func aad(orgID pgtype.UUID, name string) []byte {
	return vault.AAD(uid.String(orgID), "provider:"+name)
}

// validate checks a connection and returns the endpoint it uses: the kind's
// default unless base_url overrides it (a proxy, or Ollama on another host).
func (s *Service) validate(provider, baseURL, key string) (string, error) {
	kind, ok := lookup(provider)
	switch {
	case !ok:
		return "", exception.BadRequest(fmt.Sprintf("unknown provider %q; GET /be/v1/provider-kinds lists them", provider))
	case kind.Native() && baseURL != "":
		return "", exception.BadRequest("base_url applies only to OpenAI-compatible providers")
	case key == "" && !kind.KeyOptional:
		return "", exception.BadRequest("api_key is required for " + kind.Title)
	case len(key) > 4096:
		return "", exception.BadRequest("api_key is too long")
	}
	if baseURL == "" {
		baseURL = kind.BaseURL
	}
	if !kind.Native() && baseURL == "" {
		return "", exception.BadRequest("base_url is required for a custom provider, e.g. https://api.example.com/v1")
	}
	if baseURL != "" {
		parsed, err := url.Parse(baseURL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
			return "", exception.BadRequest("base_url must be an absolute http or https URL without credentials")
		}
	}
	return baseURL, nil
}

func (s *Service) newClient(provider, baseURL, key string) (llm.Client, error) {
	httpClient := s.policy.Client(callTimeout)
	switch provider {
	case Anthropic:
		return anthropic.New(key, s.baseURL[Anthropic], httpClient), nil
	case OpenAI:
		return openai.New(key, s.baseURL[OpenAI], httpClient), nil
	case Gemini:
		return gemini.New(key, s.baseURL[Gemini], httpClient), nil
	}
	if _, ok := lookup(provider); !ok {
		return nil, fmt.Errorf("unknown provider %q", provider)
	}
	return openai.NewCompatible(key, baseURL, httpClient), nil
}

// Test lists the models the key can use, proving the key and the endpoint work
// without spending tokens. It saves nothing.
func (s *Service) Test(ctx context.Context, in TestInput) (TestResult, error) {
	baseURL, err := s.validate(in.Provider, in.BaseURL, in.APIKey)
	if err != nil {
		return TestResult{}, err
	}
	client, err := s.newClient(in.Provider, baseURL, in.APIKey)
	if err != nil {
		return TestResult{}, err
	}
	lister, ok := client.(llm.ModelLister)
	if !ok {
		return TestResult{}, exception.BadRequest("this provider cannot be tested")
	}
	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	models, err := lister.ListModels(ctx)
	if err != nil {
		return TestResult{}, testFailure(err, in.APIKey)
	}
	if len(models) > maxModels {
		models = models[:maxModels]
	}
	return TestResult{Models: models}, nil
}

// testFailure turns a provider error into a 422 that says what went wrong
// without echoing the key (some providers quote part of it back).
func testFailure(err error, key string) error {
	detail := "the provider could not be reached"
	var apiErr *llm.Error
	switch {
	case errors.Is(err, netguard.ErrBlocked):
		detail = "the egress policy blocks that address"
	case errors.Is(err, context.DeadlineExceeded):
		detail = "the provider did not answer in time"
	case errors.As(err, &apiErr):
		switch apiErr.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			detail = fmt.Sprintf("the provider rejected the API key (HTTP %d)", apiErr.Status)
		case 0:
			detail = "the provider could not be reached: " + apiErr.Message
		default:
			detail = fmt.Sprintf("the provider answered HTTP %d: %s", apiErr.Status, apiErr.Message)
		}
	}
	if key != "" {
		detail = strings.ReplaceAll(detail, key, "***")
	}
	return exception.New(http.StatusUnprocessableEntity, "provider_test_failed", "Connection test failed", detail)
}

func hint(key string) string {
	if len(key) < 8 {
		return ""
	}
	return "…" + key[len(key)-4:]
}

func view(c gen.ProviderConnection, agents int64) Connection {
	return Connection{
		ID: c.ID, Name: c.Name, Provider: c.Provider, BaseURL: c.BaseUrl.String, KeyHint: c.KeyHint.String,
		Agents: agents, CreatedAt: c.CreatedAt.Time, CreatedBy: c.CreatedBy.String,
	}
}

// Create tests (unless skipped) and stores a connection. Nothing is stored when
// the test fails.
func (s *Service) Create(ctx context.Context, orgID pgtype.UUID, actor string, in CreateInput) (Connection, TestResult, error) {
	in.Name = strings.TrimSpace(in.Name)
	if !namePattern.MatchString(in.Name) {
		return Connection{}, TestResult{}, exception.BadRequest("name must be lowercase letters, digits, - or _ (at most 63 characters)")
	}
	baseURL, err := s.validate(in.Provider, in.BaseURL, in.APIKey)
	if err != nil {
		return Connection{}, TestResult{}, err
	}
	in.BaseURL = baseURL // stored, so a later change to a default does not move it
	if in.APIKey != "" && s.keyring == nil {
		return Connection{}, TestResult{}, exception.New(http.StatusNotImplemented, "vault_not_configured", "Vault is not configured",
			"set VAULT_MASTER_KEY to store provider API keys")
	}
	var tested TestResult
	if !in.SkipTest {
		var err error
		if tested, err = s.Test(ctx, TestInput{Provider: in.Provider, BaseURL: in.BaseURL, APIKey: in.APIKey}); err != nil {
			return Connection{}, TestResult{}, err
		}
	}
	params := gen.CreateProviderConnectionParams{
		ID: uid.New(), OrgID: orgID, Name: in.Name, Provider: in.Provider,
		BaseUrl:   pgtype.Text{String: in.BaseURL, Valid: in.BaseURL != ""},
		CreatedBy: pgtype.Text{String: actor, Valid: actor != ""},
	}
	if in.APIKey != "" {
		ciphertext, nonce, version, err := s.keyring.Seal([]byte(in.APIKey), aad(orgID, in.Name))
		if err != nil {
			return Connection{}, TestResult{}, err
		}
		params.KeyCiphertext, params.KeyNonce = ciphertext, nonce
		params.KeyVersion = pgtype.Int4{Int32: int32(version), Valid: true}
		params.KeyHint = pgtype.Text{String: hint(in.APIKey), Valid: hint(in.APIKey) != ""}
	}
	row, err := s.q.CreateProviderConnection(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) {
		return Connection{}, TestResult{}, exception.Conflict("name_taken", fmt.Sprintf("a connection named %q already exists", in.Name))
	}
	if err != nil {
		return Connection{}, TestResult{}, fmt.Errorf("create provider connection: %w", err)
	}
	return view(row, 0), tested, nil
}

// List returns the organization's connections (never their keys).
func (s *Service) List(ctx context.Context, orgID pgtype.UUID) ([]Connection, error) {
	rows, err := s.q.ListProviderConnections(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("list provider connections: %w", err)
	}
	out := make([]Connection, 0, len(rows))
	for _, row := range rows {
		out = append(out, Connection{
			ID: row.ID, Name: row.Name, Provider: row.Provider, BaseURL: row.BaseUrl.String, KeyHint: row.KeyHint.String,
			Agents: row.AgentCount, CreatedAt: row.CreatedAt.Time, CreatedBy: row.CreatedBy.String,
		})
	}
	return out, nil
}

func (s *Service) load(ctx context.Context, orgID, id pgtype.UUID) (gen.ProviderConnection, error) {
	row, err := s.q.GetProviderConnection(ctx, gen.GetProviderConnectionParams{ID: id, OrgID: orgID})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.ProviderConnection{}, exception.NotFound("provider connection")
	}
	if err != nil {
		return gen.ProviderConnection{}, fmt.Errorf("get provider connection: %w", err)
	}
	return row, nil
}

// TestStored re-tests a saved connection with its stored key.
func (s *Service) TestStored(ctx context.Context, orgID, id pgtype.UUID) (TestResult, error) {
	row, err := s.load(ctx, orgID, id)
	if err != nil {
		return TestResult{}, err
	}
	key, err := s.openKey(orgID, row)
	if err != nil {
		return TestResult{}, err
	}
	return s.Test(ctx, TestInput{Provider: row.Provider, BaseURL: row.BaseUrl.String, APIKey: key})
}

// Delete removes a connection. It is refused while an agent still uses it.
func (s *Service) Delete(ctx context.Context, orgID, id pgtype.UUID) error {
	rows, err := s.q.DeleteProviderConnection(ctx, gen.DeleteProviderConnectionParams{ID: id, OrgID: orgID})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" { // foreign_key_violation
		return exception.Conflict("connection_in_use", "an agent still uses this connection; change or delete the agent first")
	}
	if err != nil {
		return fmt.Errorf("delete provider connection: %w", err)
	}
	if rows == 0 {
		return exception.NotFound("provider connection")
	}
	return nil
}

func (s *Service) openKey(orgID pgtype.UUID, row gen.ProviderConnection) (string, error) {
	if !row.KeyVersion.Valid {
		return "", nil // a key-less connection
	}
	if s.keyring == nil {
		return "", agent.ErrNotConfigured
	}
	plaintext, err := s.keyring.Open(int(row.KeyVersion.Int32), row.KeyNonce, row.KeyCiphertext, aad(orgID, row.Name))
	if err != nil {
		return "", fmt.Errorf("decrypt the key of provider connection %q (key version %d): %w", row.Name, row.KeyVersion.Int32, err)
	}
	return string(plaintext), nil
}

// Client implements agent.Clients for named connections: it builds the client
// for ref.Connection with its decrypted key.
func (s *Service) Client(ctx context.Context, tenantID uuid.UUID, ref agent.ProviderRef) (agent.Resolved, error) {
	orgID := pgtype.UUID{Bytes: [16]byte(tenantID), Valid: true}
	row, err := s.q.GetProviderConnectionByName(ctx, gen.GetProviderConnectionByNameParams{OrgID: orgID, Name: ref.Connection})
	if errors.Is(err, pgx.ErrNoRows) {
		return agent.Resolved{}, agent.ErrConnectionNotFound
	}
	if err != nil {
		return agent.Resolved{}, fmt.Errorf("load provider connection: %w", err)
	}
	key, err := s.openKey(orgID, row)
	if err != nil {
		return agent.Resolved{}, err
	}
	client, err := s.newClient(row.Provider, row.BaseUrl.String, key)
	if err != nil {
		return agent.Resolved{}, err
	}
	return agent.Resolved{Client: client, Provider: row.Provider, Pricing: s.pricing}, nil
}
