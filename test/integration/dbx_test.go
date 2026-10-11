//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"

	"github.com/kartikey/workflow_service/pkg/dbx"
)

// backend is one database under test. MySQL runs only when MYSQL_URL is set
// (for example `app:app-password@tcp(127.0.0.1:53306)/wfsvc`).
type backend struct {
	name string
	cfg  dbx.Config
	// setup creates the tables of the test in an isolated namespace and returns
	// the (possibly schema-qualified) table names.
	setup func(t *testing.T) tables
	// slow is a statement that sleeps for 5 seconds.
	slow string
	// series returns a query producing n rows.
	series func(n int) string
}

type tables struct{ events, jsons string }

func postgresBackend(t *testing.T) backend {
	t.Helper()
	parsed, err := url.Parse(os.Getenv("DATABASE_URL"))
	if err != nil || os.Getenv("DATABASE_URL") == "" {
		t.Fatal("DATABASE_URL is required")
	}
	port, _ := strconv.Atoi(parsed.Port())
	password, _ := parsed.User.Password()
	cfg := dbx.Config{Kind: dbx.Postgres, Host: parsed.Hostname(), Port: port, Database: strings.TrimPrefix(parsed.Path, "/"),
		Username: parsed.User.Username(), Password: password, TLS: dbx.TLSDisable}
	return backend{
		name: "postgres", cfg: cfg, slow: "select pg_sleep(5)",
		series: func(n int) string { return fmt.Sprintf("select g as n from generate_series(1, %d) g", n) },
		setup: func(t *testing.T) tables {
			schema := "dbx_" + randomHex(t, 4)
			ctx := context.Background()
			conn, err := pgx.Connect(ctx, os.Getenv("DATABASE_URL"))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close(ctx)
			for _, statement := range []string{
				`create schema ` + schema,
				`create table ` + schema + `.events (id integer primary key, name text not null, score numeric(12,4), happened_at timestamptz, active boolean, note text, extra jsonb, raw bytea, uid uuid)`,
				`create table ` + schema + `.jsons (k text primary key, v jsonb)`,
				`insert into ` + schema + `.events values (1, 'first', 12345678.9012, '2026-10-11T09:00:00Z', true, null, '{"a":[1,2]}', '\xdeadbeef', '0192f8a0-0000-7000-8000-000000000001')`,
			} {
				if _, err := conn.Exec(ctx, statement); err != nil {
					t.Fatalf("%s: %v", statement, err)
				}
			}
			t.Cleanup(func() {
				c, err := pgx.Connect(context.Background(), os.Getenv("DATABASE_URL"))
				if err == nil {
					_, _ = c.Exec(context.Background(), "drop schema "+schema+" cascade")
					c.Close(context.Background())
				}
			})
			return tables{events: schema + ".events", jsons: schema + ".jsons"}
		},
	}
}

func mysqlBackend(t *testing.T) (backend, bool) {
	t.Helper()
	dsn := os.Getenv("MYSQL_URL")
	if dsn == "" {
		return backend{}, false
	}
	// user:password@tcp(host:port)/database
	at := strings.LastIndex(dsn, "@tcp(")
	rest := dsn[at+len("@tcp("):]
	hostPort, database, _ := strings.Cut(rest, ")/")
	host, portText, _ := net.SplitHostPort(hostPort)
	port, _ := strconv.Atoi(portText)
	user, password, _ := strings.Cut(dsn[:at], ":")
	cfg := dbx.Config{Kind: dbx.MySQL, Host: host, Port: port, Database: database, Username: user, Password: password, TLS: dbx.TLSDisable}
	return backend{
		name: "mysql", cfg: cfg,
		// SLEEP() is interrupted without an error, so use a statement that is genuinely slow.
		slow: "select count(*) as c from information_schema.columns a, information_schema.columns b, information_schema.columns c, information_schema.columns d",
		series: func(n int) string {
			return fmt.Sprintf("with recursive s(n) as (select 1 union all select n + 1 from s where n < %d) select /*+ SET_VAR(cte_max_recursion_depth=20000) */ n from s", n)
		},
		setup: func(t *testing.T) tables {
			suffix := randomHex(t, 4)
			events, jsons := "events_"+suffix, "jsons_"+suffix
			db, err := sql.Open("mysql", fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true&multiStatements=true", user, password, hostPort, database))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			for _, statement := range []string{
				`create table ` + events + ` (id int primary key, name varchar(100) not null, score decimal(12,4), happened_at datetime, active boolean, note text, extra json, raw blob, uid char(36))`,
				`create table ` + jsons + ` (k varchar(50) primary key, v json)`,
				`insert into ` + events + ` values (1, 'first', 12345678.9012, '2026-10-11 09:00:00', true, null, '{"a":[1,2]}', 0xdeadbeef, '0192f8a0-0000-7000-8000-000000000001')`,
			} {
				if _, err := db.Exec(statement); err != nil {
					t.Fatalf("%s: %v", statement, err)
				}
			}
			t.Cleanup(func() {
				if db, err := sql.Open("mysql", fmt.Sprintf("%s:%s@tcp(%s)/%s", user, password, hostPort, database)); err == nil {
					_, _ = db.Exec("drop table if exists " + events)
					_, _ = db.Exec("drop table if exists " + jsons)
					db.Close()
				}
			})
			return tables{events: events, jsons: jsons}
		},
	}, true
}

func eachBackend(t *testing.T, run func(t *testing.T, b backend, tb tables)) {
	backends := []backend{postgresBackend(t)}
	if my, ok := mysqlBackend(t); ok {
		backends = append(backends, my)
	}
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			run(t, b, b.setup(t))
		})
	}
}

func open(t *testing.T, cfg dbx.Config) dbx.Conn {
	t.Helper()
	conn, err := dbx.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(conn.Close)
	return conn
}

func classOf(t *testing.T, err error) dbx.Class {
	t.Helper()
	var dbErr *dbx.Error
	if !errors.As(err, &dbErr) {
		t.Fatalf("err = %v, want *dbx.Error", err)
	}
	return dbErr.Class
}

func TestDBXQueryTypesAndParameters(t *testing.T) {
	eachBackend(t, func(t *testing.T, b backend, tb tables) {
		conn := open(t, b.cfg)
		if err := conn.Ping(context.Background()); err != nil {
			t.Fatal(err)
		}
		res, err := conn.Query(context.Background(), "select id, name, score, happened_at, active, note, extra, raw, uid from "+tb.events+" where id = "+placeholder(b, 1)+" and name = "+placeholder(b, 2), []any{float64(1), "first"}, dbx.QueryOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Rows) != 1 || res.Truncated {
			t.Fatalf("rows = %v truncated=%v", res.Rows, res.Truncated)
		}
		row := res.Rows[0]
		if fmt.Sprint(row["id"]) != "1" || row["name"] != "first" || row["score"] != "12345678.9012" {
			t.Errorf("basic columns = %v; numeric/decimal must stay an exact string", row)
		}
		if row["note"] != nil {
			t.Errorf("a NULL must come back as nil: %v", row["note"])
		}
		if fmt.Sprint(row["active"]) != "true" && fmt.Sprint(row["active"]) != "1" {
			t.Errorf("active = %v", row["active"])
		}
		if ts, _ := row["happened_at"].(string); !strings.HasPrefix(ts, "2026-10-11T09:00:00") {
			t.Errorf("timestamp = %v, want RFC 3339", row["happened_at"])
		}
		extra, _ := row["extra"].(map[string]any)
		if arr, _ := extra["a"].([]any); len(arr) != 2 {
			t.Errorf("json = %v, want a decoded object", row["extra"])
		}
		if row["raw"] != "3q2+7w==" {
			t.Errorf("binary = %v, want base64", row["raw"])
		}
		if row["uid"] != "0192f8a0-0000-7000-8000-000000000001" {
			t.Errorf("uuid = %v", row["uid"])
		}
		if want := []string{"id", "name", "score", "happened_at", "active", "note", "extra", "raw", "uid"}; strings.Join(res.Columns, ",") != strings.Join(want, ",") {
			t.Errorf("columns = %v", res.Columns)
		}
	})
}

func placeholder(b backend, n int) string {
	if b.name == "postgres" {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}

func TestDBXReadsAreReadOnlyAndSingleStatement(t *testing.T) {
	eachBackend(t, func(t *testing.T, b backend, tb tables) {
		conn := open(t, b.cfg)
		for _, statement := range []string{
			"insert into " + tb.events + " (id, name) values (99, 'sneaky')",
			"update " + tb.events + " set name = 'changed'",
			"delete from " + tb.events,
			"create table " + tb.events + "_x (a int)",
			"drop table " + tb.events,
			"select 1; drop table " + tb.events,
		} {
			_, err := conn.Query(context.Background(), statement, nil, dbx.QueryOptions{})
			if err == nil {
				t.Errorf("%q was allowed in a read", statement)
				continue
			}
			if class := classOf(t, err); class != dbx.Statement {
				t.Errorf("%q: class %v, want Statement", statement, class)
			}
		}
		// Nothing changed.
		res, err := conn.Query(context.Background(), "select name from "+tb.events, nil, dbx.QueryOptions{})
		if err != nil || len(res.Rows) != 1 || res.Rows[0]["name"] != "first" {
			t.Fatalf("data after the attempts: %v %v", res.Rows, err)
		}
	})
}

func TestDBXRowCapAndTimeout(t *testing.T) {
	eachBackend(t, func(t *testing.T, b backend, tb tables) {
		conn := open(t, b.cfg)
		res, err := conn.Query(context.Background(), b.series(5000), nil, dbx.QueryOptions{MaxRows: 100})
		if err != nil || len(res.Rows) != 100 || !res.Truncated {
			t.Fatalf("rows = %d truncated=%v err=%v; want 100 rows, truncated", len(res.Rows), res.Truncated, err)
		}
		if res, err = conn.Query(context.Background(), b.series(50), nil, dbx.QueryOptions{MaxRows: 100}); err != nil || len(res.Rows) != 50 || res.Truncated {
			t.Fatalf("under the cap: rows = %d truncated=%v err=%v", len(res.Rows), res.Truncated, err)
		}
		// The hard limit applies even when a larger cap is asked for.
		if res, err = conn.Query(context.Background(), b.series(12000), nil, dbx.QueryOptions{MaxRows: 1_000_000}); err != nil || len(res.Rows) != dbx.MaxRowsLimit || !res.Truncated {
			t.Fatalf("hard cap: rows = %d truncated=%v err=%v", len(res.Rows), res.Truncated, err)
		}
		start := time.Now()
		_, err = conn.Query(context.Background(), b.slow, nil, dbx.QueryOptions{Timeout: 300 * time.Millisecond})
		if err == nil || time.Since(start) > 3*time.Second {
			t.Fatalf("the slow statement ran %v with err %v; want it cut off", time.Since(start), err)
		}
		if class := classOf(t, err); class != dbx.Transient {
			t.Fatalf("a timeout is %v, want Transient", class)
		}
		// The connection is still usable after a timeout.
		if _, err := openFresh(t, b).Query(context.Background(), "select 1", nil, dbx.QueryOptions{}); err != nil {
			t.Fatal(err)
		}
	})
}

func openFresh(t *testing.T, b backend) dbx.Conn { return open(t, b.cfg) }

func TestDBXInsert(t *testing.T) {
	eachBackend(t, func(t *testing.T, b backend, tb tables) {
		conn := open(t, b.cfg)
		ctx := context.Background()
		n, err := conn.Insert(ctx, tb.events, []map[string]any{
			{"id": float64(2), "name": "ünïcode 😀", "score": 1.5, "active": true, "note": nil, "extra": nil},
			{"id": float64(3), "name": "third", "score": float64(7), "active": false, "note": "x", "extra": nil},
		}, dbx.InsertOptions{})
		if err != nil || n != 2 {
			t.Fatalf("insert = %d, %v", n, err)
		}
		res, err := conn.Query(ctx, "select name, score from "+tb.events+" where id = "+placeholder(b, 1), []any{float64(2)}, dbx.QueryOptions{})
		if err != nil || len(res.Rows) != 1 || res.Rows[0]["name"] != "ünïcode 😀" {
			t.Fatalf("read back: %v, %v", res.Rows, err)
		}
		// JSON values go into JSON columns.
		if _, err := conn.Insert(ctx, tb.jsons, []map[string]any{{"k": "a", "v": map[string]any{"deep": []any{1.0, "two", nil}}}}, dbx.InsertOptions{}); err != nil {
			t.Fatal(err)
		}
		res, _ = conn.Query(ctx, "select v from "+tb.jsons, nil, dbx.QueryOptions{})
		if v, _ := res.Rows[0]["v"].(map[string]any); len(v["deep"].([]any)) != 3 {
			t.Fatalf("json round trip = %v", res.Rows[0]["v"])
		}
		// A duplicate key is a statement error, or ignored on request, and a failure rolls everything back.
		dup := []map[string]any{{"id": float64(10), "name": "ok"}, {"id": float64(1), "name": "dup"}}
		if _, err := conn.Insert(ctx, tb.events, dup, dbx.InsertOptions{}); err == nil || classOf(t, err) != dbx.Statement {
			t.Fatalf("a duplicate key = %v", err)
		}
		res, _ = conn.Query(ctx, "select count(*) as c from "+tb.events+" where id = 10", nil, dbx.QueryOptions{})
		if fmt.Sprint(res.Rows[0]["c"]) != "0" {
			t.Fatal("a failed insert left rows behind; it must be one transaction")
		}
		if n, err := conn.Insert(ctx, tb.events, dup, dbx.InsertOptions{Ignore: true}); err != nil || n != 1 {
			t.Fatalf("ignore duplicates = %d, %v; want 1 inserted", n, err)
		}
		// Identifiers are validated, never interpolated blindly.
		for name, tt := range map[string]struct {
			table string
			rows  []map[string]any
		}{
			"table with a statement": {tb.events + "; drop table " + tb.events, []map[string]any{{"id": 1}}},
			"quoted table":           {`"` + tb.events + `"`, []map[string]any{{"id": 1}}},
			"three-part name":        {"a.b.c", []map[string]any{{"id": 1}}},
			"column with a quote":    {tb.events, []map[string]any{{`id"; drop table x; --`: 1}}},
			"column with a space":    {tb.events, []map[string]any{{"na me": 1}}},
			"ragged rows":            {tb.events, []map[string]any{{"id": 20}, {"id": 21, "name": "x"}}},
			"no rows":                {tb.events, nil},
			"too many rows":          {tb.events, make([]map[string]any, dbx.MaxInsertRows+1)},
		} {
			if _, err := conn.Insert(ctx, tt.table, tt.rows, dbx.InsertOptions{}); err == nil || classOf(t, err) != dbx.Statement {
				t.Errorf("%s: err = %v, want a Statement error", name, err)
			}
		}
		res, _ = conn.Query(ctx, "select count(*) as c from "+tb.events, nil, dbx.QueryOptions{})
		if fmt.Sprint(res.Rows[0]["c"]) != "4" { // ids 1, 2, 3 and 10
			t.Fatalf("row count after the rejected inserts = %v, want 4", res.Rows[0]["c"])
		}
	})
}

func TestDBXTablesAndDescribe(t *testing.T) {
	eachBackend(t, func(t *testing.T, b backend, tb tables) {
		conn := open(t, b.cfg)
		tables, err := conn.Tables(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, table := range tables {
			qualified := table.Name
			if strings.Contains(tb.events, ".") {
				qualified = table.Schema + "." + table.Name
			}
			if qualified == tb.events {
				found = true
			}
			if table.Schema == "pg_catalog" || table.Schema == "information_schema" {
				t.Errorf("system table listed: %v", table)
			}
		}
		if !found {
			t.Fatalf("%s is not in %v", tb.events, tables)
		}
		columns, err := conn.Describe(context.Background(), tb.events)
		if err != nil || len(columns) != 9 || columns[0].Name != "id" || !columns[0].PrimaryKey || columns[1].Nullable {
			t.Fatalf("columns = %+v, %v", columns, err)
		}
		if _, err := conn.Describe(context.Background(), tb.events+"_missing"); err == nil || classOf(t, err) != dbx.Statement {
			t.Fatalf("a missing table = %v", err)
		}
		if _, err := conn.Describe(context.Background(), "bad name!"); err == nil {
			t.Fatal("an invalid table name was accepted")
		}
	})
}

func TestDBXConnectionFailures(t *testing.T) {
	eachBackend(t, func(t *testing.T, b backend, _ tables) {
		bad := b.cfg
		bad.Password = "definitely-wrong-password"
		_, err := dbx.Open(context.Background(), bad)
		if err == nil || classOf(t, err) != dbx.Auth {
			t.Fatalf("a wrong password = %v, want Auth", err)
		}
		if strings.Contains(err.Error(), "definitely-wrong-password") {
			t.Fatalf("the password is in the error: %v", err)
		}
		// Nothing listening is a transient failure.
		closed, _ := net.Listen("tcp", "127.0.0.1:0")
		port := closed.Addr().(*net.TCPAddr).Port
		closed.Close()
		gone := b.cfg
		gone.Host, gone.Port = "127.0.0.1", port
		if _, err := dbx.Open(context.Background(), gone); err == nil || classOf(t, err) != dbx.Transient {
			t.Fatalf("a refused connection = %v, want Transient", err)
		}
		// verify-full against a server without TLS fails instead of falling back to clear text.
		secure := b.cfg
		secure.TLS = dbx.TLSVerifyFull
		if _, err := dbx.Open(context.Background(), secure); err == nil {
			t.Fatal("verify-full connected to a server that has no TLS")
		}
		secure.TLS = "sometimes"
		if _, err := dbx.Open(context.Background(), secure); err == nil {
			t.Fatal("an unknown tls mode was accepted")
		}
		// The dial hook is what makes the egress policy apply.
		var dialed atomic.Int32
		hooked := b.cfg
		hooked.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			dialed.Add(1)
			return (&net.Dialer{}).DialContext(ctx, network, address)
		}
		open(t, hooked)
		if dialed.Load() == 0 {
			t.Fatal("the connection did not go through the supplied dial function")
		}
		refuse := b.cfg
		refuse.Dial = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("egress blocked") }
		if _, err := dbx.Open(context.Background(), refuse); err == nil || !strings.Contains(err.Error(), "egress blocked") {
			t.Fatalf("a refused dial = %v", err)
		}
	})
}
