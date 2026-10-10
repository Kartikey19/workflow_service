package expr

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	env, err := NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	bindings := Bindings{Input: map[string]any{"id": "123", "count": int64(4), "template": "${input.id}", "object": map[string]any{"id": "123"}}}
	tests := []struct {
		name         string
		source, want any
	}{
		{"empty", "", ""}, {"literal", "hello", "hello"},
		{"escape", "$${input.id}", "${input.id}"},
		{"escaped plus expression", "$${literal}-${input.id}", "${literal}-123"},
		{"unicode", "नमस्ते ${input.id}", "नमस्ते 123"},
		{"whole string", "${input.id}", "123"},
		{"integer", "${input.count}", int64(4)},
		{"arithmetic", "${input.count + 1}", int64(5)},
		{"unsigned", "${1u}", uint64(1)},
		{"double", "${1.5}", 1.5},
		{"true", "${true}", true}, {"false", "${false}", false},
		{"null", "${null}", nil},
		{"list", "${[1,2]}", []any{int64(1), int64(2)}},
		{"empty list", "${[]}", []any{}},
		{"map", `${{"id": input.id}}`, map[string]any{"id": "123"}},
		{"empty map", "${{}}", map[string]any{}},
		{"binding map", "${input.object}", map[string]any{"id": "123"}},
		{"nested result", `${{"x": [null, {"a": 1}]}}`, map[string]any{"x": []any{nil, map[string]any{"a": int64(1)}}}},
		{"url", "https://example.com/${input.id}", "https://example.com/123"},
		{"surrounded", "a${input.id}b${input.count}c", "a123b4c"},
		{"adjacent", "${input.id}${input.count}", "1234"},
		{"outer space", " ${input.count} ", " 4 "},
		{"inner space", "${ input.count }", int64(4)},
		{"boolean text", "ok=${true}", "ok=true"},
		{"null text", "x=${null}", "x=null"},
		{"list text", "x=${[1,2]}", "x=[1,2]"},
		{"map text", `x=${{"b":2,"a":1}}`, `x={"a":1,"b":2}`},
		{"quoted brace", `${"}"}`, "}"},
		{"raw string", `${r"\}"}`, `\}`},
		{"multiline", "${'''a\n}b'''}", "a\n}b"},
		{"comment", "${1 // }\n+ 2}", int64(3)},
		{"no recursive evaluation", "${input.template}", "${input.id}"},
		{"plain number", 9, 9}, {"plain null", nil, nil},
		{"plain boolean", false, false},
		{"nested configuration", map[string]any{"url": "/${input.id}", "body": []any{"${input.count}", true}}, map[string]any{"url": "/123", "body": []any{int64(4), true}}},
		{"literal key", map[string]any{"${input.id}": "${input.count}"}, map[string]any{"${input.id}": int64(4)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(context.Background(), tt.source, env, bindings)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %#v; want %#v", got, tt.want)
			}
		})
	}
}

func TestResolveErrors(t *testing.T) {
	env, err := NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"${", "${}", "${input.id +}", "${unknown}", "${input.missing}", `${{1: "x"}}`} {
		if result, err := Resolve(context.Background(), source, env, Bindings{}); err == nil || result != nil {
			t.Fatalf("expected error for %q", source)
		}
	}
	_, err = Resolve(context.Background(), map[string]any{"body": []any{"${int(secrets.TOKEN)}"}}, env, Bindings{Secrets: map[string]string{"TOKEN": "private-token"}})
	if err == nil || strings.Contains(err.Error(), "private-token") || !strings.Contains(err.Error(), "element 0") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCompiledTemplateReuseAndIsolation(t *testing.T) {
	env, err := NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	source := map[string]any{"value": "${input.count}", "object": `${{"a": [1]}}`}
	template, err := env.CompileTemplate(source)
	if err != nil {
		t.Fatal(err)
	}
	source["value"] = "changed"
	first, err := template.Resolve(context.Background(), Bindings{Input: map[string]any{"count": int64(1)}})
	if err != nil {
		t.Fatal(err)
	}
	first.(map[string]any)["object"].(map[string]any)["a"].([]any)[0] = int64(99)
	second, err := template.Resolve(context.Background(), Bindings{Input: map[string]any{"count": int64(2)}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"value": int64(2), "object": map[string]any{"a": []any{int64(1)}}}
	if !reflect.DeepEqual(second, want) {
		t.Fatalf("got %#v", second)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := template.Resolve(ctx, Bindings{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}
