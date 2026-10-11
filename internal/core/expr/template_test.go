package expr

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseTemplate(t *testing.T) {
	lit := func(s string) Segment { return Segment{Text: s} }
	exp := func(s string) Segment { return Segment{Text: s, IsExpression: true} }
	tests := []struct {
		name, source string
		want         []Segment
	}{
		{"empty", "", []Segment{lit("")}},
		{"literal", "hello", []Segment{lit("hello")}},
		{"unicode", "नमस्ते 👋", []Segment{lit("नमस्ते 👋")}},
		{"dollar", "$", []Segment{lit("$")}},
		{"double dollar", "$$", []Segment{lit("$$")}},
		{"ordinary braces", "{a}", []Segment{lit("{a}")}},
		{"expression", "${input.id}", []Segment{exp("input.id")}},
		{"prefix", "id=${input.id}", []Segment{lit("id="), exp("input.id")}},
		{"suffix", "${x}!", []Segment{exp("x"), lit("!")}},
		{"surrounded", "a${x}b", []Segment{lit("a"), exp("x"), lit("b")}},
		{"adjacent", "${a}${b}", []Segment{exp("a"), exp("b")}},
		{"separated", "a${x}b${y}c", []Segment{lit("a"), exp("x"), lit("b"), exp("y"), lit("c")}},
		{"spaces preserved", "${ x }", []Segment{exp(" x ")}},
		{"outer spaces", " ${x} ", []Segment{lit(" "), exp("x"), lit(" ")}},
		{"escaped opener", "$${input.id}", []Segment{lit("${input.id}")}},
		{"escape in text", "a$${x}b", []Segment{lit("a${x}b")}},
		{"escape then expression", "$${x}${y}", []Segment{lit("${x}"), exp("y")}},
		{"three dollars", "$$${x}", []Segment{lit("$${x}")}},
		{"escaped unclosed opener", "$${", []Segment{lit("${")}},
		{"object", `${{"id": input.id}}`, []Segment{exp(`{"id": input.id}`)}},
		{"nested object", `${{"a": {"b": 1}}}`, []Segment{exp(`{"a": {"b": 1}}`)}},
		{"list", `${[1, {"a": 2}]}`, []Segment{exp(`[1, {"a": 2}]`)}},
		{"double quoted brace", `${"}"}`, []Segment{exp(`"}"`)}},
		{"single quoted brace", `${'}'}`, []Segment{exp(`'}'`)}},
		{"escaped quote", `${"a\"}b"}`, []Segment{exp(`"a\"}b"`)}},
		{"escaped backslash", `${"\\"}`, []Segment{exp(`"\\"`)}},
		{"triple double", "${\"\"\"a\n}b\"\"\"}", []Segment{exp("\"\"\"a\n}b\"\"\"")}},
		{"triple single", "${'''a'}b'''}", []Segment{exp("'''a'}b'''")}},
		{"raw", `${r"\"}`, []Segment{exp(`r"\"`)}},
		{"raw uppercase", `${R'}'}`, []Segment{exp(`R'}'`)}},
		{"raw triple", `${r"""\}a"""}`, []Segment{exp(`r"""\}a"""`)}},
		{"bytes", `${b"}"}`, []Segment{exp(`b"}"`)}},
		{"raw bytes", `${bR"\"}`, []Segment{exp(`bR"\"`)}},
		{"comment braces", "${1 // } ignored\n+ 2}", []Segment{exp("1 // } ignored\n+ 2")}},
		{"url in string", `${"https://example.com/}"}`, []Segment{exp(`"https://example.com/}"`)}},
		{"quoted opener", `${"${x}"}`, []Segment{exp(`"${x}"`)}},
		{"null", "${null}", []Segment{exp("null")}},
		{"invalid CEL deferred", "${x +}", []Segment{exp("x +")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseTemplate(tt.source)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %#v; want %#v", got, tt.want)
			}
		})
	}
}

func TestParseTemplateErrors(t *testing.T) {
	for _, source := range []string{"${", "${x", "${}", "${ \n }", `${{"a": 1}`, `${"missing}`, "${'missing}", "${'''missing}", "${1 // }", "prefix ${valid} ${"} {
		t.Run(source, func(t *testing.T) {
			got, err := ParseTemplate(source)
			if err == nil || got != nil {
				t.Fatalf("got %#v, %v; want nil and error", got, err)
			}
		})
	}
	_, err := ParseTemplate("secret-token ${unterminated")
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatal("expected error without source contents")
	}
}
