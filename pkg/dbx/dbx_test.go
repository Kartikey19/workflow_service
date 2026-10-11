package dbx

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckReadOnly(t *testing.T) {
	for _, ok := range []string{
		"select 1", "  SELECT * from t", "with x as (select 1) select * from x", "show tables", "describe t", "desc t", "explain select 1",
		"values (1), (2)", "table t", "(select 1) union (select 2)", "-- note\nselect 1", "/* hint */ select 1", "\n\t(\n select 1)",
	} {
		if err := checkReadOnly(ok); err != nil {
			t.Errorf("%q was refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"insert into t values (1)", "update t set a = 1", "delete from t", "create table t (a int)", "drop table t", "alter table t add b int",
		"truncate t", "call p()", "grant all on *.* to x", "copy t to program 'x'", "set role admin", "begin", "commit", "lock tables t write",
		"select 1 into outfile '/tmp/x'", "select * from t INTO   DUMPFILE '/tmp/x'", "", "   ", "-- only a comment", "/* unterminated select 1",
		"1; drop table t", "selectx 1", "withdraw 1",
	} {
		err := checkReadOnly(bad)
		var dbErr *Error
		if err == nil || !errors.As(err, &dbErr) || dbErr.Class != Statement {
			t.Errorf("%q was allowed (err = %v)", bad, err)
		}
	}
}

func TestSplitTableAndColumns(t *testing.T) {
	for name, want := range map[string][2]string{"events": {"", "events"}, "analytics.events": {"analytics", "events"}, "_t1": {"", "_t1"}} {
		schema, table, err := splitTable(name)
		if err != nil || schema != want[0] || table != want[1] {
			t.Errorf("splitTable(%q) = %q, %q, %v", name, schema, table, err)
		}
	}
	for _, bad := range []string{"", "a.b.c", "a b", "a;b", `"a"`, "1abc", "a-b", strings.Repeat("x", 64), ".a", "a."} {
		if _, _, err := splitTable(bad); err == nil {
			t.Errorf("splitTable(%q) accepted", bad)
		}
	}
	names, err := columnNames([]map[string]any{{"b": 1, "a": 2}, {"a": 3, "b": 4}})
	if err != nil || strings.Join(names, ",") != "a,b" {
		t.Errorf("columnNames = %v, %v", names, err)
	}
	for name, rows := range map[string][]map[string]any{
		"empty":    nil,
		"no keys":  {{}},
		"ragged":   {{"a": 1}, {"a": 1, "b": 2}},
		"mismatch": {{"a": 1}, {"b": 1}},
		"bad name": {{"a b": 1}},
		"too many": make([]map[string]any, MaxInsertRows+1),
	} {
		if _, err := columnNames(rows); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNormalizeParam(t *testing.T) {
	if got := normalizeParam(float64(3)); got != int64(3) {
		t.Errorf("3.0 -> %#v", got)
	}
	if got := normalizeParam(2.5); got != 2.5 {
		t.Errorf("2.5 -> %#v", got)
	}
	if got := normalizeParam(float64(1e20)); got != float64(1e20) {
		t.Errorf("1e20 -> %#v, want it left a float", got)
	}
	if got := normalizeParam("x"); got != "x" {
		t.Errorf("string -> %#v", got)
	}
}

func TestQueryOptionsNormalized(t *testing.T) {
	o := QueryOptions{}.normalized()
	if o.MaxRows != DefaultMaxRows || o.Timeout != DefaultTimeout {
		t.Errorf("defaults = %+v", o)
	}
	o = QueryOptions{MaxRows: 1 << 30, Timeout: 24 * 60 * 60 * 1e9}.normalized()
	if o.MaxRows != MaxRowsLimit || o.Timeout != MaxTimeout {
		t.Errorf("caps = %+v", o)
	}
}

func TestRedactPassword(t *testing.T) {
	err := redactPassword(errors.New("auth failed for password hunter22-secret"), "hunter22-secret")
	if strings.Contains(err.Error(), "hunter22") {
		t.Errorf("password leaked: %v", err)
	}
	same := errors.New("nothing to hide")
	if redactPassword(same, "hunter22-secret") != same {
		t.Error("an error without the password must be returned as is")
	}
}
