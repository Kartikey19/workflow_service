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
	"sync"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/internal/core/expr"
	"github.com/kartikey/workflow_service/pkg/vault"
)

// redactionMark replaces a secret value wherever it would otherwise be stored.
const redactionMark = "***"

// missingSecretError names secrets a node refers to that the vault does not
// hold. Names are not sensitive; values never appear.
type missingSecretError struct{ names []string }

func (e *missingSecretError) Error() string {
	return fmt.Sprintf("secret %s is not defined; add it under /v1/secrets", strings.Join(e.names, ", "))
}

// secretRef finds secrets.NAME, secrets.GROUP.KEY and their secrets["NAME"]
// forms in one expression; input.secrets.x and mysecrets.x are not references.
var secretRef = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.])secrets(?:\.([A-Za-z_][A-Za-z0-9_]*)(?:\.([A-Za-z_][A-Za-z0-9_]*))?|\[["']([A-Za-z_][A-Za-z0-9_]*)["']\](?:\[["']([A-Za-z_][A-Za-z0-9_]*)["']\])?)`)

// hasSecret says whether a reference is satisfied. A plain secret satisfies
// secrets.NAME and anything read from it; a group satisfies secrets.GROUP and
// each of its keys by name.
func hasSecret(values map[string]string, name, key string) bool {
	if _, ok := values[name]; ok {
		return true
	}
	if key != "" {
		_, ok := values[name+"."+key]
		return ok
	}
	for stored := range values {
		if strings.HasPrefix(stored, name+".") {
			return true
		}
	}
	return false
}

// secretUse is one static reference: a secret's name, plus key for a group's key.
type secretUse struct{ name, key string }

// secretUses walks config's ${...} expressions. uses says whether any of them
// mentions secrets at all (secrets[input.name] counts); refs lists the names
// read statically. Literal text is ignored, so prose or code that mentions
// secrets.x (Python's secrets module, say) neither loads the vault nor is
// reported missing.
func secretUses(config map[string]any) (uses bool, refs []secretUse) {
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		case string:
			segments, err := expr.ParseTemplate(v)
			if err != nil {
				return
			}
			for _, segment := range segments {
				if !segment.IsExpression || !strings.Contains(segment.Text, "secrets") {
					continue
				}
				uses = true
				for _, match := range secretRef.FindAllStringSubmatch(segment.Text, -1) {
					if match[1] != "" {
						refs = append(refs, secretUse{match[1], match[2]})
					} else {
						refs = append(refs, secretUse{match[3], match[4]})
					}
				}
			}
		}
	}
	walk(config)
	return uses, refs
}

// missingSecrets lists the references values cannot satisfy, as NAME or
// group.key.
func missingSecrets(refs []secretUse, values map[string]string) []string {
	seen := map[string]bool{}
	for _, ref := range refs {
		if !hasSecret(values, ref.name, ref.key) {
			name := ref.name
			if ref.key != "" {
				name += "." + ref.key
			}
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

// maskSecrets is what stored node input sees in place of the vault: every
// credential as ***, while short settings keep their value so an expression
// such as double(secrets.group.temperature) still evaluates.
func maskSecrets(values map[string]string) map[string]string {
	masked := make(map[string]string, len(values))
	for name, value := range values {
		masked[name] = value
		if len(value) >= vault.MinScrubLength {
			masked[name] = redactionMark
		}
	}
	return masked
}

// secretValueList returns the secret values worth masking, longest first, so a
// value that contains another is replaced whole. Short values (a group's
// "high" or "0.3") are settings and would mangle the text around them.
func secretValueList(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if len(value) >= vault.MinScrubLength {
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

// redactions collects the credential values to scrub from one node attempt: the
// vault secrets its config used plus anything the executor registered.
type redactions struct {
	mu     sync.Mutex
	values map[string]struct{}
}

func newRedactions(initial []string) *redactions {
	r := &redactions{values: make(map[string]struct{}, len(initial))}
	r.Add(initial...)
	return r
}

// Add registers values to scrub; it is safe to call from the executor's goroutines.
func (r *redactions) Add(values ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, value := range values {
		if value != "" {
			r.values[value] = struct{}{}
		}
	}
}

// Values returns the registered values, longest first.
func (r *redactions) Values() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.values))
	for value := range r.values {
		out = append(out, value)
	}
	slices.SortFunc(out, func(a, b string) int { return len(b) - len(a) })
	return out
}
