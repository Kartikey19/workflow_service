// Package vault is the application service for per-organization secrets
// (05 §0.1, M12). Values are sealed with pkg/vault before they touch the
// database and are write-only through the API: only names are ever listed. The
// worker decrypts an organization's secrets when a node's config refers to them
// (it implements engine.SecretSource), and they are never written to
// node_executions.input, logs or events.
package vault

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/exception"
	"github.com/kartikey/workflow_service/pkg/uid"
	"github.com/kartikey/workflow_service/pkg/vault"
	"go.uber.org/zap"
)

const (
	// MinValueLength keeps outputs and errors scrubbable: redacting a one or two
	// character value would mangle everything around it.
	MinValueLength = 8
	// MaxValueBytes bounds a single secret.
	MaxValueBytes = 64 << 10
	rotateBatch   = 100
)

var namePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// Service is safe for concurrent use. A nil keyring means the vault is not
// configured: every operation answers 501.
type Service struct {
	q       *gen.Queries
	pool    *pgxpool.Pool
	keyring *vault.Keyring
	log     *zap.Logger
}

func NewService(pool *pgxpool.Pool, keyring *vault.Keyring, log *zap.Logger) *Service {
	if log == nil {
		log = zap.NewNop()
	}
	return &Service{q: gen.New(pool), pool: pool, keyring: keyring, log: log}
}

func notConfigured() error {
	return exception.New(501, "vault_not_configured", "Vault is not configured",
		"set VAULT_MASTER_KEY (32 bytes, base64) to store secrets")
}

// aad binds a ciphertext to its organization and name.
func aad(orgID pgtype.UUID, name string) []byte { return vault.AAD(uid.String(orgID), name) }

// Info is a secret's metadata; the value is never returned.
type Info struct {
	Name       string
	KeyVersion int
	CreatedAt  time.Time
	UpdatedAt  time.Time
	UpdatedBy  string
}

// Put stores or replaces a secret.
func (s *Service) Put(ctx context.Context, orgID pgtype.UUID, name, value, actor string) error {
	if s.keyring == nil {
		return notConfigured()
	}
	if !namePattern.MatchString(name) {
		return exception.BadRequest("a secret name starts with a capital letter and uses capitals, digits and underscores (at most 64)")
	}
	if len(value) < MinValueLength {
		return exception.BadRequest(fmt.Sprintf("a secret value must be at least %d characters", MinValueLength))
	}
	if len(value) > MaxValueBytes {
		return exception.BadRequest(fmt.Sprintf("a secret value must be at most %d bytes", MaxValueBytes))
	}
	ciphertext, nonce, version, err := s.keyring.Seal([]byte(value), aad(orgID, name))
	if err != nil {
		return err
	}
	if err := s.q.UpsertSecret(ctx, gen.UpsertSecretParams{
		ID: uid.New(), OrgID: orgID, Name: name, Ciphertext: ciphertext, Nonce: nonce, KeyVersion: int32(version),
		UpdatedBy: pgtype.Text{String: actor, Valid: actor != ""},
	}); err != nil {
		return fmt.Errorf("store secret: %w", err)
	}
	return nil
}

// List returns the organization's secret names and metadata.
func (s *Service) List(ctx context.Context, orgID pgtype.UUID) ([]Info, error) {
	if s.keyring == nil {
		return nil, notConfigured()
	}
	rows, err := s.q.ListSecretNames(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	out := make([]Info, 0, len(rows))
	for _, row := range rows {
		out = append(out, Info{Name: row.Name, KeyVersion: int(row.KeyVersion), CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time, UpdatedBy: row.UpdatedBy.String})
	}
	return out, nil
}

// Delete removes a secret. Workflows that still refer to it fail with
// E_SECRET_MISSING.
func (s *Service) Delete(ctx context.Context, orgID pgtype.UUID, name string) error {
	if s.keyring == nil {
		return notConfigured()
	}
	rows, err := s.q.DeleteSecret(ctx, gen.DeleteSecretParams{OrgID: orgID, Name: name})
	if err != nil {
		return fmt.Errorf("delete secret: %w", err)
	}
	if rows == 0 {
		return exception.NotFound("secret " + name)
	}
	return nil
}

// Secrets decrypts every secret of the organization, for one node execution.
// It implements engine.SecretSource. Decrypting on demand, not caching, means a
// changed or deleted secret applies to the very next node.
func (s *Service) Secrets(ctx context.Context, tenantID pgtype.UUID) (map[string]string, error) {
	if s.keyring == nil {
		return map[string]string{}, nil
	}
	rows, err := s.q.ListSecretsForOrg(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("load secrets: %w", err)
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		plaintext, err := s.keyring.Open(int(row.KeyVersion), row.Nonce, row.Ciphertext, aad(tenantID, row.Name))
		if err != nil {
			// Name the secret, never its value or bytes.
			return nil, fmt.Errorf("decrypt secret %s (key version %d): %w", row.Name, row.KeyVersion, err)
		}
		out[row.Name] = string(plaintext)
	}
	return out, nil
}

// RotateResult reports a rotation pass over everything sealed under the master
// key: vault secrets, provider-connection API keys and connection credentials.
type RotateResult struct {
	Rotated int // re-sealed under the current key
	Skipped int // changed by someone else while the pass ran; already current
	Failed  int // could not be opened (their key is no longer held)
}

func (r *RotateResult) add(other RotateResult) {
	r.Rotated += other.Rotated
	r.Skipped += other.Skipped
	r.Failed += other.Failed
}

// sealed is one row waiting to be re-sealed, from any table.
type sealed struct {
	id         pgtype.UUID
	label      []byte // the additional authenticated data it was sealed with
	name       string
	ciphertext []byte
	nonce      []byte
	version    int32
}

// source lists a page of rows not under the current key and replaces one.
type source struct {
	table   string
	list    func(ctx context.Context, current int32, after pgtype.UUID) ([]sealed, error)
	replace func(ctx context.Context, row sealed, ciphertext, nonce []byte, version int32) (int64, error)
}

func (s *Service) sources() []source {
	return []source{
		{
			table: "secrets",
			list: func(ctx context.Context, current int32, after pgtype.UUID) ([]sealed, error) {
				rows, err := s.q.ListSecretsToRotate(ctx, gen.ListSecretsToRotateParams{CurrentVersion: current, AfterID: after, Lim: rotateBatch})
				out := make([]sealed, 0, len(rows))
				for _, r := range rows {
					out = append(out, sealed{r.ID, aad(r.OrgID, r.Name), r.Name, r.Ciphertext, r.Nonce, r.KeyVersion})
				}
				return out, err
			},
			replace: func(ctx context.Context, row sealed, ciphertext, nonce []byte, version int32) (int64, error) {
				return s.q.RotateSecret(ctx, gen.RotateSecretParams{ID: row.id, Ciphertext: ciphertext, Nonce: nonce, NewVersion: version, OldVersion: row.version})
			},
		},
		{
			table: "provider_connections",
			list: func(ctx context.Context, current int32, after pgtype.UUID) ([]sealed, error) {
				rows, err := s.q.ListProviderKeysToRotate(ctx, gen.ListProviderKeysToRotateParams{CurrentVersion: pgtype.Int4{Int32: current, Valid: true}, AfterID: after, Lim: rotateBatch})
				out := make([]sealed, 0, len(rows))
				for _, r := range rows {
					out = append(out, sealed{r.ID, vault.AAD(uid.String(r.OrgID), "provider:"+r.Name), r.Name, r.KeyCiphertext, r.KeyNonce, r.KeyVersion.Int32})
				}
				return out, err
			},
			replace: func(ctx context.Context, row sealed, ciphertext, nonce []byte, version int32) (int64, error) {
				return s.q.RotateProviderKey(ctx, gen.RotateProviderKeyParams{
					ID: row.id, Ciphertext: ciphertext, Nonce: nonce, NewVersion: pgtype.Int4{Int32: version, Valid: true}, OldVersion: pgtype.Int4{Int32: row.version, Valid: true},
				})
			},
		},
		{
			table: "connections",
			list: func(ctx context.Context, current int32, after pgtype.UUID) ([]sealed, error) {
				rows, err := s.q.ListConnectionsToRotate(ctx, gen.ListConnectionsToRotateParams{CurrentVersion: pgtype.Int4{Int32: current, Valid: true}, AfterID: after, Lim: rotateBatch})
				out := make([]sealed, 0, len(rows))
				for _, r := range rows {
					out = append(out, sealed{r.ID, vault.AAD(uid.String(r.OrgID), "connection:"+r.Name), r.Name, r.SecretCiphertext, r.SecretNonce, r.SecretKeyVersion.Int32})
				}
				return out, err
			},
			replace: func(ctx context.Context, row sealed, ciphertext, nonce []byte, version int32) (int64, error) {
				return s.q.RotateConnectionSecret(ctx, gen.RotateConnectionSecretParams{
					ID: row.id, Ciphertext: ciphertext, Nonce: nonce, NewVersion: pgtype.Int4{Int32: version, Valid: true}, OldVersion: pgtype.Int4{Int32: row.version, Valid: true},
				})
			},
		},
	}
}

// Rotate re-seals everything that is not under the current key version. It is
// safe to run while the service is serving: each row is replaced with a
// compare-and-set on its old version, and reads keep working because old keys
// stay in the keyring until the operator removes them.
func (s *Service) Rotate(ctx context.Context) (RotateResult, error) {
	var total RotateResult
	if s.keyring == nil {
		return total, notConfigured()
	}
	for _, src := range s.sources() {
		result, err := s.rotateSource(ctx, src)
		total.add(result)
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (s *Service) rotateSource(ctx context.Context, src source) (RotateResult, error) {
	var result RotateResult
	after := pgtype.UUID{Valid: true} // the zero UUID sorts before every other
	for {
		rows, err := src.list(ctx, int32(s.keyring.Current()), after)
		if err != nil {
			return result, fmt.Errorf("list %s to rotate: %w", src.table, err)
		}
		if len(rows) == 0 {
			return result, nil
		}
		for _, row := range rows {
			after = row.id
			plaintext, err := s.keyring.Open(int(row.version), row.nonce, row.ciphertext, row.label)
			if err != nil {
				result.Failed++
				s.log.Error("rotate: cannot open a sealed value", zap.String("table", src.table), zap.String("name", row.name),
					zap.Int32("key_version", row.version), zap.Error(err))
				continue
			}
			ciphertext, nonce, version, err := s.keyring.Seal(plaintext, row.label)
			if err != nil {
				return result, err
			}
			changed, err := src.replace(ctx, row, ciphertext, nonce, int32(version))
			if err != nil {
				return result, fmt.Errorf("rotate %s: %w", src.table, err)
			}
			if changed == 0 {
				result.Skipped++
				continue
			}
			result.Rotated++
		}
	}
}

// KeyVersions reports how many sealed values use each key version, across every
// table, so an operator can tell when an old key is safe to remove.
func (s *Service) KeyVersions(ctx context.Context) (map[int]int64, error) {
	if s.keyring == nil {
		return nil, notConfigured()
	}
	rows, err := s.q.CountSealedByKeyVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("count sealed values by key version: %w", err)
	}
	out := make(map[int]int64, len(rows))
	for _, row := range rows {
		out[int(row.KeyVersion)] = row.Total
	}
	return out, nil
}

// Current is the key version new secrets are sealed under.
func (s *Service) Current() int {
	if s.keyring == nil {
		return 0
	}
	return s.keyring.Current()
}
