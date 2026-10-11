// Package expr parses workflow templates and expressions.
package expr

import (
	"fmt"
	"strings"
)

type Segment struct {
	Text         string
	IsExpression bool
}

// ParseTemplate separates literal text from ${...} expressions without evaluating
// CEL. $${ escapes an opener into literal ${. Expression whitespace is preserved.
// Empty input produces one empty literal. Errors report zero-based byte offsets,
// never source text, which may contain secrets.
func ParseTemplate(source string) ([]Segment, error) {
	var segments []Segment
	var literal strings.Builder
	for i := 0; i < len(source); {
		switch {
		case strings.HasPrefix(source[i:], "$${"):
			literal.WriteString("${")
			i += 3
		case strings.HasPrefix(source[i:], "${"):
			if literal.Len() > 0 {
				segments = append(segments, Segment{Text: literal.String()})
				literal.Reset()
			}
			end, err := expressionEnd(source, i+2)
			if err != nil {
				return nil, err
			}
			text := source[i+2 : end]
			if strings.TrimSpace(text) == "" {
				return nil, fmt.Errorf("empty template expression at byte %d", i)
			}
			segments = append(segments, Segment{Text: text, IsExpression: true})
			i = end + 1
		default:
			literal.WriteByte(source[i])
			i++
		}
	}
	if literal.Len() > 0 || len(segments) == 0 {
		segments = append(segments, Segment{Text: literal.String()})
	}
	return segments, nil
}

// expressionEnd finds the template's closing brace, ignoring braces inside CEL
// strings and line comments and balancing map/message literal braces.
func expressionEnd(source string, start int) (int, error) {
	depth := 0
	for i := start; i < len(source); {
		switch {
		case strings.HasPrefix(source[i:], "//"):
			for i < len(source) && source[i] != '\n' {
				i++
			}
		case source[i] == '\'' || source[i] == '"' || source[i] == '`':
			end, err := quotedEnd(source, i)
			if err != nil {
				return 0, err
			}
			i = end
		case source[i] == '{':
			depth++
			i++
		case source[i] == '}':
			if depth == 0 {
				return i, nil
			}
			depth--
			i++
		default:
			i++
		}
	}
	return 0, fmt.Errorf("unclosed template expression at byte %d", start-2)
}

// quotedEnd returns the first byte after a CEL string's delimiter. Raw strings
// do not interpret backslash escapes; byte strings use the same delimiters.
func quotedEnd(source string, start int) (int, error) {
	quote := source[start : start+1]
	delimiter := quote
	if quote != "`" && strings.HasPrefix(source[start:], strings.Repeat(quote, 3)) {
		delimiter = strings.Repeat(quote, 3)
	}
	raw := start > 0 && (source[start-1] == 'r' || source[start-1] == 'R')
	for i := start + len(delimiter); i < len(source); {
		if strings.HasPrefix(source[i:], delimiter) {
			return i + len(delimiter), nil
		}
		if !raw && source[i] == '\\' {
			i += 2
		} else {
			i++
		}
	}
	return 0, fmt.Errorf("unclosed quoted value at byte %d", start)
}
