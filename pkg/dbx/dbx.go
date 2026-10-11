// Package dbx is a small client for PostgreSQL and MySQL used by the database
// nodes and the data-exploration agent tools. It is built for running on behalf
// of a tenant: every connection is dialled through a caller-supplied function (so
// the egress guard applies), reads run in read-only transactions with a statement
// timeout and a row cap, and table names are validated and quoted rather than
// trusted. Credentials are never put into a connection string, so they cannot
// leak through an error message.
package dbx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"
)

// Database kinds.
const (
	Postgres = "postgres"
	MySQL    = "mysql"
)

// TLS modes, with libpq's meanings: verify-full checks the certificate and host
// name; require encrypts without verifying (for self-signed servers); disable
// sends everything in clear.
const (
	TLSVerifyFull = "verify-full"
	TLSRequire    = "require"
	TLSDisable    = "disable"
)

// Defaults and hard limits.
const (
	DefaultMaxRows    = 1000
	MaxRowsLimit      = 10000
	DefaultTimeout    = 30 * time.Second
	MaxTimeout        = 5 * time.Minute
	MaxResultBytes    = 900 << 10 // stays under the engine's per-node output limit
	MaxInsertRows     = 1000
	connectTimeout    = 15 * time.Second
	maxParameterCount = 60000 // PostgreSQL allows 65535 bind parameters
)

// Config describes one database. Dial opens the TCP connection; the caller
// passes one that enforces the egress policy (nil uses a plain net.Dialer).
type Config struct {
	Kind     string
	Host     string
	Port     int
	Database string
	Username string
	Password string
	TLS      string
	Dial     func(ctx context.Context, network, address string) (net.Conn, error)
}

// QueryOptions bound a read.
type QueryOptions struct {
	MaxRows int           // default DefaultMaxRows, capped at MaxRowsLimit
	Timeout time.Duration // statement timeout, default DefaultTimeout, capped at MaxTimeout
}

func (o QueryOptions) normalized() QueryOptions {
	if o.MaxRows <= 0 {
		o.MaxRows = DefaultMaxRows
	}
	o.MaxRows = min(o.MaxRows, MaxRowsLimit)
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	o.Timeout = min(o.Timeout, MaxTimeout)
	return o
}

// Result is the outcome of a read. Truncated says the row cap or the size cap
// cut it short.
type Result struct {
	Columns   []string
	Rows      []map[string]any
	Truncated bool
}

// InsertOptions control an insert.
type InsertOptions struct {
	// Ignore skips rows that conflict with a unique key (ON CONFLICT DO NOTHING
	// or INSERT IGNORE), which makes a repeated insert harmless.
	Ignore  bool
	Timeout time.Duration
}

// Table names a table or view.
type Table struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
}

// Column describes a column.
type Column struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Nullable   bool   `json:"nullable"`
	PrimaryKey bool   `json:"primary_key"`
}

// Conn is an open connection. It is not safe for concurrent use.
type Conn interface {
	Ping(ctx context.Context) error
	// Query runs a single read-only statement.
	Query(ctx context.Context, sql string, params []any, opts QueryOptions) (Result, error)
	// Insert adds rows (all with the same keys) to table, in one transaction.
	Insert(ctx context.Context, table string, rows []map[string]any, opts InsertOptions) (int64, error)
	// Tables lists the tables and views visible to the user.
	Tables(ctx context.Context) ([]Table, error)
	// Describe lists a table's columns.
	Describe(ctx context.Context, table string) ([]Column, error)
	Close()
}

// Open connects to the database.
func Open(ctx context.Context, cfg Config) (Conn, error) {
	if cfg.Port == 0 {
		cfg.Port = map[string]int{Postgres: 5432, MySQL: 3306}[cfg.Kind]
	}
	if cfg.TLS == "" {
		cfg.TLS = TLSVerifyFull
	}
	if cfg.TLS != TLSVerifyFull && cfg.TLS != TLSRequire && cfg.TLS != TLSDisable {
		return nil, fmt.Errorf("tls must be %s, %s or %s", TLSVerifyFull, TLSRequire, TLSDisable)
	}
	if cfg.Host == "" {
		return nil, errors.New("host is required")
	}
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	switch cfg.Kind {
	case Postgres:
		return openPostgres(ctx, cfg)
	case MySQL:
		return openMySQL(ctx, cfg)
	}
	return nil, fmt.Errorf("unsupported database kind %q", cfg.Kind)
}

// Class says how a failure should be handled.
type Class int

const (
	// Transient failures may succeed on retry: the network, timeouts, deadlocks.
	Transient Class = iota
	// Auth failures will not improve on retry: wrong user or password.
	Auth
	// Statement failures are the query's own fault: syntax, missing table,
	// constraint violation, permission denied.
	Statement
)

// Error wraps a database failure with its class and the vendor's error code.
type Error struct {
	Class Class
	Code  string
	Err   error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// readOnlyStarts are the statements Query will run. The read-only transaction is
// the second layer, not the first: MySQL executes DDL (CREATE, DROP, ALTER) outside
// the transaction's read-only guarantee because it commits implicitly, so a
// statement that is not a plain read must be refused before it is sent.
var readOnlyStarts = map[string]bool{"select": true, "with": true, "show": true, "describe": true, "desc": true, "explain": true, "values": true, "table": true}

// outputsToServerFile matches SELECT ... INTO OUTFILE / DUMPFILE, which write to
// the database server's disk.
var outputsToServerFile = regexp.MustCompile(`(?i)\binto\s+(outfile|dumpfile)\b`)

// checkReadOnly refuses anything but a single plain read.
func checkReadOnly(sql string) error {
	text := strings.TrimSpace(sql)
	// Skip leading comments and opening parentheses ("(select 1) union ...").
	for {
		switch {
		case strings.HasPrefix(text, "--"):
			if i := strings.IndexByte(text, '\n'); i >= 0 {
				text = strings.TrimSpace(text[i+1:])
				continue
			}
			text = ""
		case strings.HasPrefix(text, "/*"):
			if i := strings.Index(text, "*/"); i >= 0 {
				text = strings.TrimSpace(text[i+2:])
				continue
			}
			text = ""
		case strings.HasPrefix(text, "("):
			text = strings.TrimSpace(text[1:])
			continue
		}
		break
	}
	word := text
	if i := strings.IndexFunc(text, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') }); i >= 0 {
		word = text[:i]
	}
	if !readOnlyStarts[strings.ToLower(word)] {
		return &Error{Class: Statement, Err: errors.New("only read statements (SELECT, WITH, SHOW, DESCRIBE, EXPLAIN) are allowed here")}
	}
	if outputsToServerFile.MatchString(text) {
		return &Error{Class: Statement, Err: errors.New("INTO OUTFILE and INTO DUMPFILE are not allowed")}
	}
	return nil
}

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)

// splitTable validates "table" or "schema.table".
func splitTable(name string) (schema, table string, err error) {
	parts := strings.Split(name, ".")
	if len(parts) > 2 {
		return "", "", fmt.Errorf("table %q must be table or schema.table", name)
	}
	for _, part := range parts {
		if !identifier.MatchString(part) {
			return "", "", fmt.Errorf("table name %q: each part must be letters, digits and underscores, starting with a letter or underscore", name)
		}
	}
	if len(parts) == 2 {
		return parts[0], parts[1], nil
	}
	return "", parts[0], nil
}

// columnNames returns the sorted column set shared by every row, rejecting rows
// that disagree and names that are not plain identifiers.
func columnNames(rows []map[string]any) ([]string, error) {
	if len(rows) == 0 {
		return nil, errors.New("no rows to insert")
	}
	if len(rows) > MaxInsertRows {
		return nil, fmt.Errorf("at most %d rows can be inserted at once", MaxInsertRows)
	}
	var names []string
	for name := range rows[0] {
		if !identifier.MatchString(name) {
			return nil, fmt.Errorf("column name %q must be letters, digits and underscores", name)
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, errors.New("rows have no columns")
	}
	sortStrings(names)
	for i, row := range rows[1:] {
		if len(row) != len(names) {
			return nil, fmt.Errorf("row %d has different columns from row 0", i+1)
		}
		for _, name := range names {
			if _, ok := row[name]; !ok {
				return nil, fmt.Errorf("row %d has no value for column %q", i+1, name)
			}
		}
	}
	if len(names)*len(rows) > maxParameterCount {
		return nil, errors.New("too many values in one insert; send fewer rows or columns")
	}
	return names, nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// normalizeParam turns JSON-decoded values into what drivers bind: integral
// numbers become integers, maps and lists stay for the driver to encode.
func normalizeParam(v any) any {
	if f, ok := v.(float64); ok && f == float64(int64(f)) && f > -9e15 && f < 9e15 {
		return int64(f)
	}
	return v
}

func normalizeParams(params []any) []any {
	out := make([]any, len(params))
	for i, p := range params {
		out[i] = normalizeParam(p)
	}
	return out
}

// sizeOf estimates a row's JSON size, to enforce MaxResultBytes without encoding.
func sizeOf(row map[string]any) int {
	n := 2
	for k, v := range row {
		n += len(k) + 4
		switch value := v.(type) {
		case string:
			n += len(value) + 2
		case nil:
			n += 4
		default:
			n += len(fmt.Sprint(value)) + 2
		}
	}
	return n
}
