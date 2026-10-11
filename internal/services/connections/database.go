package connections

import (
	"context"
	"fmt"
	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/internal/core/executor/database"

	"github.com/kartikey/workflow_service/pkg/dbx"
)

func databaseKind(name, title string, defaultPort int) kind {
	info := KindInfo{
		Kind: name, Title: title,
		Fields: []Field{
			{Name: "host", Title: "Host", Type: "string", Required: true},
			{Name: "port", Title: "Port", Type: "int", Default: defaultPort},
			{Name: "database", Title: "Database", Type: "string", Required: true},
			{Name: "username", Title: "Username", Type: "string", Required: true},
			{Name: "tls", Title: "Encryption", Type: "enum", Enum: []string{dbx.TLSVerifyFull, dbx.TLSRequire, dbx.TLSDisable}, Default: dbx.TLSVerifyFull,
				Help: "verify-full checks the certificate; require encrypts without checking (self-signed servers); disable sends in clear"},
			{Name: "password", Title: "Password", Type: "string", Required: true, Secret: true,
				Help: "use a database user with only the privileges the workflows need"},
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
			if _, err := intSetting(config, "port", defaultPort, 1, 65535); err != nil {
				return err
			}
			if _, err := stringSetting(config, "database", true, 128); err != nil {
				return err
			}
			if _, err := stringSetting(config, "username", true, 128); err != nil {
				return err
			}
			mode, err := stringSetting(config, "tls", false, 16)
			if err != nil {
				return err
			}
			switch mode {
			case "":
				config["tls"] = dbx.TLSVerifyFull
			case dbx.TLSVerifyFull, dbx.TLSRequire, dbx.TLSDisable:
			default:
				return fmt.Errorf("tls must be %s, %s or %s", dbx.TLSVerifyFull, dbx.TLSRequire, dbx.TLSDisable)
			}
			if secret["password"] == "" {
				return fmt.Errorf("password is required")
			}
			return nil
		},
		test: func(ctx context.Context, s *Service, config map[string]any, secret map[string]string) error {
			cfg := DatabaseSettings(name, config, secret)
			cfg.Dial = s.policy.DialContext
			conn, err := dbx.Open(ctx, cfg)
			if err != nil {
				return databaseFailure(err)
			}
			defer conn.Close()
			return databaseFailure(conn.Ping(ctx))
		},
	}
}

// DatabaseSettings turns a stored database connection into a dbx configuration;
// the connection test and the database nodes share it.
func DatabaseSettings(kindName string, config map[string]any, secret map[string]string) dbx.Config {
	return database.Config(executor.Connection{Kind: kindName, Config: config, Secret: secret})
}

// databaseFailure phrases a driver error for the person testing a connection.
func databaseFailure(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("database connection test: %w", err)
}
