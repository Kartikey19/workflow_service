package mailer

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"net/smtp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP is a tiny SMTP server for tests. It records accepted messages.
type fakeSMTP struct {
	ln       net.Listener
	tlsConf  *tls.Config
	pool     *x509.CertPool
	implicit bool // TLS from the first byte

	user, pass string // when set, AUTH PLAIN must match
	offerTLS   bool
	rcptCode   int // reply code for RCPT (default 250)
	dataCode   int // reply code at the end of DATA (default 250)

	mu       sync.Mutex
	messages []received
	authSeen []string
}

type received struct {
	From string
	To   []string
	Data string
}

func startSMTP(t *testing.T, configure func(*fakeSMTP)) *fakeSMTP {
	t.Helper()
	cert := httptest.NewUnstartedServer(nil)
	cert.StartTLS()
	s := &fakeSMTP{tlsConf: &tls.Config{Certificates: cert.TLS.Certificates}, pool: x509.NewCertPool(), offerTLS: true}
	s.pool.AddCert(cert.Certificate())
	cert.Close()
	if configure != nil {
		configure(s)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if s.implicit {
		ln = tls.NewListener(ln, s.tlsConf)
	}
	s.ln = ln
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s
}

func (s *fakeSMTP) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *fakeSMTP) config(tlsMode string) SMTPConfig {
	return SMTPConfig{Host: "127.0.0.1", Port: s.port(), TLS: tlsMode, TLSConfig: &tls.Config{RootCAs: s.pool}, Timeout: 5 * time.Second}
}

func (s *fakeSMTP) sent() []received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]received(nil), s.messages...)
}

func (s *fakeSMTP) serve(conn net.Conn) {
	defer conn.Close()
	r, w := bufio.NewReader(conn), conn
	reply := func(format string, args ...any) { fmt.Fprintf(w, format+"\r\n", args...) }
	reply("220 fake ESMTP")
	secure := s.implicit
	var current received
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			reply("250-fake")
			if s.offerTLS && !secure {
				reply("250-STARTTLS")
			}
			if s.user != "" {
				reply("250-AUTH PLAIN")
			}
			reply("250 8BITMIME")
		case upper == "STARTTLS":
			reply("220 go ahead")
			tlsConn := tls.Server(conn, s.tlsConf)
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			conn, r, w, secure = tlsConn, bufio.NewReader(tlsConn), tlsConn, true
			reply = func(format string, args ...any) { fmt.Fprintf(w, format+"\r\n", args...) }
		case strings.HasPrefix(upper, "AUTH PLAIN"):
			encoded := strings.TrimSpace(line[len("AUTH PLAIN"):])
			raw, _ := base64.StdEncoding.DecodeString(encoded)
			s.mu.Lock()
			s.authSeen = append(s.authSeen, strings.ReplaceAll(string(raw), "\x00", "|"))
			s.mu.Unlock()
			if string(raw) == "\x00"+s.user+"\x00"+s.pass {
				reply("235 authenticated")
			} else {
				reply("535 bad credentials")
			}
		case strings.HasPrefix(upper, "MAIL FROM:"):
			addr := line[len("MAIL FROM:"):]
			if end := strings.Index(addr, ">"); end >= 0 {
				addr = addr[:end] // drop ESMTP parameters such as BODY=8BITMIME
			}
			current = received{From: strings.Trim(addr, "<> ")}
			reply("250 ok")
		case strings.HasPrefix(upper, "RCPT TO:"):
			code := s.rcptCode
			if code == 0 {
				code = 250
			}
			if code == 250 {
				current.To = append(current.To, strings.Trim(line[len("RCPT TO:"):], "<> "))
			}
			reply("%d rcpt", code)
		case upper == "DATA":
			reply("354 go")
			var data strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				data.WriteString(l)
			}
			current.Data = data.String()
			code := s.dataCode
			if code == 0 {
				code = 250
			}
			if code == 250 {
				s.mu.Lock()
				s.messages = append(s.messages, current)
				s.mu.Unlock()
			}
			reply("%d done", code)
		case upper == "QUIT":
			reply("221 bye")
			return
		case upper == "RSET", upper == "NOOP":
			reply("250 ok")
		default:
			reply("502 unknown")
		}
	}
}

var smtpServerInfo = smtp.ServerInfo{Name: "mail.example.com"}

func goodMessage() Message {
	return Message{
		From: "Ops <ops@example.com>", To: []string{"a@example.com", "Bee <b@example.com>"}, Cc: []string{"c@example.com"},
		Bcc: []string{"secret@example.com"}, Subject: "Hello ✓", Text: "plain body", MessageID: "key123@workflow",
		Date: time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC),
	}
}

func TestBuildHeadersEnvelopeAndBcc(t *testing.T) {
	m := goodMessage()
	envelope, data, err := m.Build()
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"From: \"Ops\" <ops@example.com>", "To: <a@example.com>, \"Bee\" <b@example.com>", "Cc: <c@example.com>",
		"Message-ID: <key123@workflow>", "MIME-Version: 1.0", "Content-Type: text/plain; charset=utf-8", "Date: Sun, 11 Oct 2026 09:00:00 +0000"} {
		if !strings.Contains(text, want) {
			t.Errorf("message lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "secret@example.com") {
		t.Fatal("a Bcc recipient appears in the headers")
	}
	if len(envelope.Recipients) != 4 || envelope.Recipients[3] != "secret@example.com" || envelope.From != "ops@example.com" {
		t.Fatalf("envelope = %+v, want the Bcc recipient in the envelope only", envelope)
	}
	if !strings.Contains(text, "Subject: =?utf-8?q?Hello_=E2=9C=93?=") {
		t.Errorf("subject is not RFC 2047 encoded:\n%s", text)
	}

	m.HTML = "<p>html body</p>"
	_, data, _ = m.Build()
	if !strings.Contains(string(data), "multipart/alternative") || !strings.Contains(string(data), "text/html") || !strings.Contains(string(data), "text/plain") {
		t.Fatalf("text+html must be multipart/alternative:\n%s", data)
	}
	m.Text = ""
	if _, data, _ = m.Build(); !strings.Contains(string(data), "Content-Type: text/html") {
		t.Fatalf("html only:\n%s", data)
	}
}

func TestBuildRejectsInjectionAndBadInput(t *testing.T) {
	for name, mutate := range map[string]func(*Message){
		"newline in subject":  func(m *Message) { m.Subject = "hi\r\nBcc: evil@example.com" },
		"newline in from":     func(m *Message) { m.From = "ops@example.com\r\nX-Evil: 1" },
		"newline in to":       func(m *Message) { m.To = []string{"a@example.com\nBcc: x@example.com"} },
		"newline in reply-to": func(m *Message) { m.ReplyTo = "a@example.com\r\nX: y" },
		"bad address":         func(m *Message) { m.To = []string{"not an address"} },
		"no recipients":       func(m *Message) { m.To, m.Cc, m.Bcc = nil, nil, nil },
		"too many recipients": func(m *Message) {
			m.To = make([]string, MaxRecipients+1)
			for i := range m.To {
				m.To[i] = fmt.Sprintf("u%d@example.com", i)
			}
		},
		"no body":        func(m *Message) { m.Text, m.HTML = "", "" },
		"body too large": func(m *Message) { m.Text = strings.Repeat("x", MaxBodyBytes+1) },
		"bad message id": func(m *Message) { m.MessageID = "a b>\r\n" },
	} {
		m := goodMessage()
		mutate(&m)
		if _, _, err := m.Build(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A body line starting with a dot is dot-stuffed by the SMTP client, and a
	// bare "\n" in the body does not terminate DATA early.
	m := goodMessage()
	m.Text = ".dangerous\nline two\r\n.\r\nQUIT"
	srv := startSMTP(t, nil)
	cfg := srv.config(TLSStartTLS)
	m.MessageID = ""
	if err := cfg.Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if got := srv.sent(); len(got) != 1 || !strings.Contains(got[0].Data, "dangerous") {
		t.Fatalf("messages = %+v, want exactly one with the whole body", got)
	}
}

func TestSMTPSendOverEveryTLSMode(t *testing.T) {
	for name, tt := range map[string]struct {
		mode      string
		implicit  bool
		offerTLS  bool
		wantError string
	}{
		"starttls":                 {mode: TLSStartTLS, offerTLS: true},
		"implicit tls":             {mode: TLSImplicit, implicit: true},
		"none on a loopback relay": {mode: TLSNone},
		"starttls not offered":     {mode: TLSStartTLS, offerTLS: false, wantError: "refusing to send credentials in clear"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := startSMTP(t, func(s *fakeSMTP) { s.implicit, s.offerTLS, s.user, s.pass = tt.implicit, tt.offerTLS, "mailer", "p@ss" })
			cfg := srv.config(tt.mode)
			cfg.Username, cfg.Password = "mailer", "p@ss"
			err := cfg.Send(context.Background(), goodMessage())
			if tt.wantError != "" {
				var smtpErr *Error
				if !errors.As(err, &smtpErr) || smtpErr.Temporary || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("err = %v, want a permanent error containing %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := srv.sent()
			if len(got) != 1 || got[0].From != "ops@example.com" || len(got[0].To) != 4 {
				t.Fatalf("messages = %+v", got)
			}
			if strings.Contains(got[0].Data, "Bcc:") || !strings.Contains(got[0].Data, "Message-ID: <key123@workflow>") {
				t.Fatalf("message data:\n%s", got[0].Data)
			}
			if len(srv.authSeen) != 1 || srv.authSeen[0] != "|mailer|p@ss" {
				t.Fatalf("auth = %v", srv.authSeen)
			}
		})
	}
}

func TestSMTPFailuresAreClassified(t *testing.T) {
	send := func(configure func(*fakeSMTP), cfgEdit func(*SMTPConfig)) error {
		srv := startSMTP(t, configure)
		cfg := srv.config(TLSStartTLS)
		if cfgEdit != nil {
			cfgEdit(&cfg)
		}
		return cfg.Send(context.Background(), goodMessage())
	}
	asErr := func(err error) *Error {
		t.Helper()
		var e *Error
		if !errors.As(err, &e) {
			t.Fatalf("err = %v, want *mailer.Error", err)
		}
		return e
	}
	if e := asErr(send(func(s *fakeSMTP) { s.rcptCode = 450 }, nil)); !e.Temporary || e.Code != 450 {
		t.Errorf("a 450 must be temporary: %+v", e)
	}
	if e := asErr(send(func(s *fakeSMTP) { s.rcptCode = 550 }, nil)); e.Temporary || e.Code != 550 {
		t.Errorf("a 550 must be permanent: %+v", e)
	}
	if e := asErr(send(func(s *fakeSMTP) { s.dataCode = 451 }, nil)); !e.Temporary {
		t.Errorf("a 451 after DATA must be temporary: %+v", e)
	}
	wrongPassword := func(s *fakeSMTP) { s.user, s.pass = "mailer", "right" }
	if e := asErr(send(wrongPassword, func(c *SMTPConfig) { c.Username, c.Password = "mailer", "wrong" })); e.Temporary || e.Code != 535 || !strings.Contains(e.Error(), "authentication failed") {
		t.Errorf("bad credentials must be permanent: %+v", e)
	}
	if e := asErr(send(nil, func(c *SMTPConfig) { c.Username, c.Password = "mailer", "x" })); e.Temporary || !strings.Contains(e.Error(), "does not support authentication") {
		t.Errorf("auth requested but not offered: %+v", e)
	}
	if e := asErr(send(nil, func(c *SMTPConfig) { c.TLSConfig = nil })); !strings.Contains(strings.ToLower(e.Error()), "certificate") {
		t.Errorf("an untrusted certificate must fail verification: %+v", e)
	}
	// A refused connection is temporary: the relay may come back.
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	port := closed.Addr().(*net.TCPAddr).Port
	closed.Close()
	if e := asErr((SMTPConfig{Host: "127.0.0.1", Port: port, TLS: TLSNone, Timeout: time.Second}).Send(context.Background(), goodMessage())); !e.Temporary {
		t.Errorf("a refused connection must be temporary: %+v", e)
	}
	// A password never goes over an unencrypted connection to a non-loopback host.
	plain := plainAuth{user: "u", password: "p", host: "mail.example.com", secure: false}
	if _, _, err := plain.Start(&smtpServerInfo); err == nil {
		t.Error("PLAIN over an unencrypted non-loopback connection was allowed")
	}
}

func TestCheckAndDialHook(t *testing.T) {
	srv := startSMTP(t, func(s *fakeSMTP) { s.user, s.pass = "mailer", "pw" })
	cfg := srv.config(TLSStartTLS)
	cfg.Username, cfg.Password = "mailer", "pw"
	var dialed []string
	cfg.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		dialed = append(dialed, address)
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	if err := cfg.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(dialed) != 1 || len(srv.sent()) != 0 {
		t.Fatalf("dialed %v, sent %d messages; Check must connect and authenticate but send nothing", dialed, len(srv.sent()))
	}
	cfg.Dial = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("egress blocked") }
	if err := cfg.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "egress blocked") {
		t.Fatalf("Check ignored the dial hook: %v", err)
	}
	_ = io.EOF
}
