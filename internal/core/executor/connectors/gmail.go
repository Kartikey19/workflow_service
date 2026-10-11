package connectors

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/pkg/mailer"
	"github.com/kartikey/workflow_service/pkg/netguard"
)

// Gmail sends mail as the connected Google account (gmail.send).
type Gmail struct{ base }

var _ executor.Executor = (*Gmail)(nil)

// NewGmail builds the executor. A nil endpoints map selects the public APIs.
func NewGmail(conns executor.Connections, policy netguard.Policy, ep Endpoints) *Gmail {
	return &Gmail{base{conns, policy, ep}}
}

func (*Gmail) Type() string { return "gmail.send" }

func (*Gmail) Describe() executor.Description {
	return executor.Description{
		Title: "Send Gmail", Category: "integration",
		Description: "Sends one email as the Google account behind a Gmail connection. 429 and rate-limit replies are retried; Gmail has no " +
			"idempotency key, so a send whose result is lost can be sent again when redelivered.",
		Example: `- id: notify
  type: gmail.send
  config:
    connection: my-gmail
    to: [team@example.com]
    subject: "Report ready"
    text: "The nightly report is ready."`,
		Output: `{"id": "<message id>", "thread_id": "<thread id>"}`,
	}
}

func (*Gmail) ValidateConfig(raw json.RawMessage) error {
	if err := executor.CheckFields(raw, []executor.Field{
		{Name: "connection", Kind: "string", Required: true},
		{Name: "to", Kind: "any"}, {Name: "cc", Kind: "any"}, {Name: "bcc", Kind: "any"},
		{Name: "reply_to", Kind: "string"}, {Name: "from", Kind: "string"},
		{Name: "subject", Kind: "string", Required: true},
		{Name: "text", Kind: "string"}, {Name: "html", Kind: "string"},
	}); err != nil {
		return err
	}
	var c map[string]any
	_ = json.Unmarshal(raw, &c)
	if _, ok := c["text"]; !ok {
		if _, ok := c["html"]; !ok {
			return errors.New("text or html is required")
		}
	}
	for _, key := range []string{"to", "cc", "bcc"} {
		if _, ok := c[key]; ok {
			return nil
		}
	}
	return errors.New("at least one of to, cc or bcc is required")
}

// recipients accepts one address or a list.
type recipients []string

func (l *recipients) UnmarshalJSON(data []byte) error {
	var one string
	if json.Unmarshal(data, &one) == nil {
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

func (g *Gmail) Execute(ctx context.Context, req executor.Request) (executor.Result, error) {
	var c struct {
		Connection string     `json:"connection"`
		To         recipients `json:"to"`
		Cc         recipients `json:"cc"`
		Bcc        recipients `json:"bcc"`
		ReplyTo    string     `json:"reply_to"`
		From       string     `json:"from"`
		Subject    string     `json:"subject"`
		Text       string     `json:"text"`
		HTML       string     `json:"html"`
	}
	if err := executor.DecodeConfig(req.Config, &c); err != nil {
		return executor.Result{}, err
	}
	conn, err := g.connection(ctx, req, c.Connection, "gmail")
	if err != nil {
		return executor.Result{}, err
	}
	from := c.From
	if from == "" {
		from, _ = conn.Config["account"].(string)
	}
	if from == "" {
		return executor.Result{}, executor.Permanent("bad_config", errors.New("from is required: the connection has no known account address"))
	}
	domain := "gmail.com"
	if at := strings.LastIndexByte(from, '@'); at >= 0 {
		domain = strings.Trim(from[at+1:], "> ")
	}
	message := mailer.Message{
		From: from, To: c.To, Cc: c.Cc, Bcc: c.Bcc, ReplyTo: c.ReplyTo, Subject: c.Subject, Text: c.Text, HTML: c.HTML,
		MessageID: req.IdempotencyKey + "@" + domain,
	}
	_, wire, err := message.Build()
	if err != nil {
		return executor.Result{}, executor.Permanent("bad_config", err)
	}
	body, _ := json.Marshal(map[string]string{"raw": base64.RawURLEncoding.EncodeToString(wire)})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, g.ep.URL("gmail_api", "")+"/gmail/v1/users/me/messages/send", bytes.NewReader(body))
	if err != nil {
		return executor.Result{}, executor.Permanent("bad_config", err)
	}
	authorize(httpReq, conn)
	httpReq.Header.Set("Content-Type", "application/json")
	data, err := g.do(ctx, httpReq, "Gmail")
	if err != nil {
		return executor.Result{}, err
	}
	var sent struct {
		ID       string `json:"id"`
		ThreadID string `json:"threadId"`
	}
	_ = json.Unmarshal(data, &sent)
	output, _ := json.Marshal(map[string]any{"id": sent.ID, "thread_id": sent.ThreadID})
	return executor.Result{Output: output}, nil
}
