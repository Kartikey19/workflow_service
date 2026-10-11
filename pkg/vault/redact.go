package vault

import (
	"encoding/json"
	"regexp"
	"strings"
)

var jsonStrings = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

// RedactJSON masks a credential inside JSON strings (including object keys).
// It preserves numeric values and valid JSON, even when the credential contains
// quotes, backslashes or digits. The input is expected to be valid JSON.
func RedactJSON(data json.RawMessage, secret string) json.RawMessage {
	if secret == "" {
		return data
	}
	return jsonStrings.ReplaceAllFunc(data, func(token []byte) []byte {
		var value string
		if json.Unmarshal(token, &value) != nil || !strings.Contains(value, secret) {
			return token
		}
		encoded, _ := json.Marshal(strings.ReplaceAll(value, secret, "[redacted]"))
		return encoded
	})
}
