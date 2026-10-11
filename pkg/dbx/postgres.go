package dbx

import (
	"context"
	"crypto/tls"
	"database/sql/driver"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type postgresConn struct{ conn *pgx.Conn }

func openPostgres(ctx context.Context, cfg Config) (Conn, error) {
	pc, err := pgx.ParseConfig("")
	if err != nil {
		return nil, err
	}
	pc.Host, pc.Port, pc.Database, pc.User, pc.Password = cfg.Host, uint16(cfg.Port), cfg.Database, cfg.Username, cfg.Password
	pc.ConnectTimeout = connectTimeout
	pc.RuntimeParams["application_name"] = "workflow-service"
	switch cfg.TLS {
	case TLSDisable:
		pc.TLSConfig = nil
	case TLSRequire:
		pc.TLSConfig = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} //nolint:gosec // libpq's "require": encrypt, do not verify
	default:
		pc.TLSConfig = &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}
	}
	pc.Fallbacks = nil // never silently fall back to an unencrypted connection
	if cfg.Dial != nil {
		pc.DialFunc = cfg.Dial
	}
	conn, err := pgx.ConnectConfig(ctx, pc)
	if err != nil {
		return nil, classifyPostgres(redactPassword(err, cfg.Password))
	}
	return &postgresConn{conn: conn}, nil
}

// redactPassword keeps a password out of an error message, in case a driver or
// server quotes it.
func redactPassword(err error, password string) error {
	if err == nil || len(password) < 4 || !strings.Contains(err.Error(), password) {
		return err
	}
	return &redactedError{msg: strings.ReplaceAll(err.Error(), password, "***"), err: err}
}

// redactedError prints a message with the password masked but keeps the original
// chain, so errors.Is and errors.As (egress blocks, server error codes) still
// work for classification.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

func classifyPostgres(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		class := Statement
		switch {
		case pgErr.Code == "28P01" || pgErr.Code == "28000": // invalid password / authorization
			class = Auth
		case strings.HasPrefix(pgErr.Code, "08"), // connection exception
			strings.HasPrefix(pgErr.Code, "53"),          // insufficient resources
			strings.HasPrefix(pgErr.Code, "57"),          // operator intervention, incl. statement timeout 57014
			pgErr.Code == "40001", pgErr.Code == "40P01": // serialization failure, deadlock
			class = Transient
		}
		return &Error{Class: class, Code: pgErr.Code, Err: err}
	}
	// A failed connection attempt can wrap a PgError in a connect error; anything
	// else (dial, TLS, timeouts) is a network problem worth retrying.
	return &Error{Class: Transient, Err: err}
}

func (c *postgresConn) Close() { _ = c.conn.Close(context.Background()) }

func (c *postgresConn) Ping(ctx context.Context) error {
	var one int
	return classifyPostgres(c.conn.QueryRow(ctx, "select 1").Scan(&one))
}

// readOnly runs fn in a read-only transaction with a statement timeout.
func (c *postgresConn) readOnly(ctx context.Context, timeout time.Duration, fn func(pgx.Tx) error) error {
	tx, err := c.conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return classifyPostgres(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, fmt.Sprintf("set local statement_timeout = %d", timeout.Milliseconds())); err != nil {
		return classifyPostgres(err)
	}
	return classifyPostgres(fn(tx))
}

func (c *postgresConn) Query(ctx context.Context, sql string, params []any, opts QueryOptions) (Result, error) {
	opts = opts.normalized()
	if err := checkReadOnly(sql); err != nil {
		return Result{}, err
	}
	var result Result
	err := c.readOnly(ctx, opts.Timeout, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, normalizeParams(params)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for _, field := range rows.FieldDescriptions() {
			result.Columns = append(result.Columns, field.Name)
		}
		size := 0
		for rows.Next() {
			if len(result.Rows) >= opts.MaxRows {
				result.Truncated = true
				break
			}
			values, err := rows.Values()
			if err != nil {
				return err
			}
			row := make(map[string]any, len(values))
			for i, v := range values {
				row[result.Columns[i]] = jsonValue(v)
			}
			if size += sizeOf(row); size > MaxResultBytes {
				result.Truncated = true
				break
			}
			result.Rows = append(result.Rows, row)
		}
		return rows.Err()
	})
	return result, err
}

func quotePG(identifier string) string { return `"` + identifier + `"` }

func (c *postgresConn) Insert(ctx context.Context, table string, rows []map[string]any, opts InsertOptions) (int64, error) {
	schema, name, err := splitTable(table)
	if err != nil {
		return 0, &Error{Class: Statement, Err: err}
	}
	columns, err := columnNames(rows)
	if err != nil {
		return 0, &Error{Class: Statement, Err: err}
	}
	target := quotePG(name)
	if schema != "" {
		target = quotePG(schema) + "." + target
	}
	quoted := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = quotePG(column)
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	var total int64
	err = func() error {
		tx, err := c.conn.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
		if _, err := tx.Exec(ctx, fmt.Sprintf("set local statement_timeout = %d", min(timeout, MaxTimeout).Milliseconds())); err != nil {
			return err
		}
		perStatement := max(1, maxParameterCount/len(columns))
		for start := 0; start < len(rows); start += perStatement {
			chunk := rows[start:min(start+perStatement, len(rows))]
			var sql strings.Builder
			args := make([]any, 0, len(chunk)*len(columns))
			fmt.Fprintf(&sql, "insert into %s (%s) values ", target, strings.Join(quoted, ", "))
			for i, row := range chunk {
				if i > 0 {
					sql.WriteString(", ")
				}
				sql.WriteByte('(')
				for j, column := range columns {
					if j > 0 {
						sql.WriteString(", ")
					}
					args = append(args, normalizeParam(row[column]))
					fmt.Fprintf(&sql, "$%d", len(args))
				}
				sql.WriteByte(')')
			}
			if opts.Ignore {
				sql.WriteString(" on conflict do nothing")
			}
			tag, err := tx.Exec(ctx, sql.String(), args...)
			if err != nil {
				return err
			}
			total += tag.RowsAffected()
		}
		return tx.Commit(ctx)
	}()
	return total, classifyPostgres(err)
}

func (c *postgresConn) Tables(ctx context.Context) ([]Table, error) {
	var out []Table
	err := c.readOnly(ctx, DefaultTimeout, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			select table_schema, table_name from information_schema.tables
			 where table_schema not in ('pg_catalog', 'information_schema') and table_schema not like 'pg\_%'
			 order by table_schema, table_name limit 500`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t Table
			if err := rows.Scan(&t.Schema, &t.Name); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

func (c *postgresConn) Describe(ctx context.Context, table string) ([]Column, error) {
	schema, name, err := splitTable(table)
	if err != nil {
		return nil, &Error{Class: Statement, Err: err}
	}
	var out []Column
	err = c.readOnly(ctx, DefaultTimeout, func(tx pgx.Tx) error {
		// With no schema given, resolve the name the way the connection's
		// search_path would.
		rows, err := tx.Query(ctx, `
			select c.column_name, c.data_type, c.is_nullable = 'YES',
			       exists (select 1 from information_schema.table_constraints tc
			                 join information_schema.key_column_usage k
			                   on k.constraint_name = tc.constraint_name and k.table_schema = tc.table_schema
			                where tc.constraint_type = 'PRIMARY KEY' and tc.table_schema = c.table_schema
			                  and tc.table_name = c.table_name and k.column_name = c.column_name)
			  from information_schema.columns c
			 where c.table_name = $2 and c.table_schema = coalesce(nullif($1, ''), (select current_schema()))
			 order by c.ordinal_position`, schema, name)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var col Column
			if err := rows.Scan(&col.Name, &col.Type, &col.Nullable, &col.PrimaryKey); err != nil {
				return err
			}
			out = append(out, col)
		}
		return rows.Err()
	})
	if err == nil && len(out) == 0 {
		return nil, &Error{Class: Statement, Err: fmt.Errorf("table %q does not exist or is not visible", table)}
	}
	return out, err
}

// jsonValue converts a decoded database value into something that encodes to
// JSON without losing precision.
func jsonValue(v any) any {
	switch value := v.(type) {
	case nil, bool, string, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return value
	case time.Time:
		return value.UTC().Format(time.RFC3339Nano)
	case [16]byte:
		return fmt.Sprintf("%x-%x-%x-%x-%x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
	case []byte:
		if utf8.Valid(value) {
			return string(value)
		}
		return base64.StdEncoding.EncodeToString(value)
	case netip.Addr:
		return value.String()
	case netip.Prefix:
		return value.String()
	case map[string]any, []any:
		return value
	case driver.Valuer: // numeric, interval and friends decode to a Valuer
		if converted, err := value.Value(); err == nil {
			return jsonValue(converted)
		}
	}
	return fmt.Sprint(v)
}
