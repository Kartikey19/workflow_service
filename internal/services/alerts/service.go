// Package alerts manages notification channels and delivers the alert outbox.
//
// Events are written to alert_deliveries by the transaction that observed them
// (a run finishing, a schedule slot failing; see runs.EnqueueAlert). Workers
// claim due deliveries with FOR UPDATE SKIP LOCKED, send each one and record
// the outcome; failures back off and give up after a few attempts. Channel
// secrets (Slack and webhook URLs, webhook signing keys) are sealed with the
// vault keyring and never returned after creation.
package alerts

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/internal/services/runs"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/db"
	"github.com/kartikey/workflow_service/pkg/exception"
	"github.com/kartikey/workflow_service/pkg/netguard"
	"github.com/kartikey/workflow_service/pkg/uid"
	"github.com/kartikey/workflow_service/pkg/vault"
	"go.uber.org/zap"
)

// Events a channel can subscribe to.
var Events = []string{runs.AlertRunFailed, runs.AlertRunRecovered, runs.AlertConnectionAuth, runs.AlertScheduleFailed}

// Kinds of channel.
const (
	Slack   = "slack"
	Webhook = "webhook"
	Email   = "email"
)

// backoff is the wait after each failed attempt; its length is the attempt
// budget.
var backoff = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 3 * time.Hour}

const sendTimeout = 10 * time.Second

// Executor sends one email; the email.send node implements it.
type Executor interface {
	Execute(ctx context.Context, req executor.Request) (executor.Result, error)
}

type Service struct {
	pool      *pgxpool.Pool
	q         *gen.Queries
	keys      *vault.Keyring
	conns     executor.Connections
	mailer    Executor
	client    *http.Client
	publicURL string
	log       *zap.Logger
}

func New(pool *pgxpool.Pool, keys *vault.Keyring, policy netguard.Policy, conns executor.Connections, mailer Executor, publicURL string, log *zap.Logger) *Service {
	if log == nil {
		log = zap.NewNop()
	}
	return &Service{
		pool: pool, q: gen.New(pool), keys: keys, conns: conns, mailer: mailer,
		client: policy.Client(sendTimeout), publicURL: strings.TrimRight(publicURL, "/"), log: log,
	}
}

// Input creates or changes a channel. On update, empty fields keep their value.
type Input struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Events    []string `json:"events"`
	Workflows []string `json:"workflows"`
	Enabled   *bool    `json:"enabled"`
	// URL is the Slack incoming-webhook URL or the webhook endpoint.
	URL string `json:"url"`
	// Connection and To configure email through a saved SMTP or Resend account.
	Connection string   `json:"connection"`
	To         []string `json:"to"`
}

// Info is a channel as the API shows it. Target is a redacted description.
type Info struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Kind       string    `json:"kind"`
	Events     []string  `json:"events"`
	Workflows  []string  `json:"workflows"`
	Enabled    bool      `json:"enabled"`
	Target     string    `json:"target"`
	Connection string    `json:"connection,omitempty"`
	To         []string  `json:"to,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// Created is returned once, with the webhook signing secret.
type Created struct {
	Info
	SigningSecret string `json:"signing_secret,omitempty"`
}

type channelConfig struct {
	Target     string   `json:"target,omitempty"`
	Connection string   `json:"connection,omitempty"`
	To         []string `json:"to,omitempty"`
}

type channelSecret struct {
	URL           string `json:"url"`
	SigningSecret string `json:"signing_secret,omitempty"`
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

func view(row gen.AlertChannel) Info {
	var cfg channelConfig
	_ = json.Unmarshal(row.Config, &cfg)
	target := cfg.Target
	if row.Kind == Email {
		target = cfg.Connection + " → " + strings.Join(cfg.To, ", ")
	}
	return Info{
		ID: uid.String(row.ID), Name: row.Name, Kind: row.Kind, Events: row.Events, Workflows: row.Workflows,
		Enabled: row.Enabled, Target: target, Connection: cfg.Connection, To: cfg.To, CreatedAt: row.CreatedAt.Time,
	}
}

func aad(org, id pgtype.UUID) []byte { return vault.AAD(uid.String(org), "alert:"+uid.String(id)) }

func randomSecret() string {
	raw := make([]byte, 32)
	rand.Read(raw)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// checkURL accepts an absolute http(s) URL. Where it may point is decided at
// send time by the egress policy, like every other tenant-chosen address.
func checkURL(kind, raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return "", exception.BadRequest("url must be an absolute http(s) URL without credentials")
	}
	if kind == Slack {
		// Slack's URL is itself the credential: show only where it goes.
		return u.Host + "/…" + lastChars(u.Path, 4), nil
	}
	return u.Scheme + "://" + u.Host + u.EscapedPath(), nil
}

func lastChars(value string, n int) string {
	if len(value) <= n {
		return value
	}
	return value[len(value)-n:]
}

func checkEvents(events []string) ([]string, error) {
	if len(events) == 0 {
		return slices.Clone(Events), nil
	}
	out := []string{}
	for _, e := range events {
		if !slices.Contains(Events, e) {
			return nil, exception.BadRequest(fmt.Sprintf("unknown event %q; use %s", e, strings.Join(Events, ", ")))
		}
		if !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	return out, nil
}

func checkRecipients(to []string) ([]string, error) {
	out := []string{}
	for _, address := range to {
		address = strings.TrimSpace(address)
		if address == "" {
			continue
		}
		if _, err := mail.ParseAddress(address); err != nil {
			return nil, exception.BadRequest(fmt.Sprintf("%q is not an email address", address))
		}
		out = append(out, address)
	}
	if len(out) == 0 || len(out) > 20 {
		return nil, exception.BadRequest("email alerts need between 1 and 20 recipients")
	}
	return out, nil
}

func (s *Service) checkConnection(ctx context.Context, org pgtype.UUID, name string) error {
	if s.conns == nil {
		return exception.BadRequest("saved connections are not available")
	}
	if _, err := s.conns.Connection(ctx, uuid.UUID(org.Bytes), name, "smtp", "resend"); err != nil {
		if errors.Is(err, executor.ErrConnectionNotFound) {
			return exception.BadRequest(fmt.Sprintf("no SMTP or Resend connection named %q", name))
		}
		return fmt.Errorf("load email connection: %w", err)
	}
	return nil
}

func (s *Service) seal(org, id pgtype.UUID, secret channelSecret) ([]byte, []byte, pgtype.Int4, error) {
	if s.keys == nil {
		return nil, nil, pgtype.Int4{}, exception.New(501, "vault_not_configured", "Vault is not configured", "set VAULT_MASTER_KEY for Slack and webhook alerts")
	}
	plain, _ := json.Marshal(secret)
	cipher, nonce, version, err := s.keys.Seal(plain, aad(org, id))
	if err != nil {
		return nil, nil, pgtype.Int4{}, err
	}
	return cipher, nonce, pgtype.Int4{Int32: int32(version), Valid: true}, nil
}

func (s *Service) open(org, id pgtype.UUID, cipher, nonce []byte, version pgtype.Int4) (channelSecret, error) {
	var secret channelSecret
	if s.keys == nil || !version.Valid {
		return secret, errors.New("channel secret is unavailable")
	}
	plain, err := s.keys.Open(int(version.Int32), nonce, cipher, aad(org, id))
	if err != nil {
		return secret, errors.New("decrypt channel secret")
	}
	return secret, json.Unmarshal(plain, &secret)
}

func (s *Service) Create(ctx context.Context, org pgtype.UUID, in Input) (Created, error) {
	if !namePattern.MatchString(in.Name) {
		return Created{}, exception.BadRequest("a channel name uses lowercase letters, digits, - and _")
	}
	events, err := checkEvents(in.Events)
	if err != nil {
		return Created{}, err
	}
	workflows := in.Workflows
	if workflows == nil {
		workflows = []string{}
	}
	id := uid.New()
	params := gen.CreateAlertChannelParams{ID: id, OrgID: org, Name: in.Name, Kind: in.Kind, Events: events, Workflows: workflows, Enabled: in.Enabled == nil || *in.Enabled}
	var cfg channelConfig
	created := Created{}
	switch in.Kind {
	case Slack, Webhook:
		if cfg.Target, err = checkURL(in.Kind, in.URL); err != nil {
			return Created{}, err
		}
		secret := channelSecret{URL: strings.TrimSpace(in.URL)}
		if in.Kind == Webhook {
			secret.SigningSecret = randomSecret()
			created.SigningSecret = secret.SigningSecret
		}
		if params.SecretCiphertext, params.SecretNonce, params.SecretKeyVersion, err = s.seal(org, id, secret); err != nil {
			return Created{}, err
		}
	case Email:
		if cfg.To, err = checkRecipients(in.To); err != nil {
			return Created{}, err
		}
		if err := s.checkConnection(ctx, org, in.Connection); err != nil {
			return Created{}, err
		}
		cfg.Connection = in.Connection
	default:
		return Created{}, exception.BadRequest("kind must be slack, webhook or email")
	}
	params.Config, _ = json.Marshal(cfg)
	row, err := s.q.CreateAlertChannel(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) {
		return Created{}, exception.Conflict("name_taken", "an alert channel with this name already exists")
	}
	if err != nil {
		return Created{}, fmt.Errorf("create alert channel: %w", err)
	}
	created.Info = view(row)
	return created, nil
}

func (s *Service) List(ctx context.Context, org pgtype.UUID) ([]Info, error) {
	rows, err := s.q.ListAlertChannels(ctx, org)
	if err != nil {
		return nil, fmt.Errorf("list alert channels: %w", err)
	}
	out := make([]Info, 0, len(rows))
	for _, row := range rows {
		out = append(out, view(row))
	}
	return out, nil
}

func (s *Service) get(ctx context.Context, org, id pgtype.UUID) (gen.AlertChannel, error) {
	row, err := s.q.GetAlertChannel(ctx, gen.GetAlertChannelParams{OrgID: org, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, exception.NotFound("alert channel")
	}
	if err != nil {
		return row, fmt.Errorf("get alert channel: %w", err)
	}
	return row, nil
}

// Update changes subscriptions, recipients or the enabled flag. The URL and
// signing secret of a Slack or webhook channel are fixed: replace the channel
// to change them.
func (s *Service) Update(ctx context.Context, org, id pgtype.UUID, in Input) (Info, error) {
	row, err := s.get(ctx, org, id)
	if err != nil {
		return Info{}, err
	}
	if in.URL != "" {
		return Info{}, exception.BadRequest("a channel's URL cannot change; create a new channel")
	}
	if in.Events != nil {
		if row.Events, err = checkEvents(in.Events); err != nil {
			return Info{}, err
		}
	}
	if in.Workflows != nil {
		row.Workflows = in.Workflows
	}
	if in.Enabled != nil {
		row.Enabled = *in.Enabled
	}
	if row.Kind == Email && (in.To != nil || in.Connection != "") {
		var cfg channelConfig
		_ = json.Unmarshal(row.Config, &cfg)
		if in.To != nil {
			if cfg.To, err = checkRecipients(in.To); err != nil {
				return Info{}, err
			}
		}
		if in.Connection != "" {
			if err := s.checkConnection(ctx, org, in.Connection); err != nil {
				return Info{}, err
			}
			cfg.Connection = in.Connection
		}
		row.Config, _ = json.Marshal(cfg)
	}
	updated, err := s.q.UpdateAlertChannel(ctx, gen.UpdateAlertChannelParams{
		OrgID: org, ID: id, Config: row.Config, Events: row.Events, Workflows: row.Workflows, Enabled: row.Enabled,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Info{}, exception.NotFound("alert channel")
	}
	if err != nil {
		return Info{}, fmt.Errorf("update alert channel: %w", err)
	}
	return view(updated), nil
}

func (s *Service) Delete(ctx context.Context, org, id pgtype.UUID) error {
	n, err := s.q.DeleteAlertChannel(ctx, gen.DeleteAlertChannelParams{OrgID: org, ID: id})
	if err != nil {
		return fmt.Errorf("delete alert channel: %w", err)
	}
	if n == 0 {
		return exception.NotFound("alert channel")
	}
	return nil
}

// Delivery is one entry of the delivery log.
type Delivery struct {
	ID          string         `json:"id"`
	Channel     string         `json:"channel"`
	ChannelKind string         `json:"channel_kind"`
	Event       string         `json:"event"`
	Payload     map[string]any `json:"payload"`
	Status      string         `json:"status"`
	Attempts    int32          `json:"attempts"`
	LastError   string         `json:"last_error,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	SentAt      *time.Time     `json:"sent_at"`
	NextAttempt *time.Time     `json:"next_attempt_at"`
}

func (s *Service) Deliveries(ctx context.Context, org pgtype.UUID, limit int32) ([]Delivery, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.q.ListAlertDeliveries(ctx, gen.ListAlertDeliveriesParams{OrgID: org, Lim: limit})
	if err != nil {
		return nil, fmt.Errorf("list alert deliveries: %w", err)
	}
	out := make([]Delivery, 0, len(rows))
	for _, r := range rows {
		d := Delivery{
			ID: uid.String(r.ID), Channel: r.ChannelName, ChannelKind: r.ChannelKind, Event: r.Event,
			Status: r.Status, Attempts: r.Attempts, LastError: r.LastError.String, CreatedAt: r.CreatedAt.Time,
		}
		_ = json.Unmarshal(r.Payload, &d.Payload)
		if r.SentAt.Valid {
			d.SentAt = &r.SentAt.Time
		}
		if r.Status == "pending" {
			d.NextAttempt = &r.NextAttemptAt.Time
		}
		out = append(out, d)
	}
	return out, nil
}

// message is a rendered alert, shared by every channel kind.
type message struct {
	id      string
	event   string
	org     pgtype.UUID
	subject string
	text    string
	link    string
	payload map[string]any
}

func (s *Service) render(id, event string, org pgtype.UUID, payload map[string]any) message {
	str := func(key string) string { v, _ := payload[key].(string); return v }
	workflow := str("workflow")
	m := message{id: id, event: event, org: org, payload: payload}
	if runID := str("run_id"); runID != "" && s.publicURL != "" {
		m.link = s.publicURL + "/ui/runs/" + runID
	}
	var lines []string
	switch event {
	case runs.AlertRunFailed:
		m.subject = "Workflow " + workflow + " is failing"
		if step := str("node_id"); step != "" {
			lines = append(lines, "Step "+step+" failed: "+str("message"))
		} else if msg := str("message"); msg != "" {
			lines = append(lines, msg)
		}
		lines = append(lines, "Further failures are counted without another alert until a run succeeds.")
	case runs.AlertRunRecovered:
		m.subject = "Workflow " + workflow + " recovered"
		lines = append(lines, fmt.Sprintf("A run succeeded after %v failed run(s).", payload["failures"]))
	case runs.AlertConnectionAuth:
		m.subject = "Reconnect an account used by " + workflow
		lines = append(lines, "Step "+str("node_id")+" lost access to its account: "+str("message"),
			"Open Connectors and reconnect it, then resume the run.")
	case runs.AlertScheduleFailed:
		m.subject = "Schedule " + str("schedule") + " did not start " + workflow
		lines = append(lines, str("message"), "Slot: "+str("slot"))
	case "test":
		m.subject = "Test alert from Weft"
		lines = append(lines, "This channel is set up correctly.")
	default:
		m.subject = event + " in " + workflow
	}
	if m.link != "" {
		lines = append(lines, "Open the run: "+m.link)
	}
	m.text = strings.Join(lines, "\n")
	return m
}

// permanentError is a delivery failure that retrying cannot fix.
type permanentError struct{ error }

func (s *Service) send(ctx context.Context, kind string, channelID pgtype.UUID, config []byte, cipher, nonce []byte, version pgtype.Int4, m message) error {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	switch kind {
	case Slack, Webhook:
		secret, err := s.open(m.org, channelID, cipher, nonce, version)
		if err != nil {
			return permanentError{err}
		}
		var body []byte
		headers := map[string]string{"Content-Type": "application/json"}
		if kind == Slack {
			text := "*" + m.subject + "*\n" + m.text
			body, _ = json.Marshal(map[string]any{"text": text})
		} else {
			body, _ = json.Marshal(map[string]any{
				"id": m.id, "event": m.event, "subject": m.subject, "text": m.text, "url": m.link, "data": m.payload,
			})
			mac := hmac.New(sha256.New, []byte(secret.SigningSecret))
			mac.Write(body)
			headers["X-Weft-Event"] = m.event
			headers["X-Weft-Delivery"] = m.id
			headers["X-Weft-Signature"] = "sha256=" + hex.EncodeToString(mac.Sum(nil))
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, secret.URL, bytes.NewReader(body))
		if err != nil {
			return permanentError{errors.New("invalid channel URL")}
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := s.client.Do(req)
		if err != nil {
			if errors.Is(err, netguard.ErrBlocked) {
				return permanentError{errors.New("the egress policy blocks this address")}
			}
			return errors.New("could not reach the endpoint")
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return nil
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			return fmt.Errorf("endpoint answered %d", resp.StatusCode)
		default:
			return permanentError{fmt.Errorf("endpoint answered %d", resp.StatusCode)}
		}
	case Email:
		if s.mailer == nil {
			return permanentError{errors.New("email is not available on this worker")}
		}
		var cfg channelConfig
		_ = json.Unmarshal(config, &cfg)
		raw, _ := json.Marshal(map[string]any{"connection": cfg.Connection, "to": cfg.To, "subject": "[Weft] " + m.subject, "text": m.text})
		_, err := s.mailer.Execute(ctx, executor.Request{
			TenantID: uuid.UUID(m.org.Bytes), NodeID: "alert", Attempt: 1, Config: raw,
			IdempotencyKey: "alert-" + m.id, Deadline: time.Now().Add(sendTimeout),
		})
		if err != nil {
			var permanent *executor.PermanentError
			if errors.As(err, &permanent) {
				return permanentError{err}
			}
			return err
		}
		return nil
	}
	return permanentError{fmt.Errorf("unknown channel kind %q", kind)}
}

// Test sends a test alert through a channel now and reports the outcome.
func (s *Service) Test(ctx context.Context, org, id pgtype.UUID) error {
	row, err := s.get(ctx, org, id)
	if err != nil {
		return err
	}
	m := s.render(uid.String(uid.New()), "test", org, map[string]any{"event": "test"})
	if err := s.send(ctx, row.Kind, row.ID, row.Config, row.SecretCiphertext, row.SecretNonce, row.SecretKeyVersion, m); err != nil {
		return exception.New(502, "alert_test_failed", "Test alert failed", errorText(err))
	}
	return nil
}

// errorText keeps delivery errors short and free of URLs, which may carry
// credentials (Slack's URL is the secret).
func errorText(err error) string {
	text := err.Error()
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		text = urlErr.Err.Error()
	}
	if len(text) > 300 {
		text = text[:300]
	}
	return text
}

// Deliver sends due deliveries. Workers call it from the sweeper.
func (s *Service) Deliver(ctx context.Context) (int, error) {
	sent := 0
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		due, err := q.ClaimAlertDeliveries(ctx, 10)
		if err != nil {
			return fmt.Errorf("claim alert deliveries: %w", err)
		}
		for _, d := range due {
			// Leave the rest for the next pass rather than let a slow endpoint
			// run the sweeper's pass out of time and roll back what was sent.
			if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < sendTimeout+time.Second {
				break
			}
			payload := map[string]any{}
			_ = json.Unmarshal(d.Payload, &payload)
			m := s.render(uid.String(d.ID), d.Event, d.OrgID, payload)
			err := s.send(ctx, d.ChannelKind, d.ChannelID, d.ChannelConfig, d.SecretCiphertext, d.SecretNonce, d.SecretKeyVersion, m)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			finish := gen.FinishAlertDeliveryParams{ID: d.ID, Status: "sent", NextAttemptAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}}
			if err != nil {
				var permanent permanentError
				finish.LastError = pgtype.Text{String: errorText(err), Valid: true}
				if errors.As(err, &permanent) || int(d.Attempts) >= len(backoff) {
					finish.Status = "failed"
				} else {
					finish.Status = "pending"
					finish.NextAttemptAt.Time = time.Now().Add(backoff[d.Attempts])
				}
				s.log.Warn("alert delivery failed", zap.String("delivery_id", uid.String(d.ID)), zap.String("channel", d.ChannelName),
					zap.String("status", finish.Status), zap.String("error", finish.LastError.String))
			} else {
				sent++
			}
			if err := q.FinishAlertDelivery(ctx, finish); err != nil {
				return fmt.Errorf("record alert delivery: %w", err)
			}
		}
		return nil
	})
	return sent, err
}
