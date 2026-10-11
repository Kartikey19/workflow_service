package expr

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/cel-go/common/types"
)

func TestEnvCompileAndEval(t *testing.T) {
	environment, err := NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	bindings := Bindings{
		Input:   map[string]any{"count": int64(4), "name": "Alice"},
		Nodes:   map[string]NodeView{"fetch": {Output: map[string]any{"id": "order-1"}, Status: "succeeded", Attempt: 2, Metrics: map[string]any{"tokens": int64(10)}}},
		Run:     RunView{ID: "run-1", CorrelationID: "order-1", StartedAt: now.Add(-time.Minute), Attempt: 1},
		Secrets: map[string]string{"TOKEN": "private-token"}, Now: now,
	}
	tests := []struct {
		source string
		want   any
	}{
		{"input.count + 1", int64(5)},
		{"input.name", "Alice"},
		{"input.count > 3", true},
		{"1.5 + 2.0", 3.5},
		{"nodes.fetch.output.id", "order-1"},
		{"nodes.fetch.status", "succeeded"},
		{"nodes.fetch.attempt", int64(2)},
		{"nodes.fetch.metrics.tokens", int64(10)},
		{"run.id", "run-1"},
		{"run.correlation_id", "order-1"},
		{"run.attempt", int64(1)},
		{"now", now},
		{"now - run.started_at", time.Minute},
		{"secrets.TOKEN", "private-token"},
		{"[input.count, 2].size()", int64(2)},
		{`{"name": input.name}.name`, "Alice"},
		{"has(input.missing)", false},
	}
	for _, tt := range tests {
		t.Run(tt.source, func(t *testing.T) {
			program, err := environment.Compile(tt.source)
			if err != nil {
				t.Fatal(err)
			}
			value, err := program.Eval(context.Background(), bindings)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(value.Value(), tt.want) {
				t.Fatalf("got %#v; want %#v", value.Value(), tt.want)
			}
		})
	}
}

func TestCompileRejectsInvalidExpressions(t *testing.T) {
	environment, err := NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"", "input.count +", `${input.count}`, "unknown.value", `1 + "x"`, `now + 1`} {
		t.Run(source, func(t *testing.T) {
			if _, err := environment.Compile(source); err == nil {
				t.Fatal("expected compilation error")
			}
		})
	}
}

func TestProgramReuseAndCancellation(t *testing.T) {
	environment, err := NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	program, err := environment.Compile("input.count + 1")
	if err != nil {
		t.Fatal(err)
	}
	for _, count := range []int64{2, 9} {
		result, err := program.Eval(context.Background(), Bindings{Input: map[string]any{"count": count}})
		if err != nil || result != types.Int(count+1) {
			t.Fatalf("result = %v, %v", result, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := program.Eval(ctx, Bindings{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestEvalPreservesCollectionsAndNull(t *testing.T) {
	environment, err := NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		source string
		kind   string
	}{
		{"null", "null_type"}, {"[1,2]", "list"}, {`{"a": 1}`, "map"},
	} {
		program, err := environment.Compile(tt.source)
		if err != nil {
			t.Fatal(err)
		}
		result, err := program.Eval(context.Background(), Bindings{})
		if err != nil {
			t.Fatal(err)
		}
		if result.Type().TypeName() != tt.kind {
			t.Fatalf("type = %s, want %s", result.Type().TypeName(), tt.kind)
		}
	}
}

func TestEvalErrorsDoNotExposeBindings(t *testing.T) {
	environment, err := NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"input.missing", "input.count + 1", "int(secrets.TOKEN)"} {
		program, err := environment.Compile(source)
		if err != nil {
			t.Fatal(err)
		}
		result, err := program.Eval(context.Background(), Bindings{Input: map[string]any{"count": "wrong type"}, Secrets: map[string]string{"TOKEN": "private-token"}})
		if err == nil || result != nil {
			t.Fatalf("expected runtime error for %s", source)
		}
		if strings.Contains(err.Error(), "private-token") {
			t.Fatal("runtime error leaked secret")
		}
	}
}

func TestEvalEnforcesCostLimit(t *testing.T) {
	environment, err := NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	program, err := environment.Compile("input.items.map(x, x + 1)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := program.Eval(context.Background(), Bindings{Input: map[string]any{"items": make([]int64, 100_000)}}); err == nil {
		t.Fatal("expected cost limit error")
	}
}
