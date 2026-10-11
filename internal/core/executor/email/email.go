// Package email implements the email.send node (02 §4.11): one message over SMTP
// or Resend, with credentials taken from a named connection or given inline
// through ${secrets.*}.
package email

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/pkg/httpx"
	"github.com/kartikey/workflow_service/pkg/mailer"
	"github.com/kartikey/workflow_service/pkg/netguard"
)

const resendTimeout = 30 * time.Second

// Executor sends email.
type Executor struct {
	conns     executor.Connections
	policy    netguard.Policy
	resendURL string
}

var _ executor.Executor = (*Executor)(nil)

// New builds the executor. resendURL is Resend's API root; empty selects the
// public endpoint. A nil conns means only inline credentials work.
func New(conns executor.Connections, policy netguard.Policy, resendURL string) *Executor {
	if resendURL == "" {
		resendURL = "https://api.resend.com"
	}
	return &Executor{conns: conns, policy: policy, resendURL: strings.TrimRight(resendURL, "/")}
}

func (*Executor) Type() string { return "email.send" }

func (*Executor) Describe() executor.Description {
	return executor.Description{
		Title: "Send email", Category: "integration",
		Description: "Sends one email through an SMTP server or Resend. Use a saved connection (`connection: <name>`) or give credentials inline " +
			"with ${secrets.*}. Temporary failures (4xx, timeouts, rate limits) are retried; rejected recipients and bad credentials are not. " +
			"The Message-ID (SMTP) and Idempotency-Key (Resend) are stable across retries.",
		Example: `- id: notify
  type: email.send
  needs: [create_ticket]
  config:
    connection: team-smtp
    to: [oncall@example.com]
    subject: "Ticket ${nodes.create_ticket.output.body.id} created"
    text: "A new ticket needs attention."

# inline credentials instead of a connection:
#   resend: {api_key: ${secrets.RESEND_KEY}}
#   smtp: {host: smtp.example.com, username: bot, password: ${secrets.SMTP_PASSWORD}}`,
		Output: `{"provider": "smtp" | "resend", "message_id": "...", "recipients": 2}`,
	}
}

type config struct {
	Connection string        `json:"connection"`
	SMTP       *inlineSMTP   `json:"smtp"`
	Resend     *inlineResend `json:"resend"`
	From       string        `json:"from"`
	To         stringList    `json:"to"`
	Cc         stringList    `json:"cc"`
	Bcc        stringList    `json:"bcc"`
	ReplyTo    string        `json:"reply_to"`
	Subject    string        `json:"subject"`
	Text       string        `json:"text"`
	HTML       string        `json:"html"`
}

type inlineSMTP struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	TLS      string `json:"tls"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type inlineResend struct {
	APIKey string `json:"api_key"`
}

// stringList accepts "a@x.com" or ["a@x.com", "b@x.com"].
type stringList []string

func (l *stringList) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		if one != "" {
			*l = []string{one}
		}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return errors.New("must be an address or a list of addresses")
	}
	*l = many
	return nil
}

func (*Executor) ValidateConfig(raw json.RawMessage) error {
	if err := executor.CheckFields(raw, []executor.Field{
		{Name: "connection", Kind: "string"},
		{Name: "smtp", Kind: "object"},
		{Name: "resend", Kind: "object"},
		{Name: "from", Kind: "string"},
		{Name: "to", Kind: "any"},
		{Name: "cc", Kind: "any"},
		{Name: "bcc", Kind: "any"},
		{Name: "reply_to", Kind: "string"},
		{Name: "subject", Kind: "string", Required: true},
		{Name: "text", Kind: "string"},
		{Name: "html", Kind: "string"},
	}); err != nil {
		return err
	}
	var c map[string]any
	_ = json.Unmarshal(raw, &c)
	chosen := 0
	for _, key := range []string{"connection", "smtp", "resend"} {
		if _, ok := c[key]; ok {
			chosen++
		}
	}
	if chosen != 1 {
		return errors.New("set exactly one of connection, smtp or resend")
	}
	if _, hasText := c["text"]; !hasText {
		if _, hasHTML := c["html"]; !hasHTML {
			return errors.New("text or html is required")
		}
	}
	if _, ok := c["to"]; !ok {
		if _, ok := c["cc"]; !ok {
			if _, ok := c["bcc"]; !ok {
				return errors.New("at least one of to, cc or bcc is required")
			}
		}
	}
	return nil
}

// provider is the resolved way to send.
type provider struct {
	kind   string // "smtp" or "resend"
	smtp   mailer.SMTPConfig
	apiKey string
	from   string // default sender from a connection
}

func (e *Executor) resolve(ctx context.Context, req executor.Request, c config) (provider, error) {
	switch {
	case c.Connection != "":
		if e.conns == nil {
			return provider{}, executor.Permanent("bad_config", errors.New("saved connections are not available on this worker"))
		}
		conn, err := e.conns.Connection(ctx, req.TenantID, c.Connection, "smtp", "resend")
		if errors.Is(err, executor.ErrConnectionNotFound) {
			return provider{}, executor.Permanent("E_CONNECTION_NOT_FOUND", fmt.Errorf("no smtp or resend connection named %q", c.Connection))
		}
		if err != nil {
			return provider{}, executor.Retryable("connection_lookup", err)
		}
		for _, v := range conn.Secret {
			req.RedactSecrets(v)
		}
		from, _ := conn.Config["from"].(string)
		if conn.Kind == "resend" {
			return provider{kind: "resend", apiKey: conn.Secret["api_key"], from: from}, nil
		}
		return provider{kind: "smtp", smtp: smtpFromConnection(conn), from: from}, nil
	case c.SMTP != nil:
		cfg := mailer.SMTPConfig{Host: c.SMTP.Host, Port: c.SMTP.Port, TLS: c.SMTP.TLS, Username: c.SMTP.Username, Password: c.SMTP.Password}
		if cfg.Port == 0 {
			cfg.Port = 587
		}
		if cfg.TLS == "" {
			cfg.TLS = mailer.TLSStartTLS
			if cfg.Port == 465 {
				cfg.TLS = mailer.TLSImplicit
			}
		}
		if cfg.Host == "" || strings.ContainsAny(cfg.Host, "/ \r\n") {
			return provider{}, executor.Permanent("bad_config", errors.New("smtp.host is required and must be a host name"))
		}
		if cfg.TLS != mailer.TLSStartTLS && cfg.TLS != mailer.TLSImplicit && cfg.TLS != mailer.TLSNone {
			return provider{}, executor.Permanent("bad_config", errors.New("smtp.tls must be starttls, tls or none"))
		}
		req.RedactSecrets(cfg.Password)
		return provider{kind: "smtp", smtp: cfg}, nil
	default:
		req.RedactSecrets(c.Resend.APIKey)
		return provider{kind: "resend", apiKey: c.Resend.APIKey}, nil
	}
}

func smtpFromConnection(conn executor.Connection) mailer.SMTPConfig {
	cfg := mailer.SMTPConfig{Password: conn.Secret["password"], TLS: mailer.TLSStartTLS, Port: 587}
	cfg.Host, _ = conn.Config["host"].(string)
	cfg.Username, _ = conn.Config["username"].(string)
	if mode, ok := conn.Config["tls"].(string); ok && mode != "" {
		cfg.TLS = mode
	}
	switch port := conn.Config["port"].(type) {
	case float64:
		cfg.Port = int(port)
	case int:
		cfg.Port = port
	}
	return cfg
}

func (e *Executor) Execute(ctx context.Context, req executor.Request) (executor.Result, error) {
	var c config
	if err := executor.DecodeConfig(req.Config, &c); err != nil {
		return executor.Result{}, err
	}
	chosen := 0
	for _, set := range []bool{c.Connection != "", c.SMTP != nil, c.Resend != nil} {
		if set {
			chosen++
		}
	}
	if chosen != 1 {
		return executor.Result{}, executor.Permanent("bad_config", errors.New("set exactly one of connection, smtp or resend"))
	}
	p, err := e.resolve(ctx, req, c)
	if err != nil {
		return executor.Result{}, err
	}
	from := c.From
	if from == "" {
		from = p.from
	}
	if from == "" {
		return executor.Result{}, executor.Permanent("bad_config", errors.New("from is required: set it on the node or on the connection"))
	}
	message := mailer.Message{
		From: from, To: c.To, Cc: c.Cc, Bcc: c.Bcc, ReplyTo: c.ReplyTo, Subject: c.Subject, Text: c.Text, HTML: c.HTML,
		MessageID: messageID(req.IdempotencyKey, from),
	}
	// Validate before touching the network, so a bad message is permanent.
	envelope, _, err := message.Build()
	if err != nil {
		return executor.Result{}, executor.Permanent("bad_config", err)
	}
	output, _ := json.Marshal(map[string]any{"provider": p.kind, "message_id": message.MessageID, "recipients": len(envelope.Recipients)})
	if p.kind == "smtp" {
		p.smtp.Dial = e.policy.DialContext
		if err := p.smtp.Send(ctx, message); err != nil {
			return executor.Result{}, classifySMTP(ctx, err)
		}
		return executor.Result{Output: output}, nil
	}
	if err := e.sendResend(ctx, p.apiKey, message, req.IdempotencyKey); err != nil {
		return executor.Result{}, err
	}
	return executor.Result{Output: output}, nil
}

// messageID derives a stable Message-ID from the node's idempotency key, so a
// retried send carries the same ID and a receiver or relay can recognise it.
func messageID(key, from string) string {
	domain := "workflow.local"
	if at := strings.LastIndexByte(from, '@'); at >= 0 {
		domain = strings.Trim(from[at+1:], "> ")
	}
	return key + "@" + domain
}

func classifySMTP(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var smtpErr *mailer.Error
	if errors.As(err, &smtpErr) {
		switch {
		case smtpErr.Temporary:
			return executor.Retryable("smtp_temporary", err)
		case smtpErr.Code == 535 || strings.Contains(err.Error(), "authentication failed"):
			return executor.Permanent("smtp_auth", err)
		default:
			return executor.Permanent("smtp_rejected", err)
		}
	}
	if errors.Is(err, netguard.ErrBlocked) {
		return executor.Permanent("egress_blocked", err)
	}
	return executor.Retryable("connection_error", err)
}

func (e *Executor) sendResend(ctx context.Context, apiKey string, m mailer.Message, idempotencyKey string) error {
	body := map[string]any{"from": m.From, "to": m.To, "subject": m.Subject}
	if len(m.Cc) > 0 {
		body["cc"] = m.Cc
	}
	if len(m.Bcc) > 0 {
		body["bcc"] = m.Bcc
	}
	if m.ReplyTo != "" {
		body["reply_to"] = m.ReplyTo
	}
	if m.Text != "" {
		body["text"] = m.Text
	}
	if m.HTML != "" {
		body["html"] = m.HTML
	}
	body["headers"] = map[string]string{"Message-ID": "<" + m.MessageID + ">"}
	encoded, err := json.Marshal(body)
	if err != nil {
		return executor.Permanent("bad_config", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.resendURL+"/emails", bytes.NewReader(encoded))
	if err != nil {
		return executor.Permanent("bad_config", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	res, err := e.policy.Client(resendTimeout).Do(req)
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, netguard.ErrBlocked):
			return executor.Permanent("egress_blocked", err)
		}
		return executor.Retryable("connection_error", errors.New("Resend could not be reached"))
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return nil
	}
	var failure struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(data, &failure)
	cause := fmt.Errorf("Resend answered HTTP %d: %s", res.StatusCode, strings.TrimSpace(failure.Message))
	switch {
	case res.StatusCode == http.StatusTooManyRequests:
		return &executor.RetryableError{Code: "rate_limited", Err: cause, RetryAfter: httpx.ParseRetryAfter(res.Header.Get("Retry-After"))}
	case res.StatusCode >= 500 || res.StatusCode == http.StatusRequestTimeout:
		return &executor.RetryableError{Code: "upstream_5xx", Err: cause, RetryAfter: httpx.ParseRetryAfter(res.Header.Get("Retry-After"))}
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return executor.Permanent("auth", cause)
	default:
		return executor.Permanent("upstream_4xx", cause)
	}
}
