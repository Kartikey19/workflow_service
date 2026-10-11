package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/kartikey/workflow_service/internal/core/executor"
)

// redactionMark replaces a secret value wherever it would otherwise be stored.
const redactionMark = "***"

// missingSecretError names secrets a node refers to that the vault does not
// hold. Names are not sensitive; values never appear.
type missingSecretError struct{ names []string }

func (e *missingSecretError) Error() string {
	return fmt.Sprintf("secret %s is not defined; add it under /v1/secrets", strings.Join(e.names, ", "))
}

// secretRef finds secrets.NAME and secrets["NAME"] in a config.
var secretRef = regexp.MustCompile(`secrets(?:\.([A-Za-z_][A-Za-z0-9_]*)|\[\\?"([A-Za-z_][A-Za-z0-9_]*)\\?"\])`)

// missingSecrets lists the secret names config refers to that values lacks.
func missingSecrets(config map[string]any, values map[string]string) []string {
	raw, err := json.Marshal(config)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, match := range secretRef.FindAllStringSubmatch(string(raw), -1) {
		name := match[1]
		if name == "" {
			name = match[2]
		}
		if _, ok := values[name]; !ok {
			seen[name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// secretValueList returns the non-empty secret values, longest first, so a
// value that contains another is replaced whole.
func secretValueList(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			out = append(out, value)
		}
	}
	slices.SortFunc(out, func(a, b string) int { return len(b) - len(a) })
	return out
}

// scrub replaces every secret value in text, also in its JSON-escaped form (a
// value with quotes or backslashes appears escaped inside a JSON document).
func scrub(text string, values []string) string {
	for _, value := range values {
		text = strings.ReplaceAll(text, value, redactionMark)
		if encoded, err := json.Marshal(value); err == nil && len(encoded) > 2 {
			if inner := string(encoded[1 : len(encoded)-1]); inner != value {
				text = strings.ReplaceAll(text, inner, redactionMark)
			}
		}
	}
	return text
}

func scrubBytes(data []byte, values []string) []byte {
	if len(data) == 0 {
		return data
	}
	clean := scrub(string(data), values)
	if bytes.Equal([]byte(clean), data) {
		return data
	}
	return []byte(clean)
}

// scrubError keeps the error's class (retryable or permanent, its code and
// Retry-After) while removing secret values from its message.
func scrubError(err error, values []string) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	clean := scrub(message, values)
	if clean == message {
		return err
	}
	cause := errors.New(clean)
	var retryable *executor.RetryableError
	var permanent *executor.PermanentError
	switch {
	case errors.As(err, &retryable):
		return &executor.RetryableError{Code: retryable.Code, Err: stripCode(cause, retryable.Code), RetryAfter: retryable.RetryAfter}
	case errors.As(err, &permanent):
		return &executor.PermanentError{Code: permanent.Code, Err: stripCode(cause, permanent.Code)}
	default:
		return cause
	}
}

// stripCode drops the "code: " prefix the error types add, so the scrubbed copy
// does not repeat it.
func stripCode(err error, code string) error {
	if code == "" {
		return err
	}
	return errors.New(strings.TrimPrefix(err.Error(), code+": "))
}
