package connections

import (
	"context"
	"errors"
	"github.com/kartikey/workflow_service/internal/core/executor/connectors"
	"github.com/kartikey/workflow_service/pkg/mongox"
	"strings"
)

func tokenKind(name, title string) kind {
	info := KindInfo{Kind: name, Title: title, Fields: []Field{{Name: "access_token", Title: "Integration token", Type: "string", Required: true, Secret: true, Help: "Slack: bot token with chat:write and the history scopes you need. Notion: integration token; share the pages it should access."}}}
	return kind{info: info, hintField: "access_token", check: func(config map[string]any, secret map[string]string) error {
		if err := rejectUnknown(config, secret, info); err != nil {
			return err
		}
		token := secret["access_token"]
		if token == "" || len(token) > 8192 || strings.ContainsAny(token, "\r\n\x00") {
			return errors.New("access_token is required and must not contain control characters")
		}
		return nil
	}, test: func(ctx context.Context, s *Service, config map[string]any, secret map[string]string) error {
		return connectors.ProbeToken(ctx, name, secret["access_token"], s.policy, s.opts.Endpoints)
	}}
}
func mongoKind() kind {
	info := KindInfo{Kind: KindMongo, Title: "MongoDB database", Fields: []Field{
		{Name: "database", Title: "Database", Type: "string", Required: true},
		{Name: "tls", Title: "Encryption", Type: "enum", Enum: []string{"verify-full", "disable"}, Default: "verify-full", Help: "Certificate verification is enabled by default. Use disable only for a trusted local server."},
		{Name: "uri", Title: "Connection URI", Type: "string", Required: true, Secret: true, Help: "mongodb:// or mongodb+srv:// including credentials. TLS is controlled by the Encryption field."},
	}}
	return kind{info: info, check: func(config map[string]any, secret map[string]string) error {
		if err := rejectUnknown(config, secret, info); err != nil {
			return err
		}
		if config["tls"] == nil || config["tls"] == "" {
			config["tls"] = "verify-full"
		}
		return mongox.Validate(connectors.MongoSettings(config, secret))
	}, test: func(ctx context.Context, s *Service, config map[string]any, secret map[string]string) error {
		cfg := connectors.MongoSettings(config, secret)
		cfg.Dial = s.policy.DialContext
		conn, err := mongox.Open(ctx, cfg)
		if err != nil {
			return err
		}
		defer conn.Close()
		return nil
	}}
}
