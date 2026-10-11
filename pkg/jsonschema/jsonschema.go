// Package jsonschema validates JSON values against the small JSON Schema subset
// used by wait_event payloads and agent output schemas: type, enum, const,
// required, properties, additionalProperties (boolean), items, minItems,
// maxItems, minLength, maxLength, pattern, minimum and maximum. Anything
// else is rejected by Check so authors learn at publish time, not at runtime.
package jsonschema

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

var keywords = map[string]bool{
	"type": true, "enum": true, "const": true, "required": true, "properties": true,
	"additionalProperties": true, "items": true, "minItems": true, "maxItems": true,
	"minLength": true, "maxLength": true, "pattern": true, "minimum": true, "maximum": true,
	"description": true, "title": true, "default": true, "format": true,
}

var types = map[string]bool{"object": true, "array": true, "string": true, "number": true, "integer": true, "boolean": true, "null": true}

// FieldError is one violation at a JSON path such as $.items[2].price.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Check reports an error when schema uses an unsupported keyword or shape.
func Check(schema any) error {
	return check(schema, "$")
}

func check(schema any, path string) error {
	object, ok := schema.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: schema must be an object", path)
	}
	for key := range object {
		if !keywords[key] {
			return fmt.Errorf("%s: unsupported schema keyword %q", path, key)
		}
	}
	if value, ok := object["type"]; ok {
		name, isString := value.(string)
		if !isString || !types[name] {
			return fmt.Errorf("%s: type must be one of object, array, string, number, integer, boolean, null", path)
		}
	}
	if value, ok := object["required"]; ok {
		list, isList := value.([]any)
		if !isList {
			return fmt.Errorf("%s: required must be a list of property names", path)
		}
		for _, item := range list {
			if _, isString := item.(string); !isString {
				return fmt.Errorf("%s: required must be a list of property names", path)
			}
		}
	}
	if value, ok := object["properties"]; ok {
		properties, isObject := value.(map[string]any)
		if !isObject {
			return fmt.Errorf("%s: properties must be an object", path)
		}
		for name, child := range properties {
			if err := check(child, path+"."+name); err != nil {
				return err
			}
		}
	}
	if value, ok := object["additionalProperties"]; ok {
		if _, isBool := value.(bool); !isBool {
			return fmt.Errorf("%s: additionalProperties must be true or false", path)
		}
	}
	if value, ok := object["items"]; ok {
		if err := check(value, path+"[]"); err != nil {
			return err
		}
	}
	if value, ok := object["enum"]; ok {
		if _, isList := value.([]any); !isList {
			return fmt.Errorf("%s: enum must be a list", path)
		}
	}
	if value, ok := object["pattern"]; ok {
		text, isString := value.(string)
		if !isString {
			return fmt.Errorf("%s: pattern must be a string", path)
		}
		if _, err := regexp.Compile(text); err != nil {
			return fmt.Errorf("%s: pattern is not a valid regular expression", path)
		}
	}
	for _, key := range []string{"minItems", "maxItems", "minLength", "maxLength", "minimum", "maximum"} {
		if value, ok := object[key]; ok {
			if _, isNumber := number(value); !isNumber {
				return fmt.Errorf("%s: %s must be a number", path, key)
			}
		}
	}
	return nil
}

// ValidateJSON decodes data and validates it against schema.
func ValidateJSON(schema any, data []byte) []FieldError {
	var value any
	if len(data) == 0 {
		data = []byte("null")
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return []FieldError{{Field: "$", Message: "is not valid JSON"}}
	}
	return Validate(schema, value)
}

// Validate returns every violation, sorted by field path.
func Validate(schema, value any) []FieldError {
	var errs []FieldError
	validate(schema, value, "$", &errs)
	sort.SliceStable(errs, func(i, j int) bool { return errs[i].Field < errs[j].Field })
	return errs
}

func validate(schema, value any, path string, errs *[]FieldError) {
	object, ok := schema.(map[string]any)
	if !ok {
		return
	}
	add := func(format string, args ...any) {
		*errs = append(*errs, FieldError{Field: path, Message: fmt.Sprintf(format, args...)})
	}
	if name, ok := object["type"].(string); ok && !isType(name, value) {
		add("must be %s, got %s", article(name), describe(value))
		return
	}
	if options, ok := object["enum"].([]any); ok {
		match := false
		for _, option := range options {
			if equal(option, value) {
				match = true
				break
			}
		}
		if !match {
			add("must be one of %s", list(options))
		}
	}
	if expected, ok := object["const"]; ok && !equal(expected, value) {
		add("must equal %v", expected)
	}
	switch v := value.(type) {
	case map[string]any:
		properties, _ := object["properties"].(map[string]any)
		if required, ok := object["required"].([]any); ok {
			for _, item := range required {
				name, _ := item.(string)
				if _, present := v[name]; !present {
					*errs = append(*errs, FieldError{Field: path + "." + name, Message: "is required"})
				}
			}
		}
		for name, child := range v {
			if childSchema, ok := properties[name]; ok {
				validate(childSchema, child, path+"."+name, errs)
			} else if additional, ok := object["additionalProperties"].(bool); ok && !additional {
				*errs = append(*errs, FieldError{Field: path + "." + name, Message: "is not an allowed property"})
			}
		}
	case []any:
		if minimum, ok := number(object["minItems"]); ok && float64(len(v)) < minimum {
			add("must have at least %v items", minimum)
		}
		if maximum, ok := number(object["maxItems"]); ok && float64(len(v)) > maximum {
			add("must have at most %v items", maximum)
		}
		if items, ok := object["items"]; ok {
			for i, item := range v {
				validate(items, item, fmt.Sprintf("%s[%d]", path, i), errs)
			}
		}
	case string:
		length := float64(len([]rune(v)))
		if minimum, ok := number(object["minLength"]); ok && length < minimum {
			add("must be at least %v characters", minimum)
		}
		if maximum, ok := number(object["maxLength"]); ok && length > maximum {
			add("must be at most %v characters", maximum)
		}
		if pattern, ok := object["pattern"].(string); ok {
			if re, err := regexp.Compile(pattern); err == nil && !re.MatchString(v) {
				add("must match %s", pattern)
			}
		}
	default:
		if n, isNumber := number(value); isNumber {
			if minimum, ok := number(object["minimum"]); ok && n < minimum {
				add("must be at least %v", minimum)
			}
			if maximum, ok := number(object["maximum"]); ok && n > maximum {
				add("must be at most %v", maximum)
			}
		}
	}
}

func isType(name string, value any) bool {
	switch name {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	case "number":
		_, ok := number(value)
		return ok
	case "integer":
		n, ok := number(value)
		return ok && n == math.Trunc(n)
	}
	return false
}

func number(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func equal(a, b any) bool {
	if x, ok := number(a); ok {
		y, ok := number(b)
		return ok && x == y
	}
	return reflect.DeepEqual(a, b)
}

func describe(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	}
	if _, ok := number(value); ok {
		return "a number"
	}
	return fmt.Sprintf("%T", value)
}

func article(name string) string {
	switch name {
	case "object", "array", "integer":
		return "an " + name
	case "null":
		return "null"
	}
	return "a " + name
}

func list(options []any) string {
	parts := make([]string, len(options))
	for i, option := range options {
		encoded, _ := json.Marshal(option)
		parts[i] = string(encoded)
	}
	return strings.Join(parts, ", ")
}
