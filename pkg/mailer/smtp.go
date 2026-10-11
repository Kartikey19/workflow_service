package mailer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"time"
)

// TLS modes.
const (
	TLSStartTLS = "starttls" // connect in clear, upgrade; the default on port 587
	TLSImplicit = "tls"      // TLS from the first byte; the default on port 465
	TLSNone     = "none"     // no encryption; only for a trusted local relay
)

// SMTPConfig describes one SMTP server.
type SMTPConfig struct {
	Host     string
	Port     int
	TLS      string
	Username string
	Password string

	// Dial opens the connection; the caller supplies one that enforces the
	// egress policy. nil uses a plain net.Dialer.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
	// TLSConfig overrides certificate verification settings, for tests with a
	// private CA. Never fill it from tenant-supplied data.
	TLSConfig *tls.Config
	// Timeout bounds the whole exchange; default 30s.
	Timeout time.Duration
}

// Error is an SMTP failure. Temporary errors (4xx, connection trouble) are worth
// retrying; permanent ones (5xx, bad credentials) are not.
type Error struct {
	Temporary bool
	Code      int
	Err       error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

func classify(err error) error {
	if err == nil {
		return nil
	}
	var protocol *textproto.Error
	if errors.As(err, &protocol) {
		return &Error{Temporary: protocol.Code >= 400 && protocol.Code < 500, Code: protocol.Code, Err: err}
	}
	return &Error{Temporary: true, Err: err}
}

func (c SMTPConfig) address() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }

func (c SMTPConfig) tlsConfig() *tls.Config {
	if c.TLSConfig != nil {
		cfg := c.TLSConfig.Clone()
		if cfg.ServerName == "" {
			cfg.ServerName = c.Host
		}
		return cfg
	}
	return &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}
}

// connect opens and prepares a session: greeting, STARTTLS when asked for, and
// authentication when credentials are set.
func (c SMTPConfig) connect(ctx context.Context) (*smtp.Client, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	dial := c.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	conn, err := dial(ctx, "tcp", c.address())
	if err != nil {
		return nil, classify(err)
	}
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	if c.TLS == TLSImplicit {
		conn = tls.Client(conn, c.tlsConfig())
	}
	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		_ = conn.Close()
		return nil, classify(err)
	}
	if c.TLS == TLSStartTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			_ = client.Close()
			return nil, &Error{Err: errors.New("the server does not offer STARTTLS; refusing to send credentials in clear")}
		}
		if err := client.StartTLS(c.tlsConfig()); err != nil {
			_ = client.Close()
			return nil, classify(err)
		}
	}
	if c.Username != "" {
		if ok, _ := client.Extension("AUTH"); !ok {
			_ = client.Close()
			return nil, &Error{Err: errors.New("the server does not support authentication")}
		}
		if err := client.Auth(plainAuth{c.Username, c.Password, c.Host, c.TLS != TLSNone}); err != nil {
			_ = client.Close()
			// Wrong credentials will not improve on a retry.
			var protocol *textproto.Error
			if errors.As(err, &protocol) && protocol.Code >= 500 {
				return nil, &Error{Code: protocol.Code, Err: fmt.Errorf("authentication failed: %w", err)}
			}
			return nil, classify(err)
		}
	}
	return client, nil
}

// plainAuth is PLAIN authentication that, unlike smtp.PlainAuth, works over any
// connection we have already decided is acceptable.
type plainAuth struct {
	user, password, host string
	secure               bool
}

func (a plainAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !a.secure && !server.TLS && !isLoopback(server.Name) {
		return "", nil, errors.New("refusing to send a password over an unencrypted connection")
	}
	return "PLAIN", []byte("\x00" + a.user + "\x00" + a.password), nil
}

func (a plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("unexpected server challenge")
	}
	return nil, nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// Check connects, negotiates TLS and authenticates, without sending anything.
func (c SMTPConfig) Check(ctx context.Context) error {
	client, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer client.Close()
	return classify(client.Quit())
}

// Send delivers the message and returns when the server has accepted it.
func (c SMTPConfig) Send(ctx context.Context, m Message) error {
	envelope, data, err := m.Build()
	if err != nil {
		return &Error{Err: err}
	}
	client, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := client.Mail(envelope.From); err != nil {
		return classify(err)
	}
	for _, recipient := range envelope.Recipients {
		if err := client.Rcpt(recipient); err != nil {
			return classify(err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return classify(err)
	}
	if _, err := w.Write(data); err != nil {
		return classify(err)
	}
	if err := w.Close(); err != nil {
		return classify(err)
	}
	return classify(client.Quit())
}
