package spec

import (
	"fmt"
	"math"
	"net/mail"
	"reflect"
	"regexp"
	"sort"
	"time"
	"uuid"
)

// FieldError is one input validation failure, reported per field (02 §7).
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidateInput checks run input against the declared inputs, applies defaults
// and returns the resolved input to persist. Undeclared fields pass through so
// webhook payloads can carry extra data. A spec without inputs accepts any object.
func ValidateInput(inputs map[string]InputField, given map[string]any) (map[string]any, []FieldError) {
	resolved := make(map[string]any, len(given)+len(inputs))
	for key, value := range given {
		resolved[key] = value
	}
	var errs []FieldError
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		field := inputs[name]
		value, present := resolved[name]
		if !present || value == nil {
			if field.Default != nil {
				resolved[name] = field.Default
				continue
			}
			if field.Required {
				errs = append(errs, FieldError{name, "is required"})
			}
			continue
		}
		if message := checkValue(field, value); message != "" {
			errs = append(errs, FieldError{name, message})
		}
	}
	return resolved, errs
}

func checkValue(field InputField, value any) string {
	switch field.Type {
	case "", "any":
	case "string":
		text, ok := value.(string)
		if !ok {
			return "must be a string"
		}
		if field.Pattern != "" {
			pattern, err := regexp.Compile(field.Pattern)
			if err != nil || !pattern.MatchString(text) {
				return fmt.Sprintf("must match pattern %q", field.Pattern)
			}
		}
		switch field.Format {
		case "uuid":
			if _, err := uuid.Parse(text); err != nil {
				return "must be a UUID"
			}
		case "date-time":
			if _, err := time.Parse(time.RFC3339, text); err != nil {
				return "must be an RFC 3339 date-time"
			}
		case "email":
			if address, err := mail.ParseAddress(text); err != nil || address.Address != text {
				return "must be an email address"
			}
		}
	case "number", "integer":
		number, ok := toFloat(value)
		if !ok {
			return "must be a number"
		}
		if field.Type == "integer" && number != math.Trunc(number) {
			return "must be an integer"
		}
		if field.Minimum != nil && number < *field.Minimum {
			return fmt.Sprintf("must be at least %v", *field.Minimum)
		}
		if field.Maximum != nil && number > *field.Maximum {
			return fmt.Sprintf("must be at most %v", *field.Maximum)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return "must be a boolean"
		}
	case "object":
		if _, ok := value.(map[string]any); !ok {
			return "must be an object"
		}
	case "array":
		if _, ok := value.([]any); !ok {
			return "must be an array"
		}
	default:
		return fmt.Sprintf("has unsupported type %q", field.Type)
	}
	if len(field.Enum) > 0 {
		for _, allowed := range field.Enum {
			if equalValues(allowed, value) {
				return ""
			}
		}
		return fmt.Sprintf("must be one of %v", field.Enum)
	}
	return ""
}

func toFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case int32:
		return float64(v), true
	case uint64:
		return float64(v), true
	}
	return 0, false
}

// equalValues compares YAML-decoded enum values with JSON-decoded input values,
// where the same number may arrive as int or float64.
func equalValues(a, b any) bool {
	if x, ok := toFloat(a); ok {
		y, ok := toFloat(b)
		return ok && x == y
	}
	return reflect.DeepEqual(a, b)
}

// validateInputSchema checks the declared inputs themselves at publish time.
func validateInputSchema(inputs map[string]InputField) []Diagnostic {
	var diagnostics []Diagnostic
	add := func(name, message string) {
		diagnostics = append(diagnostics, Diagnostic{
			Severity: "error", Code: "E_INPUT_SCHEMA", Path: "inputs." + name, Message: message,
		})
	}
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		field := inputs[name]
		switch field.Type {
		case "", "any", "string", "number", "integer", "boolean", "object", "array":
		default:
			add(name, fmt.Sprintf("type %q must be string, number, integer, boolean, object or array", field.Type))
			continue
		}
		switch field.Format {
		case "", "uuid", "date-time", "email":
		default:
			add(name, fmt.Sprintf("format %q must be uuid, date-time or email", field.Format))
		}
		if field.Pattern != "" {
			if _, err := regexp.Compile(field.Pattern); err != nil {
				add(name, "pattern is not a valid regular expression")
			}
		}
		if field.Default != nil {
			if message := checkValue(field, field.Default); message != "" {
				add(name, "default "+message)
			}
		}
	}
	return diagnostics
}
