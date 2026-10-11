// Package database implements the db.query and db.insert nodes (02 §4.13) for
// PostgreSQL and MySQL connections. Reads run in read-only transactions with a
// statement timeout and a row cap; values reach the database only as bind
// parameters, never as SQL text; every connection goes through the egress guard.
package database

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/pkg/dbx"
	"github.com/kartikey/workflow_service/pkg/netguard"
)

// Kinds are the connection kinds these nodes accept.
var Kinds = []string{dbx.Postgres, dbx.MySQL}

type base struct {
	conns  executor.Connections
	policy netguard.Policy
}

// Config turns a stored connection into a database configuration. The egress
// dialer is attached by the caller.
func Config(conn executor.Connection) dbx.Config {
	cfg := dbx.Config{Kind: conn.Kind, Password: conn.Secret["password"], TLS: dbx.TLSVerifyFull}
	cfg.Host, _ = conn.Config["host"].(string)
	cfg.Database, _ = conn.Config["database"].(string)
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

// open connects through the named connection. The caller must Close the result.
func (b base) open(ctx context.Context, req executor.Request, name string) (dbx.Conn, error) {
	if name == "" {
		return nil, executor.Permanent("bad_config", errors.New("connection is required"))
	}
	if b.conns == nil {
		return nil, executor.Permanent("bad_config", errors.New("saved connections are not available on this worker"))
	}
	conn, err := b.conns.Connection(ctx, req.TenantID, name, Kinds...)
	switch {
	case errors.Is(err, executor.ErrConnectionNotFound):
		return nil, executor.Permanent("E_CONNECTION_NOT_FOUND", fmt.Errorf("no postgres or mysql connection named %q", name))
	case err != nil:
		return nil, executor.Retryable("connection_lookup", err)
	}
	req.RedactSecrets(conn.Secret["password"])
	cfg := Config(conn)
	cfg.Dial = b.policy.DialContext
	db, err := dbx.Open(ctx, cfg)
	if err != nil {
		return nil, classify(ctx, err)
	}
	return db, nil
}

// classify maps a database failure onto the retry model.
func classify(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err() // DeadlineExceeded is a retryable timeout; Canceled is permanent
	}
	if errors.Is(err, netguard.ErrBlocked) {
		return executor.Permanent("egress_blocked", err)
	}
	var dbErr *dbx.Error
	if errors.As(err, &dbErr) {
		switch dbErr.Class {
		case dbx.Auth:
			return executor.Permanent("db_auth", err)
		case dbx.Statement:
			return executor.Permanent("db_statement", err)
		default:
			return executor.Retryable("db_transient", err)
		}
	}
	return executor.Retryable("db_transient", err)
}

// timeoutOf parses an optional duration setting.
func timeoutOf(text string) (time.Duration, error) {
	if text == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(text)
	if err != nil || d <= 0 {
		return 0, errors.New("timeout must be a positive duration such as 30s")
	}
	return min(d, dbx.MaxTimeout), nil
}

// literalInt requires an integer setting, when present, to be written literally.
func literalInt(config map[string]any, key string, lo, hi int) error {
	value, present := config[key]
	if !present || value == nil {
		return nil
	}
	if executor.IsExpression(value) {
		return nil // checked again once resolved
	}
	number, ok := value.(float64)
	if !ok || number != float64(int(number)) || int(number) < lo || int(number) > hi {
		return fmt.Errorf("%s must be an integer between %d and %d", key, lo, hi)
	}
	return nil
}

func plainIdentifier(s string) bool { return s != "" && !strings.ContainsAny(s, "\r\n\x00") }
