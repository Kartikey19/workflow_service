package jsonschema

import (
	"encoding/json"
	"testing"
)

func TestValidate(t *testing.T) {
	schema := map[string]any{}
	if err := json.Unmarshal([]byte(`{
		"type": "object", "required": ["rider", "eta"], "additionalProperties": false,
		"properties": {
			"rider": {"type": "string", "minLength": 2, "pattern": "^R-"},
			"eta": {"type": "integer", "minimum": 0, "maximum": 120},
			"tags": {"type": "array", "maxItems": 2, "items": {"type": "string", "enum": ["fast", "cold"]}}
		}}`), &schema); err != nil {
		t.Fatal(err)
	}
	if err := Check(schema); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		payload string
		want    []string
	}{
		{`{"rider":"R-42","eta":5}`, nil},
		{`{"rider":"R-42","eta":5,"tags":["fast"]}`, nil},
		{`{}`, []string{"$.eta", "$.rider"}},
		{`{"rider":"X","eta":1.5}`, []string{"$.eta", "$.rider", "$.rider"}},
		{`{"rider":"R-1","eta":500,"extra":true}`, []string{"$.eta", "$.extra"}},
		{`{"rider":"R-1","eta":1,"tags":["slow","fast","cold"]}`, []string{"$.tags", "$.tags[0]"}},
		{`[1]`, []string{"$"}},
		{`not json`, []string{"$"}},
	}
	for _, c := range cases {
		got := ValidateJSON(schema, []byte(c.payload))
		if len(got) != len(c.want) {
			t.Errorf("%s: got %+v, want fields %v", c.payload, got, c.want)
			continue
		}
		for i, field := range c.want {
			if got[i].Field != field {
				t.Errorf("%s: got %+v, want fields %v", c.payload, got, c.want)
				break
			}
		}
	}
}

func TestCheckRejectsUnsupportedSchemas(t *testing.T) {
	for _, raw := range []string{
		`[]`, `{"type":"decimal"}`, `{"oneOf":[]}`, `{"properties":{"a":{"type":1}}}`,
		`{"pattern":"("}`, `{"required":"a"}`, `{"additionalProperties":{}}`, `{"minimum":"1"}`,
	} {
		var schema any
		if err := json.Unmarshal([]byte(raw), &schema); err != nil {
			t.Fatal(err)
		}
		if Check(schema) == nil {
			t.Errorf("Check(%s) = nil, want error", raw)
		}
	}
}
