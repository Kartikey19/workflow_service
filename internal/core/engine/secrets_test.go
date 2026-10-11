package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kartikey/workflow_service/internal/core/executor"
)

func TestScrubReplacesRawAndJSONEscapedValues(t *testing.T) {
	values := secretValueList(map[string]string{"A": "tok-abcdef", "B": `p"w\d-123456`, "C": "tok-abcdef-longer"})
	if values[0] != "tok-abcdef-longer" {
		t.Fatalf("values = %v, want the longest first so a containing value is replaced whole", values)
	}
	text := `Bearer tok-abcdef-longer and tok-abcdef; json: "p\"w\\d-123456"; raw: p"w\d-123456`
	got := scrub(text, values)
	for _, leaked := range []string{"tok-abcdef", `p\"w\\d-123456`, `p"w\d-123456`} {
		if strings.Contains(got, leaked) {
			t.Fatalf("scrub left %q in %q", leaked, got)
		}
	}
	if strings.Count(got, redactionMark) != 4 {
		t.Fatalf("scrub = %q, want four marks", got)
	}
	if scrub("nothing to hide", values) != "nothing to hide" {
		t.Fatal("scrub changed text without secrets")
	}
	if got := scrubBytes(nil, values); got != nil {
		t.Fatalf("scrubBytes(nil) = %v", got)
	}
}

func TestScrubErrorKeepsTheErrorClass(t *testing.T) {
	values := []string{"s3cr3t-value"}
	retry := &executor.RetryableError{Code: "upstream_5xx", Err: errors.New(`Post "https://x/?key=s3cr3t-value": boom`), RetryAfter: 7 * time.Second}
	got := scrubError(retry, values)
	var r *executor.RetryableError
	if !errors.As(got, &r) || r.Code != "upstream_5xx" || r.RetryAfter != 7*time.Second || strings.Contains(got.Error(), "s3cr3t") {
		t.Fatalf("retryable = %#v (%v)", got, got)
	}
	got = scrubError(executor.Permanent("bad", errors.New("token s3cr3t-value rejected")), values)
	var p *executor.PermanentError
	if !errors.As(got, &p) || p.Code != "bad" || strings.Contains(got.Error(), "s3cr3t") || strings.Count(got.Error(), "bad:") != 1 {
		t.Fatalf("permanent = %#v (%v)", got, got)
	}
	plain := errors.New("no secret here")
	if scrubError(plain, values) != plain {
		t.Fatal("an error without secrets must be returned unchanged")
	}
	if scrubError(nil, values) != nil {
		t.Fatal("nil error")
	}
	if got := scrubError(errors.New("has s3cr3t-value"), values); strings.Contains(got.Error(), "s3cr3t") {
		t.Fatalf("plain error = %v", got)
	}
}

func TestMissingSecrets(t *testing.T) {
	config := map[string]any{
		"headers": map[string]any{"Authorization": "Bearer ${secrets.API_KEY}", "X": `${secrets["OTHER"]}`},
		"body":    []any{"${double(secrets.llm.temperature)}", "${secrets['llm']['effort']}", "${input.secrets.NOT_A_REF}"},
		// Literal text is not a reference, even when it reads like one.
		"note":   "see secrets.NOTE for details",
		"source": "import secrets\ntoken = secrets.token_hex(16)",
	}
	uses, refs := secretUses(config)
	if !uses {
		t.Fatal("config uses secrets")
	}
	got := missingSecrets(refs, map[string]string{"PRESENT": "x"})
	want := []string{"API_KEY", "OTHER", "llm.effort", "llm.temperature"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("missing = %v, want %v", got, want)
	}
	values := map[string]string{"API_KEY": "x", "OTHER": "y", "llm.temperature": "0.3", "llm.effort": "high"}
	if got := missingSecrets(refs, values); len(got) != 0 {
		t.Fatalf("missing = %v, want none", got)
	}
	if got := missingSecrets(refs, map[string]string{"API_KEY": "x", "OTHER": "y", "llm.effort": "high"}); strings.Join(got, ",") != "llm.temperature" {
		t.Fatalf("missing = %v, want the absent group key by name", got)
	}
	// Mentions outside ${...} must not load the vault; a dynamic read must.
	if uses, _ := secretUses(map[string]any{"source": "import secrets", "note": "see secrets.md"}); uses {
		t.Fatal("literal text loaded the vault")
	}
	if uses, refs := secretUses(map[string]any{"v": "${secrets[input.name]}"}); !uses || len(refs) != 0 {
		t.Fatalf("dynamic read: uses=%v refs=%v", uses, refs)
	}
}
