// Package mailer builds RFC 5322 messages and sends them over SMTP. It rejects
// anything that could inject headers, bounds the size of what it builds, and
// dials through a caller-supplied function so the egress guard applies.
package mailer

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"time"
)

// Limits on a single message.
const (
	MaxRecipients = 50
	MaxBodyBytes  = 1 << 20
)

// Message is one email. Text, HTML or both must be set.
type Message struct {
	From      string
	To        []string
	Cc        []string
	Bcc       []string
	ReplyTo   string
	Subject   string
	Text      string
	HTML      string
	MessageID string // optional; derive it from an idempotency key so a resend can be recognised
	Date      time.Time
}

// Address parses one address ("a@b.c" or "Name <a@b.c>") and rejects control
// characters, so it is safe to place in a header.
func Address(value string) (*mail.Address, error) {
	if strings.ContainsAny(value, "\r\n\x00") {
		return nil, errors.New("address contains a control character")
	}
	address, err := mail.ParseAddress(strings.TrimSpace(value))
	if err != nil {
		return nil, fmt.Errorf("invalid address %q", value)
	}
	return address, nil
}

// Envelope is what the SMTP transaction needs, separate from the headers: Bcc
// recipients appear here and nowhere in the message.
type Envelope struct {
	From       string
	Recipients []string
}

// Build validates the message and returns its envelope and wire form.
func (m Message) Build() (Envelope, []byte, error) {
	from, err := Address(m.From)
	if err != nil {
		return Envelope{}, nil, fmt.Errorf("from: %w", err)
	}
	var recipients []string
	collect := func(label string, values []string) ([]*mail.Address, error) {
		out := make([]*mail.Address, 0, len(values))
		for _, value := range values {
			address, err := Address(value)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", label, err)
			}
			out = append(out, address)
			recipients = append(recipients, address.Address)
		}
		return out, nil
	}
	to, err := collect("to", m.To)
	if err != nil {
		return Envelope{}, nil, err
	}
	cc, err := collect("cc", m.Cc)
	if err != nil {
		return Envelope{}, nil, err
	}
	if _, err := collect("bcc", m.Bcc); err != nil {
		return Envelope{}, nil, err
	}
	switch {
	case len(recipients) == 0:
		return Envelope{}, nil, errors.New("at least one recipient is required")
	case len(recipients) > MaxRecipients:
		return Envelope{}, nil, fmt.Errorf("at most %d recipients are allowed", MaxRecipients)
	case m.Text == "" && m.HTML == "":
		return Envelope{}, nil, errors.New("text or html is required")
	case len(m.Text)+len(m.HTML) > MaxBodyBytes:
		return Envelope{}, nil, fmt.Errorf("the body is over %d bytes", MaxBodyBytes)
	case strings.ContainsAny(m.Subject, "\r\n\x00"):
		return Envelope{}, nil, errors.New("subject contains a line break")
	}
	var replyTo *mail.Address
	if m.ReplyTo != "" {
		if replyTo, err = Address(m.ReplyTo); err != nil {
			return Envelope{}, nil, fmt.Errorf("reply_to: %w", err)
		}
	}
	messageID := m.MessageID
	if messageID == "" {
		messageID = randomID() + "@" + domainOf(from.Address)
	}
	if strings.ContainsAny(messageID, "\r\n<> \x00") {
		return Envelope{}, nil, errors.New("message id contains an illegal character")
	}
	date := m.Date
	if date.IsZero() {
		date = time.Now()
	}

	var buf bytes.Buffer
	header := func(name, value string) { fmt.Fprintf(&buf, "%s: %s\r\n", name, value) }
	header("From", from.String())
	header("To", joinAddresses(to))
	if len(cc) > 0 {
		header("Cc", joinAddresses(cc))
	}
	if replyTo != nil {
		header("Reply-To", replyTo.String())
	}
	header("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	header("Date", date.Format(time.RFC1123Z))
	header("Message-ID", "<"+messageID+">")
	header("MIME-Version", "1.0")
	if err := writeBody(&buf, m); err != nil {
		return Envelope{}, nil, err
	}
	return Envelope{From: from.Address, Recipients: recipients}, buf.Bytes(), nil
}

func joinAddresses(addresses []*mail.Address) string {
	parts := make([]string, len(addresses))
	for i, a := range addresses {
		parts[i] = a.String()
	}
	return strings.Join(parts, ", ")
}

func domainOf(address string) string {
	if at := strings.LastIndexByte(address, '@'); at >= 0 {
		return address[at+1:]
	}
	return "localhost"
}

func randomID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func writeBody(buf *bytes.Buffer, m Message) error {
	switch {
	case m.Text != "" && m.HTML != "":
		boundary := randomID()
		fmt.Fprintf(buf, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", boundary)
		w := multipart.NewWriter(buf)
		if err := w.SetBoundary(boundary); err != nil {
			return err
		}
		for _, part := range []struct{ contentType, body string }{{"text/plain", m.Text}, {"text/html", m.HTML}} {
			h := textproto.MIMEHeader{}
			h.Set("Content-Type", part.contentType+"; charset=utf-8")
			h.Set("Content-Transfer-Encoding", "quoted-printable")
			pw, err := w.CreatePart(h)
			if err != nil {
				return err
			}
			if err := writeQP(pw, part.body); err != nil {
				return err
			}
		}
		return w.Close()
	default:
		contentType, body := "text/plain", m.Text
		if m.HTML != "" {
			contentType, body = "text/html", m.HTML
		}
		fmt.Fprintf(buf, "Content-Type: %s; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", contentType)
		return writeQP(buf, body)
	}
}

func writeQP(w interface{ Write([]byte) (int, error) }, body string) error {
	qp := quotedprintable.NewWriter(w)
	if _, err := qp.Write([]byte(body)); err != nil {
		return err
	}
	return qp.Close()
}
