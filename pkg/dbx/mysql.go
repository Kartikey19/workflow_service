package dbx

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

type mysqlConn struct {
	db   *sql.DB
	conn *sql.Conn
}

func openMySQL(ctx context.Context, cfg Config) (Conn, error) {
	mc := mysql.NewConfig()
	mc.User, mc.Passwd, mc.DBName = cfg.Username, cfg.Password, cfg.Database
	mc.Net, mc.Addr = "tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	mc.Timeout, mc.ReadTimeout, mc.WriteTimeout = connectTimeout, MaxTimeout+time.Minute, MaxTimeout+time.Minute
	mc.ParseTime, mc.MultiStatements, mc.InterpolateParams = true, false, false
	mc.Loc = time.UTC
	mc.AllowNativePasswords = true
	switch cfg.TLS {
	case TLSDisable:
		mc.TLS = nil
	case TLSRequire:
		mc.TLS = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} //nolint:gosec // "require": encrypt, do not verify
	default:
		mc.TLS = &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}
	}
	if cfg.Dial != nil {
		mc.DialFunc = cfg.Dial
	}
	connector, err := mysql.NewConnector(mc)
	if err != nil {
		return nil, classifyMySQL(err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, classifyMySQL(redactPassword(err, cfg.Password))
	}
	return &mysqlConn{db: db, conn: conn}, nil
}

func classifyMySQL(err error) error {
	if err == nil {
		return nil
	}
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		class := Statement
		switch myErr.Number {
		case 1045, 1044, 1698: // access denied
			class = Auth
		case 1205, 1213, 3024, 1317, 2002, 2003, 2006, 2013, 1040, 1203: // lock wait, deadlock, max_execution_time, interrupted, connection trouble, too many connections
			class = Transient
		}
		return &Error{Class: class, Code: strconv.Itoa(int(myErr.Number)), Err: err}
	}
	return &Error{Class: Transient, Err: err}
}

func (c *mysqlConn) Close() {
	_ = c.conn.Close()
	_ = c.db.Close()
}

func (c *mysqlConn) Ping(ctx context.Context) error {
	return classifyMySQL(c.conn.PingContext(ctx))
}

// readOnly runs fn in a read-only transaction with a SELECT time limit.
func (c *mysqlConn) readOnly(ctx context.Context, timeout time.Duration, fn func(*sql.Tx) error) error {
	// max_execution_time is in milliseconds and applies to SELECT statements.
	if _, err := c.conn.ExecContext(ctx, fmt.Sprintf("set session max_execution_time = %d", timeout.Milliseconds())); err != nil {
		return classifyMySQL(err)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout+5*time.Second) // belt and braces: the driver closes the connection
	defer cancel()
	tx, err := c.conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return classifyMySQL(err)
	}
	defer func() { _ = tx.Rollback() }()
	return classifyMySQL(fn(tx))
}

func (c *mysqlConn) Query(ctx context.Context, query string, params []any, opts QueryOptions) (Result, error) {
	opts = opts.normalized()
	if err := checkReadOnly(query); err != nil {
		return Result{}, err
	}
	var result Result
	err := c.readOnly(ctx, opts.Timeout, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, query, normalizeParams(params)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		types, err := rows.ColumnTypes()
		if err != nil {
			return err
		}
		result.Columns = make([]string, len(types))
		for i, t := range types {
			result.Columns[i] = t.Name()
		}
		size := 0
		for rows.Next() {
			if len(result.Rows) >= opts.MaxRows {
				result.Truncated = true
				break
			}
			values := make([]any, len(types))
			pointers := make([]any, len(types))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				return err
			}
			row := make(map[string]any, len(values))
			for i, v := range values {
				row[result.Columns[i]] = mysqlValue(types[i].DatabaseTypeName(), v)
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

// mysqlValue converts a scanned value using the column's type, since the text
// protocol delivers most values as bytes.
func mysqlValue(typeName string, v any) any {
	raw, ok := v.([]byte)
	if !ok {
		return jsonValue(v)
	}
	text := string(raw)
	switch strings.TrimPrefix(typeName, "UNSIGNED ") {
	case "TINYINT", "SMALLINT", "MEDIUMINT", "INT", "BIGINT", "YEAR":
		if i, err := strconv.ParseInt(text, 10, 64); err == nil {
			return i
		}
		if u, err := strconv.ParseUint(text, 10, 64); err == nil {
			return fmt.Sprint(u) // beyond int64: keep exact
		}
	case "FLOAT", "DOUBLE":
		if f, err := strconv.ParseFloat(text, 64); err == nil {
			return f
		}
	case "JSON":
		var decoded any
		if json.Unmarshal(raw, &decoded) == nil {
			return decoded
		}
	case "BLOB", "TINYBLOB", "MEDIUMBLOB", "LONGBLOB", "BINARY", "VARBINARY", "BIT", "GEOMETRY":
		return base64.StdEncoding.EncodeToString(raw)
	}
	return text
}

func quoteMy(identifier string) string { return "`" + identifier + "`" }

func (c *mysqlConn) Insert(ctx context.Context, table string, rows []map[string]any, opts InsertOptions) (int64, error) {
	schema, name, err := splitTable(table)
	if err != nil {
		return 0, &Error{Class: Statement, Err: err}
	}
	columns, err := columnNames(rows)
	if err != nil {
		return 0, &Error{Class: Statement, Err: err}
	}
	target := quoteMy(name)
	if schema != "" {
		target = quoteMy(schema) + "." + target
	}
	quoted := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = quoteMy(column)
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, min(timeout, MaxTimeout))
	defer cancel()
	var total int64
	err = func() error {
		tx, err := c.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		perStatement := max(1, maxParameterCount/len(columns))
		verb := "insert"
		if opts.Ignore {
			verb = "insert ignore"
		}
		for start := 0; start < len(rows); start += perStatement {
			chunk := rows[start:min(start+perStatement, len(rows))]
			var query strings.Builder
			args := make([]any, 0, len(chunk)*len(columns))
			fmt.Fprintf(&query, "%s into %s (%s) values ", verb, target, strings.Join(quoted, ", "))
			for i, row := range chunk {
				if i > 0 {
					query.WriteString(", ")
				}
				query.WriteString("(" + strings.TrimSuffix(strings.Repeat("?, ", len(columns)), ", ") + ")")
				for _, column := range columns {
					args = append(args, mysqlArg(row[column]))
				}
			}
			res, err := tx.ExecContext(ctx, query.String(), args...)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			total += n
		}
		return tx.Commit()
	}()
	return total, classifyMySQL(err)
}

// mysqlArg encodes objects and lists as JSON text, which MySQL JSON columns accept.
func mysqlArg(v any) any {
	switch value := normalizeParam(v).(type) {
	case map[string]any, []any:
		encoded, _ := json.Marshal(value)
		return string(encoded)
	default:
		return value
	}
}

func (c *mysqlConn) Tables(ctx context.Context) ([]Table, error) {
	var out []Table
	err := c.readOnly(ctx, DefaultTimeout, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select table_schema, table_name from information_schema.tables
			 where table_schema = database() order by table_name limit 500`)
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

func (c *mysqlConn) Describe(ctx context.Context, table string) ([]Column, error) {
	schema, name, err := splitTable(table)
	if err != nil {
		return nil, &Error{Class: Statement, Err: err}
	}
	var out []Column
	err = c.readOnly(ctx, DefaultTimeout, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select column_name, column_type, is_nullable = 'YES', column_key = 'PRI'
			  from information_schema.columns
			 where table_name = ? and table_schema = coalesce(nullif(?, ''), database())
			 order by ordinal_position`, name, schema)
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
