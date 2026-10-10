package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Field describes one config key for publish-time validation. Publish sees the
// raw config, where any value may still be a ${...} expression string; such
// values pass the kind check and are validated again at execution.
type Field struct {
	Name     string
	Kind     string // string | number | integer | bool | object | array | duration | any
	Required bool
	Enum     []string
}

// IsExpression reports whether v is a string containing a ${...} expression.
func IsExpression(v any) bool {
	s, ok := v.(string)
	return ok && strings.Contains(s, "${")
}

// CheckFields validates a raw config object against fields. Unknown keys are
// rejected so typos surface at publish time.
func CheckFields(raw json.RawMessage, fields []Field) error {
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil || config == nil {
		return errors.New("config must be an object")
	}
	known := make(map[string]Field, len(fields))
	var problems []string
	for _, field := range fields {
		known[field.Name] = field
		value, present := config[field.Name]
		if !present || value == nil {
			if field.Required {
				problems = append(problems, fmt.Sprintf("%s is required", field.Name))
			}
			continue
		}
		if message := checkKind(field, value); message != "" {
			problems = append(problems, field.Name+" "+message)
		}
	}
	var unknown []string
	for key := range config {
		if _, ok := known[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	for _, key := range unknown {
		problems = append(problems, fmt.Sprintf("unknown field %q", key))
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func checkKind(field Field, value any) string {
	if IsExpression(value) {
		return ""
	}
	switch field.Kind {
	case "string":
		text, ok := value.(string)
		if !ok {
			return "must be a string"
		}
		if len(field.Enum) > 0 {
			for _, allowed := range field.Enum {
				if strings.EqualFold(text, allowed) {
					return ""
				}
			}
			return "must be one of " + strings.Join(field.Enum, ", ")
		}
	case "number":
		if _, ok := value.(float64); !ok {
			return "must be a number"
		}
	case "integer":
		number, ok := value.(float64)
		if !ok || number != float64(int64(number)) {
			return "must be an integer"
		}
	case "bool":
		if _, ok := value.(bool); !ok {
			return "must be true or false"
		}
	case "object":
		if _, ok := value.(map[string]any); !ok {
			return "must be an object"
		}
	case "array":
		if _, ok := value.([]any); !ok {
			return "must be a list"
		}
	case "duration":
		text, ok := value.(string)
		if !ok {
			return "must be a duration such as 30s"
		}
		if d, err := time.ParseDuration(text); err != nil || d <= 0 {
			return "must be a positive duration such as 30s"
		}
	}
	return ""
}

// DecodeConfig decodes a resolved config into dst; a failure is permanent
// because retrying the same config cannot succeed.
func DecodeConfig(raw json.RawMessage, dst any) error {
	if err := json.Unmarshal(raw, dst); err != nil {
		return &PermanentError{Code: "bad_config", Err: fmt.Errorf("decode resolved config: %w", err)}
	}
	return nil
}

// Permanent wraps err as a permanent error with code.
func Permanent(code string, err error) error { return &PermanentError{Code: code, Err: err} }

// Retryable wraps err as a retryable error with code.
func Retryable(code string, err error) error { return &RetryableError{Code: code, Err: err} }

// JSON encodes v as node output.
func JSON(v any) (json.RawMessage, error) {
	encoded, err := json.Marshal(v)
	if err != nil {
		return nil, &PermanentError{Code: "invalid_output", Err: err}
	}
	return encoded, nil
}
