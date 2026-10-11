package expr

import (
	"context"
	"testing"
)

func TestLibraryFunctions(t *testing.T) {
	env, err := NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	bindings := Bindings{Input: map[string]any{
		"items": []any{map[string]any{"price": 2.5}, map[string]any{"price": 1.5}},
		"ids":   []any{int64(1), int64(2), int64(3)},
		"name":  "Ada Lovelace",
	}}
	cases := map[string]any{
		`input.items.map(i, i.price).sum()`: 4.0,
		`input.ids.sum()`:                   int64(6),
		`input.name.lowerAscii()`:           "ada lovelace",
		`input.name.split(" ")[0]`:          "Ada",
		`base64.encode(b"hi")`:              "aGk=",
		`math.greatest(1, 5, 3)`:            int64(5),
	}
	for source, want := range cases {
		program, err := env.Compile(source)
		if err != nil {
			t.Fatalf("%s: compile: %v", source, err)
		}
		got, err := program.Eval(context.Background(), bindings)
		if err != nil {
			t.Fatalf("%s: eval: %v", source, err)
		}
		if got.Value() != want {
			t.Fatalf("%s = %v (%T), want %v", source, got.Value(), got.Value(), want)
		}
	}
}
