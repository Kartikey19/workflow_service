// Package triggers owns tenant-scoped webhook credentials and run creation.
package triggers

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kartikey/workflow_service/internal/services/runs"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/exception"
	"github.com/kartikey/workflow_service/pkg/uid"
	"github.com/kartikey/workflow_service/pkg/vault"
)

type Starter interface {
	Start(context.Context, runs.StartParams) (runs.StartResult, error)
}
type Service struct {
	q    *gen.Queries
	keys *vault.Keyring
	runs Starter
}

func New(pool *pgxpool.Pool, keys *vault.Keyring, starter Starter) *Service {
	return &Service{q: gen.New(pool), keys: keys, runs: starter}
}

type Input struct {
	Name      string `json:"name"`
	Workflow  string `json:"workflow"`
	InputMode string `json:"input_mode"`
	Enabled   *bool  `json:"enabled"`
	Signed    bool   `json:"signed"`
}
type Info struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Workflow  string    `json:"workflow"`
	InputMode string    `json:"input_mode"`
	Enabled   bool      `json:"enabled"`
	Signed    bool      `json:"signed"`
	CreatedAt time.Time `json:"created_at"`
}
type Created struct {
	Info
	Token         string `json:"token"`
	SigningSecret string `json:"signing_secret,omitempty"`
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

func token() string {
	raw := make([]byte, 32)
	rand.Read(raw)
	return base64.RawURLEncoding.EncodeToString(raw)
}
func hash(value string) []byte { sum := sha256.Sum256([]byte(value)); return sum[:] }
func mode(value string) (string, error) {
	if value == "" {
		return "body", nil
	}
	if value != "body" && value != "envelope" {
		return "", exception.BadRequest("input_mode must be body or envelope")
	}
	return value, nil
}
func view(row gen.Trigger) Info {
	return Info{ID: uid.String(row.ID), Name: row.Name, Workflow: row.Workflow, InputMode: row.InputMode, Enabled: row.Enabled, Signed: row.SecretKeyVersion.Valid, CreatedAt: row.CreatedAt.Time}
}
func (s *Service) Create(ctx context.Context, org pgtype.UUID, in Input) (Created, error) {
	if !namePattern.MatchString(in.Name) {
		return Created{}, exception.BadRequest("invalid webhook name")
	}
	inputMode, err := mode(in.InputMode)
	if err != nil {
		return Created{}, err
	}
	workflow, err := s.q.GetWorkflowByName(ctx, gen.GetWorkflowByNameParams{TenantID: org, Name: in.Workflow})
	if errors.Is(err, pgx.ErrNoRows) || err == nil && workflow.ArchivedAt.Valid {
		return Created{}, exception.BadRequest("workflow must name an active workflow in this organization")
	}
	if err != nil {
		return Created{}, fmt.Errorf("load webhook workflow: %w", err)
	}
	id := uid.New()
	credential := token()
	params := gen.CreateTriggerParams{ID: id, OrgID: org, Name: in.Name, Workflow: in.Workflow, TokenHash: hash(credential), InputMode: inputMode, Enabled: true}
	if in.Enabled != nil {
		params.Enabled = *in.Enabled
	}
	secret := ""
	if in.Signed {
		if s.keys == nil {
			return Created{}, exception.New(501, "vault_not_configured", "Vault is not configured", "set VAULT_MASTER_KEY for signed webhooks")
		}
		secret = token()
		cipher, nonce, version, err := s.keys.Seal([]byte(secret), vault.AAD(uid.String(org), "trigger:"+uid.String(id)))
		if err != nil {
			return Created{}, err
		}
		params.SecretCiphertext = cipher
		params.SecretNonce = nonce
		params.SecretKeyVersion = pgtype.Int4{Int32: int32(version), Valid: true}
	}
	row, err := s.q.CreateTrigger(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) {
		return Created{}, exception.Conflict("name_taken", "a webhook with this name already exists")
	}
	if err != nil {
		return Created{}, fmt.Errorf("create webhook: %w", err)
	}
	return Created{Info: view(row), Token: credential, SigningSecret: secret}, nil
}
func (s *Service) List(ctx context.Context, org pgtype.UUID) ([]Info, error) {
	rows, err := s.q.ListTriggers(ctx, org)
	if err != nil {
		return nil, fmt.Errorf("list webhooks: %w", err)
	}
	out := make([]Info, 0, len(rows))
	for _, r := range rows {
		out = append(out, view(r))
	}
	return out, nil
}
func (s *Service) Update(ctx context.Context, org, id pgtype.UUID, enabled *bool, inputMode string) (Info, error) {
	row, err := s.q.GetTrigger(ctx, gen.GetTriggerParams{OrgID: org, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return Info{}, exception.NotFound("webhook")
	}
	if err != nil {
		return Info{}, err
	}
	if enabled != nil {
		row.Enabled = *enabled
	}
	if inputMode != "" {
		row.InputMode, err = mode(inputMode)
		if err != nil {
			return Info{}, err
		}
	}
	row, err = s.q.UpdateTrigger(ctx, gen.UpdateTriggerParams{OrgID: org, ID: id, Enabled: row.Enabled, InputMode: row.InputMode})
	if errors.Is(err, pgx.ErrNoRows) {
		return Info{}, exception.NotFound("webhook")
	}
	if err != nil {
		return Info{}, fmt.Errorf("update webhook: %w", err)
	}
	return view(row), nil
}
func (s *Service) Delete(ctx context.Context, org, id pgtype.UUID) error {
	n, err := s.q.DeleteTrigger(ctx, gen.DeleteTriggerParams{OrgID: org, ID: id})
	if err != nil {
		return fmt.Errorf("delete webhook: %w", err)
	}
	if n == 0 {
		return exception.NotFound("webhook")
	}
	return nil
}
func (s *Service) RotateToken(ctx context.Context, org, id pgtype.UUID) (string, error) {
	value := token()
	n, err := s.q.RotateTriggerToken(ctx, gen.RotateTriggerTokenParams{OrgID: org, ID: id, TokenHash: hash(value)})
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", exception.NotFound("webhook")
	}
	return value, nil
}

// Fire authenticates the exact bytes before parsing. RunService applies the same
// plan allowances, validation and atomic idempotency as ordinary API runs.
func (s *Service) Fire(ctx context.Context, credential, signature, key string, body []byte, headers http.Header, query map[string][]string) (runs.StartResult, error) {
	if len(credential) != 43 {
		return runs.StartResult{}, exception.NotFound("webhook")
	}
	row, err := s.q.GetTriggerByToken(ctx, hash(credential))
	if errors.Is(err, pgx.ErrNoRows) {
		return runs.StartResult{}, exception.NotFound("webhook")
	}
	if err != nil {
		return runs.StartResult{}, fmt.Errorf("load webhook: %w", err)
	}
	if row.SecretKeyVersion.Valid {
		if s.keys == nil {
			return runs.StartResult{}, exception.Internal()
		}
		secret, err := s.keys.Open(int(row.SecretKeyVersion.Int32), row.SecretNonce, row.SecretCiphertext, vault.AAD(uid.String(row.OrgID), "trigger:"+uid.String(row.ID)))
		if err != nil {
			return runs.StartResult{}, errors.New("decrypt webhook signing key")
		}
		supplied, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
		mac := hmac.New(sha256.New, secret)
		mac.Write(body)
		if err != nil || !strings.HasPrefix(signature, "sha256=") || !hmac.Equal(supplied, mac.Sum(nil)) {
			return runs.StartResult{}, exception.Unauthorized()
		}
	}
	if len(key) > 200 {
		return runs.StartResult{}, exception.BadRequest("Idempotency-Key must be at most 200 characters")
	}
	var input map[string]any
	if row.InputMode == "body" {
		if json.Unmarshal(body, &input) != nil || input == nil {
			return runs.StartResult{}, exception.BadRequest("webhook body must be a JSON object")
		}
	} else {
		var value any
		if json.Unmarshal(body, &value) != nil {
			return runs.StartResult{}, exception.BadRequest("webhook body must be JSON")
		}
		safeHeaders := map[string]any{}
		for name, values := range headers {
			switch strings.ToLower(name) {
			case "authorization", "cookie", "x-workflow-signature", "idempotency-key":
				continue
			}
			safeHeaders[name] = values
		}
		safeQuery := map[string]any{}
		for name, values := range query {
			safeQuery[name] = values
		}
		input = map[string]any{"body": value, "headers": safeHeaders, "query": safeQuery}
	}
	if key != "" {
		key = "hook:" + uid.String(row.ID) + ":" + hex.EncodeToString(hash(key))
	}
	return s.runs.Start(ctx, runs.StartParams{TenantID: row.OrgID, Workflow: row.Workflow, Input: input, Trigger: "webhook", CreatedBy: "webhook:" + row.Name, IdempotencyKey: key})
}
