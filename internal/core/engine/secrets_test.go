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
		"note":    "no secrets mentioned except secrets.PRESENT",
	}
	got := missingSecrets(config, map[string]string{"PRESENT": "x"})
	if len(got) != 2 || got[0] != "API_KEY" || got[1] != "OTHER" {
		t.Fatalf("missing = %v, want [API_KEY OTHER]", got)
	}
	if got := missingSecrets(config, map[string]string{"API_KEY": "x", "OTHER": "y", "PRESENT": "z"}); len(got) != 0 {
		t.Fatalf("missing = %v, want none", got)
	}
}
