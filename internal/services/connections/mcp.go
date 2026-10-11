package connections

import (
	"context"
	"github.com/kartikey/workflow_service/pkg/mcpclient"
	"time"
)

func mcpKind() kind {
	info := KindInfo{Kind: KindMCP, Title: "MCP server", Fields: []Field{
		{Name: "url", Title: "Server URL", Type: "string", Required: true},
		{Name: "transport", Title: "Transport", Type: "enum", Enum: []string{"streamable_http", "sse"}, Default: "streamable_http"},
		{Name: "token", Title: "Bearer token", Type: "string", Secret: true},
	}}
	return kind{info: info, hintField: "token", check: func(config map[string]any, secret map[string]string) error {
		if err := rejectUnknown(config, secret, info); err != nil {
			return err
		}
		target, err := stringSetting(config, "url", true, 2048)
		if err != nil {
			return err
		}
		transport, err := stringSetting(config, "transport", false, 32)
		if err != nil {
			return err
		}
		if transport == "" {
			transport = "streamable_http"
			config["transport"] = transport
		}
		return mcpclient.Validate(mcpclient.Config{URL: target, Transport: transport, Token: secret["token"]})
	}, test: func(ctx context.Context, s *Service, config map[string]any, secret map[string]string) error {
		target, _ := config["url"].(string)
		transport, _ := config["transport"].(string)
		client := s.policy.Client(2 * time.Minute)
		defer client.CloseIdleConnections()
		session, err := mcpclient.Connect(ctx, mcpclient.Config{URL: target, Transport: transport, Token: secret["token"]}, client)
		if err != nil {
			return err
		}
		defer session.Close()
		_, err = session.ListTools(ctx)
		return err
	}}
}
