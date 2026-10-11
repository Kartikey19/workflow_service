package connections

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kartikey/workflow_service/pkg/mailer"
)

func smtpKind() kind {
	info := KindInfo{
		Kind: KindSMTP, Title: "SMTP server",
		Fields: []Field{
			{Name: "host", Title: "Host", Type: "string", Required: true},
			{Name: "port", Title: "Port", Type: "int", Default: 587},
			{Name: "tls", Title: "Encryption", Type: "enum", Enum: []string{mailer.TLSStartTLS, mailer.TLSImplicit, mailer.TLSNone}, Default: mailer.TLSStartTLS,
				Help: "starttls (port 587), tls (port 465), or none for a trusted local relay"},
			{Name: "username", Title: "Username", Type: "string"},
			{Name: "from", Title: "Default sender", Type: "string", Help: "e.g. Ops <ops@example.com>"},
			{Name: "password", Title: "Password", Type: "string", Secret: true},
		},
	}
	return kind{
		info: info, hintField: "password",
		check: func(config map[string]any, secret map[string]string) error {
			if err := rejectUnknown(config, secret, info); err != nil {
				return err
			}
			host, err := stringSetting(config, "host", true, 253)
			if err != nil {
				return err
			}
			if !validHost(host) {
				return fmt.Errorf("host %q is not a valid host name", host)
			}
			port, err := intSetting(config, "port", 587, 1, 65535)
			if err != nil {
				return err
			}
			mode, err := stringSetting(config, "tls", false, 16)
			if err != nil {
				return err
			}
			switch mode {
			case "":
				mode = mailer.TLSStartTLS
				if port == 465 {
					mode = mailer.TLSImplicit
				}
				config["tls"] = mode
			case mailer.TLSStartTLS, mailer.TLSImplicit, mailer.TLSNone:
			default:
				return fmt.Errorf("tls must be %s, %s or %s", mailer.TLSStartTLS, mailer.TLSImplicit, mailer.TLSNone)
			}
			username, err := stringSetting(config, "username", false, 320)
			if err != nil {
				return err
			}
			if username != "" && secret["password"] == "" {
				return fmt.Errorf("password is required when a username is set")
			}
			if username == "" && secret["password"] != "" {
				return fmt.Errorf("a password needs a username")
			}
			from, err := stringSetting(config, "from", false, 320)
			if err != nil {
				return err
			}
			if from != "" {
				if _, err := mailer.Address(from); err != nil {
					return fmt.Errorf("from: %w", err)
				}
			}
			return nil
		},
		test: func(ctx context.Context, s *Service, config map[string]any, secret map[string]string) error {
			cfg := SMTPSettings(config, secret)
			cfg.Dial = s.policy.DialContext
			return cfg.Check(ctx)
		},
	}
}

// SMTPSettings turns a stored SMTP connection into a mailer configuration; the
// executor and the connection test share it.
func SMTPSettings(config map[string]any, secret map[string]string) mailer.SMTPConfig {
	cfg := mailer.SMTPConfig{Password: secret["password"], TLS: mailer.TLSStartTLS, Port: 587}
	cfg.Host, _ = config["host"].(string)
	cfg.Username, _ = config["username"].(string)
	if mode, ok := config["tls"].(string); ok && mode != "" {
		cfg.TLS = mode
	}
	switch port := config["port"].(type) {
	case int:
		cfg.Port = port
	case float64:
		cfg.Port = int(port)
	}
	return cfg
}

func resendKind() kind {
	info := KindInfo{
		Kind: KindResend, Title: "Resend",
		Fields: []Field{
			{Name: "from", Title: "Default sender", Type: "string", Help: "must be on a domain verified in Resend"},
			{Name: "api_key", Title: "API key", Type: "string", Required: true, Secret: true},
		},
	}
	return kind{
		info: info, hintField: "api_key",
		check: func(config map[string]any, secret map[string]string) error {
			if err := rejectUnknown(config, secret, info); err != nil {
				return err
			}
			if secret["api_key"] == "" {
				return fmt.Errorf("api_key is required")
			}
			from, err := stringSetting(config, "from", false, 320)
			if err != nil {
				return err
			}
			if from != "" {
				if _, err := mailer.Address(from); err != nil {
					return fmt.Errorf("from: %w", err)
				}
			}
			return nil
		},
		test: func(ctx context.Context, s *Service, config map[string]any, secret map[string]string) error {
			ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.endpoints.Resend+"/domains", nil)
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+secret["api_key"])
			res, err := s.policy.Client(20 * time.Second).Do(req)
			if err != nil {
				return fmt.Errorf("Resend could not be reached")
			}
			defer res.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
			switch {
			case res.StatusCode == http.StatusUnauthorized && strings.Contains(string(body), "restricted_api_key"):
				return nil // a sending-only key is valid; it just may not list domains
			case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
				return fmt.Errorf("Resend rejected the API key (HTTP %d)", res.StatusCode)
			case res.StatusCode >= 400:
				return fmt.Errorf("Resend answered HTTP %d", res.StatusCode)
			}
			return nil
		},
	}
}
