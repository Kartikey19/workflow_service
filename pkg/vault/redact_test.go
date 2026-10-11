package vault

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactJSONKeepsNumbersAndEscapedStrings(t *testing.T) {
	raw := json.RawMessage(`{"number":123456,"nested":["key:123456"],"123456":"value"}`)
	clean := RedactJSON(raw, "123456")
	if !json.Valid(clean) || !strings.Contains(string(clean), `"number":123456`) || strings.Contains(string(clean), `"123456"`) || strings.Contains(string(clean), "key:123456") {
		t.Fatalf("redaction: %s", clean)
	}
	secret := `quote"\secret`
	encoded, _ := json.Marshal(map[string]any{"value": "before " + secret + " after"})
	clean = RedactJSON(encoded, secret)
	var decoded map[string]string
	if err := json.Unmarshal(clean, &decoded); err != nil || decoded["value"] != "before [redacted] after" {
		t.Fatalf("escaped redaction: %s %v", clean, err)
	}
}
