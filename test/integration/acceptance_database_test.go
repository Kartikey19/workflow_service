//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kartikey/workflow_service/internal/config"
)

// dbConnection is the API body for a connection to the backend under test.
func dbConnection(name string, b backend) map[string]any {
	return map[string]any{
		"name": name, "kind": b.cfg.Kind,
		"config": map[string]any{"host": b.cfg.Host, "port": b.cfg.Port, "database": b.cfg.Database, "username": b.cfg.Username, "tls": "disable"},
		"secret": map[string]any{"password": b.cfg.Password},
	}
}

func dbWorkflow(name, nodeType, nodeConfig string) string {
	return "apiVersion: workflow/v1\nname: " + name + "\ninputs:\n  id: {type: number}\n  since: {type: string}\nnodes:\n  - id: act\n    type: " + nodeType +
		"\n    retry: {max_attempts: 3, backoff: fixed, initial: 10ms}\n    config: " + nodeConfig + "\n"
}

func eachDatabase(t *testing.T, run func(t *testing.T, s *stack, owner *person, b backend, tb tables, conn string)) {
	backends := []backend{postgresBackend(t)}
	if my, ok := mysqlBackend(t); ok {
		backends = append(backends, my)
	}
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			s := newStack(t, ctx)
			owner := s.owner(t, "DB Co "+b.name)
			tb := b.setup(t)
			conn := b.name + "-warehouse"
			created := owner.mustOK("POST", "/be/v1/connections", dbConnection(conn, b), 201)
			if _, ok := created["secret"]; ok || strings.Contains(toJSON(created), `"password"`) {
				t.Fatalf("the connection view exposes the secret: %v", created)
			}
			run(t, s, owner, b, tb, conn)
		})
	}
}

// M14: db.query returns rows through a saved connection, with values bound as
// parameters, a row cap, and read-only enforcement; the password is stored
// nowhere.
func TestDatabaseQueryNode(t *testing.T) {
	eachDatabase(t, func(t *testing.T, s *stack, owner *person, b backend, tb tables, conn string) {
		p1, p2 := placeholder(b, 1), placeholder(b, 2)
		publish := func(name, nodeType, nodeConfig string) {
			if status, body := owner.publish(dbWorkflow(name, nodeType, nodeConfig)); status != 201 {
				t.Fatalf("publish %s = %d %v", name, status, body)
			}
		}
		publish("q-params", "db.query", fmt.Sprintf(`{connection: %s, sql: "select id, name, score from %s where id = %s and name = %s", params: ['${input.id}', first]}`, conn, tb.events, p1, p2))
		_, started := owner.call("POST", "/be/v1/runs", map[string]any{"workflow": "q-params", "input": map[string]any{"id": 1}})
		run := owner.waitRun(started["id"].(string))
		if run["status"] != "succeeded" {
			t.Fatalf("run = %v", run)
		}
		var output map[string]any
		if err := s.h.pool.QueryRow(context.Background(), `select output from node_executions where run_id = $1 and node_id = 'act'`, mustParse(t, started["id"].(string))).Scan(&output); err != nil {
			t.Fatal(err)
		}
		rows := output["rows"].([]any)
		if len(rows) != 1 || output["row_count"] != float64(1) || output["truncated"] != false || rows[0].(map[string]any)["name"] != "first" || rows[0].(map[string]any)["score"] != "12345678.9012" {
			t.Fatalf("output = %v", output)
		}
		// (Skipped when the test password coincides with a plaintext config value.)
		if n := s.h.occurrences(b.cfg.Password); n != 0 && !strings.Contains(b.cfg.Database+b.cfg.Username+b.cfg.Host, b.cfg.Password) {
			t.Fatalf("the database password is stored in %d row(s)", n)
		}

		// The row cap truncates, and fail_on_truncate turns that into a failure.
		publish("q-cap", "db.query", fmt.Sprintf(`{connection: %s, sql: "%s", max_rows: 25}`, conn, b.series(500)))
		_, started = owner.call("POST", "/be/v1/runs", map[string]any{"workflow": "q-cap"})
		owner.waitRun(started["id"].(string))
		_ = s.h.pool.QueryRow(context.Background(), `select output from node_executions where run_id = $1 and node_id = 'act'`, mustParse(t, started["id"].(string))).Scan(&output)
		if output["row_count"] != float64(25) || output["truncated"] != true {
			t.Fatalf("capped output = %v", output["truncated"])
		}
		publish("q-strict", "db.query", fmt.Sprintf(`{connection: %s, sql: "%s", max_rows: 25, fail_on_truncate: true}`, conn, b.series(500)))
		id, run := runToEnd(t, owner, "q-strict")
		if attempts := s.h.attempts(mustParse(t, id), "act"); run["status"] != "failed" || len(attempts) != 1 || attempts[0]["code"] != "too_many_rows" {
			t.Fatalf("fail_on_truncate: %v %v", run["status"], attempts)
		}

		// A write in a read node is refused once, permanently, and changes nothing.
		for name, statement := range map[string]string{
			"q-write": "delete from " + tb.events,
			"q-ddl":   "drop table " + tb.events,
		} {
			publish(name, "db.query", fmt.Sprintf(`{connection: %s, sql: "%s"}`, conn, statement))
			id, run := runToEnd(t, owner, name)
			attempts := s.h.attempts(mustParse(t, id), "act")
			if run["status"] != "failed" || len(attempts) != 1 || attempts[0]["code"] != "db_statement" {
				t.Errorf("%s: %v %v, want one db_statement failure", name, run["status"], attempts)
			}
		}
		publish("q-still-there", "db.query", fmt.Sprintf(`{connection: %s, sql: "select count(*) as c from %s"}`, conn, tb.events))
		if _, run := runToEnd(t, owner, "q-still-there"); run["status"] != "succeeded" {
			t.Fatalf("the table was damaged by a read node: %v", run)
		}
	})
}

// A slow statement is cut off by the timeout and retried as transient.
func TestDatabaseQueryTimeout(t *testing.T) {
	eachDatabase(t, func(t *testing.T, s *stack, owner *person, b backend, tb tables, conn string) {
		node := fmt.Sprintf(`{connection: %s, sql: "%s", timeout: 300ms}`, conn, b.slow)
		if status, body := owner.publish(dbWorkflow("q-slow", "db.query", node)); status != 201 {
			t.Fatalf("publish = %d %v", status, body)
		}
		id, run := runToEnd(t, owner, "q-slow")
		attempts := s.h.attempts(mustParse(t, id), "act")
		if run["status"] != "failed" || len(attempts) != 3 || attempts[0]["code"] != "db_transient" {
			t.Fatalf("a timeout: %v %v, want three transient attempts", run["status"], attempts)
		}
	})
}

// Values never become SQL text: expressions in `sql` are refused at publish time,
// and injection through a parameter is just a value.
func TestDatabaseQueryRefusesSQLInterpolation(t *testing.T) {
	eachDatabase(t, func(t *testing.T, s *stack, owner *person, b backend, tb tables, conn string) {
		bad := fmt.Sprintf(`{connection: %s, sql: "select * from %s where name = '${input.since}'"}`, conn, tb.events)
		if status, body := owner.publish(dbWorkflow("q-inject", "db.query", bad)); status != 400 || !strings.Contains(toJSON(body), "must not contain ${...}") {
			t.Fatalf("publish with ${} in sql = %d %v, want a 400 explaining params", status, body)
		}
		node := fmt.Sprintf(`{connection: %s, sql: "select count(*) as c from %s where name = %s", params: ['${input.since}']}`, conn, tb.events, placeholder(b, 1))
		if status, body := owner.publish(dbWorkflow("q-param", "db.query", node)); status != 201 {
			t.Fatalf("publish = %d %v", status, body)
		}
		_, started := owner.call("POST", "/be/v1/runs", map[string]any{"workflow": "q-param", "input": map[string]any{"since": "x'; drop table " + tb.events + "; --"}})
		if run := owner.waitRun(started["id"].(string)); run["status"] != "succeeded" {
			t.Fatalf("run = %v", run)
		}
		// The hostile value was just compared as a string; the table is intact.
		if status, _ := owner.publish(dbWorkflow("q-check", "db.query", fmt.Sprintf(`{connection: %s, sql: "select count(*) as c from %s"}`, conn, tb.events))); status != 201 {
			t.Fatal("publish check")
		}
		if _, run := runToEnd(t, owner, "q-check"); run["status"] != "succeeded" {
			t.Fatalf("the table was dropped through a parameter: %v", run)
		}
	})
}

// db.insert: one row, many rows, conflicts, failure classification, and names
// that are validated rather than trusted.
func TestDatabaseInsertNode(t *testing.T) {
	eachDatabase(t, func(t *testing.T, s *stack, owner *person, b backend, tb tables, conn string) {
		publish := func(name, nodeConfig string) {
			if status, body := owner.publish(dbWorkflow(name, "db.insert", nodeConfig)); status != 201 {
				t.Fatalf("publish %s = %d %v", name, status, body)
			}
		}
		count := func() string {
			publishCount := dbWorkflow("count-"+randomHex(t, 3), "db.query", fmt.Sprintf(`{connection: %s, sql: "select count(*) as c from %s"}`, conn, tb.events))
			name := strings.Fields(strings.Split(publishCount, "\n")[1])[1]
			if status, body := owner.publish(publishCount); status != 201 {
				t.Fatalf("publish count = %d %v", status, body)
			}
			id, _ := runToEnd(t, owner, name)
			var output map[string]any
			if err := s.h.pool.QueryRow(context.Background(), `select output from node_executions where run_id = $1 and node_id = 'act'`, mustParse(t, id)).Scan(&output); err != nil {
				t.Fatal(err)
			}
			return fmt.Sprint(output["rows"].([]any)[0].(map[string]any)["c"])
		}

		publish("ins-row", fmt.Sprintf(`{connection: %s, table: %s, row: {id: '${input.id}', name: "ünï ${input.since}", score: 4.5, active: true}}`, conn, tb.events))
		_, started := owner.call("POST", "/be/v1/runs", map[string]any{"workflow": "ins-row", "input": map[string]any{"id": 7, "since": "x"}})
		if run := owner.waitRun(started["id"].(string)); run["status"] != "succeeded" {
			t.Fatalf("run = %v", run)
		}
		var output map[string]any
		_ = s.h.pool.QueryRow(context.Background(), `select output from node_executions where run_id = $1 and node_id = 'act'`, mustParse(t, started["id"].(string))).Scan(&output)
		if output["inserted"] != float64(1) || count() != "2" {
			t.Fatalf("inserted = %v, rows = %s", output["inserted"], count())
		}

		publish("ins-rows", fmt.Sprintf(`{connection: %s, table: %s, rows: [{id: 20, name: a}, {id: 21, name: b}, {id: 22, name: c}]}`, conn, tb.events))
		if _, run := runToEnd(t, owner, "ins-rows"); run["status"] != "succeeded" || count() != "5" {
			t.Fatalf("rows insert: %v, count %s", run["status"], count())
		}

		// A duplicate key fails permanently unless conflicts are ignored.
		dup := `rows: [{id: 21, name: dup}, {id: 30, name: fresh}]`
		publish("ins-dup", fmt.Sprintf(`{connection: %s, table: %s, %s}`, conn, tb.events, dup))
		id, run := runToEnd(t, owner, "ins-dup")
		attempts := s.h.attempts(mustParse(t, id), "act")
		if run["status"] != "failed" || len(attempts) != 1 || attempts[0]["code"] != "db_statement" || count() != "5" {
			t.Fatalf("duplicate: %v %v rows %s; want one db_statement failure and nothing inserted", run["status"], attempts, count())
		}
		publish("ins-ignore", fmt.Sprintf(`{connection: %s, table: %s, %s, ignore_conflicts: true}`, conn, tb.events, dup))
		if _, run := runToEnd(t, owner, "ins-ignore"); run["status"] != "succeeded" || count() != "6" {
			t.Fatalf("ignore_conflicts: %v, count %s, want only the fresh row added", run["status"], count())
		}

		// Names are validated, not trusted.
		for name, node := range map[string]string{
			"ins-bad-table":  fmt.Sprintf(`{connection: %s, table: "%s; drop table %s", row: {id: 1}}`, conn, tb.events, tb.events),
			"ins-bad-column": fmt.Sprintf(`{connection: %s, table: %s, row: {"id; drop table x": 1}}`, conn, tb.events),
		} {
			publish(name, node)
			id, run := runToEnd(t, owner, name)
			attempts := s.h.attempts(mustParse(t, id), "act")
			if run["status"] != "failed" || len(attempts) != 1 || attempts[0]["code"] != "db_statement" {
				t.Errorf("%s: %v %v", name, run["status"], attempts)
			}
		}
		if count() != "6" {
			t.Fatalf("rows after the rejected inserts = %s", count())
		}
	})
}

// Connection tests and run-time failures: wrong credentials are rejected before
// saving and are permanent at run time; the password never appears.
func TestDatabaseConnectionFailures(t *testing.T) {
	eachDatabase(t, func(t *testing.T, s *stack, owner *person, b backend, tb tables, conn string) {
		bad := dbConnection("wrong-"+b.name, b)
		bad["secret"] = map[string]any{"password": "wrong-database-password"}
		status, body := owner.call("POST", "/be/v1/connections", bad)
		if status != 422 || body["code"] != "connection_test_failed" || strings.Contains(toJSON(body), "wrong-database-password") {
			t.Fatalf("a wrong password = %d %v", status, body)
		}
		bad["skip_test"] = true
		owner.mustOK("POST", "/be/v1/connections", bad, 201)
		if status, _ := owner.publish(dbWorkflow("bad-pass", "db.query", fmt.Sprintf(`{connection: wrong-%s, sql: "select 1"}`, b.name))); status != 201 {
			t.Fatal("publish")
		}
		id, run := runToEnd(t, owner, "bad-pass")
		attempts := s.h.attempts(mustParse(t, id), "act")
		if run["status"] != "failed" || len(attempts) != 1 || attempts[0]["code"] != "db_auth" {
			t.Fatalf("run-time wrong password: %v %v, want one db_auth failure", run["status"], attempts)
		}
		if n := s.h.occurrences("wrong-database-password"); n != 0 {
			t.Fatalf("the rejected password is stored in %d row(s)", n)
		}
		// An unknown connection and a connection of another kind.
		owner.mustOK("POST", "/be/v1/connections", map[string]any{"name": "mail", "kind": "smtp", "skip_test": true,
			"config": map[string]any{"host": "smtp.example.com"}}, 201)
		for name, connection := range map[string]string{"no-conn": "ghost", "wrong-kind": "mail"} {
			if status, _ := owner.publish(dbWorkflow(name, "db.query", fmt.Sprintf(`{connection: %s, sql: "select 1"}`, connection))); status != 201 {
				t.Fatal("publish")
			}
			id, run := runToEnd(t, owner, name)
			attempts := s.h.attempts(mustParse(t, id), "act")
			if run["status"] != "failed" || len(attempts) != 1 || attempts[0]["code"] != "E_CONNECTION_NOT_FOUND" {
				t.Errorf("%s: %v %v", name, run["status"], attempts)
			}
		}
	})
}

// The egress policy applies to database connectors: with the default policy a
// loopback or private address is refused, both when testing a connection and
// when a node runs.
func TestDatabaseConnectorsRespectEgressRules(t *testing.T) {
	ctx := context.Background()
	s := newStack(t, ctx, func(c *config.Config) { c.Egress.AllowPrivate = false })
	owner := s.owner(t, "Egress Co")
	b := postgresBackend(t)
	status, body := owner.call("POST", "/be/v1/connections", dbConnection("blocked", b))
	if status != 422 || !strings.Contains(body["detail"].(string), "egress policy") {
		t.Fatalf("a loopback database under the default policy = %d %v, want the egress policy to refuse it", status, body)
	}
	withSkip := dbConnection("blocked", b)
	withSkip["skip_test"] = true
	owner.mustOK("POST", "/be/v1/connections", withSkip, 201)
	if status, _ := owner.publish(dbWorkflow("egress", "db.query", `{connection: blocked, sql: "select 1"}`)); status != 201 {
		t.Fatal("publish")
	}
	id, run := runToEnd(t, owner, "egress")
	attempts := s.h.attempts(mustParse(t, id), "act")
	if run["status"] != "failed" || len(attempts) != 1 || attempts[0]["code"] != "egress_blocked" {
		t.Fatalf("run: %v %v, want one egress_blocked failure", run["status"], attempts)
	}
}

// A foreach over db.query reads per element, each with its own parameters.
func TestDatabaseQueryInsideForeach(t *testing.T) {
	eachDatabase(t, func(t *testing.T, s *stack, owner *person, b backend, tb tables, conn string) {
		if _, err := mustInsertRows(t, owner, conn, tb.events); err != nil {
			t.Fatal(err)
		}
		source := fmt.Sprintf(`apiVersion: workflow/v1
name: per-id
nodes:
  - id: look
    type: db.query
    foreach: {items: '${[1, 2, 3]}'}
    config:
      connection: %s
      sql: "select name from %s where id = %s"
      params: ['${item}']
outputs: {names: '${nodes.look.output.map(o, o.rows[0].name)}'}
`, conn, tb.events, placeholder(b, 1))
		if status, body := owner.publish(source); status != 201 {
			t.Fatalf("publish = %d %v", status, body)
		}
		id, run := runToEnd(t, owner, "per-id")
		if run["status"] != "succeeded" {
			t.Fatalf("run = %v", run)
		}
		out, _ := s.h.runOutput(mustParse(t, id))
		names := out["names"].([]any)
		if len(names) != 3 || names[0] != "first" || names[1] != "two" || names[2] != "three" {
			t.Fatalf("names = %v", names)
		}
	})
}

func mustInsertRows(t *testing.T, owner *person, conn, table string) (string, error) {
	t.Helper()
	source := fmt.Sprintf("apiVersion: workflow/v1\nname: seed\nnodes:\n  - id: act\n    type: db.insert\n    config: {connection: %s, table: %s, rows: [{id: 2, name: two}, {id: 3, name: three}]}\n", conn, table)
	if status, body := owner.publish(source); status != 201 {
		return "", fmt.Errorf("publish seed = %d %v", status, body)
	}
	_, run := runToEnd(t, owner, "seed")
	if run["status"] != "succeeded" {
		return "", fmt.Errorf("seed run = %v", run)
	}
	return "", nil
}
