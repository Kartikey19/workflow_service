package httpexec

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kartikey/workflow_service/internal/executor"
	"github.com/kartikey/workflow_service/internal/netguard"
)

func run(t *testing.T, e *Executor, config map[string]any) (executor.Result, error) {
	t.Helper()
	raw, _ := json.Marshal(config)
	return e.Execute(context.Background(), executor.Request{Config: raw, IdempotencyKey: "wf_KEY"})
}

func TestHTTPClassification(t *testing.T) {
	var gotKey, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("Idempotency-Key")
		data, _ := io.ReadAll(r.Body)
		gotBody = string(data)
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"id":7}`))
		case "/busy":
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(429)
		case "/down":
			w.WriteHeader(503)
		case "/bad":
			w.WriteHeader(400)
		case "/big":
			_, _ = w.Write([]byte(strings.Repeat("x", maxResponseBytes+10)))
		}
	}))
	defer server.Close()
	e := New(netguard.Policy{AllowPrivate: true})

	res, err := run(t, e, map[string]any{"method": "POST", "url": server.URL + "/ok", "body": map[string]any{"a": 1}})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Status int            `json:"status"`
		Body   map[string]any `json:"body"`
	}
	_ = json.Unmarshal(res.Output, &out)
	if out.Status != 201 || out.Body["id"] != float64(7) || gotKey != "wf_KEY" || gotBody != `{"a":1}` {
		t.Fatalf("output %s key %q body %q", res.Output, gotKey, gotBody)
	}

	_, err = run(t, e, map[string]any{"url": server.URL + "/busy"})
	var retryable *executor.RetryableError
	if !errors.As(err, &retryable) || retryable.Code != "rate_limited" || retryable.RetryAfter != 30*time.Second {
		t.Fatalf("429 err = %#v", err)
	}
	if _, err = run(t, e, map[string]any{"url": server.URL + "/down"}); !errors.As(err, &retryable) || retryable.Code != "upstream_5xx" {
		t.Fatalf("503 err = %v", err)
	}
	var permanent *executor.PermanentError
	if _, err = run(t, e, map[string]any{"url": server.URL + "/bad"}); !errors.As(err, &permanent) || permanent.Code != "upstream_4xx" {
		t.Fatalf("400 err = %v", err)
	}
	if _, err = run(t, e, map[string]any{"url": server.URL + "/big"}); !errors.As(err, &permanent) || permanent.Code != "response_too_large" {
		t.Fatalf("big err = %v", err)
	}
	if _, err = run(t, New(netguard.Policy{}), map[string]any{"url": server.URL + "/ok"}); !errors.As(err, &permanent) || permanent.Code != "egress_blocked" {
		t.Fatalf("loopback err = %v", err)
	}
}

func TestHTTPValidateConfig(t *testing.T) {
	e := New(netguard.Policy{})
	for raw, wantErr := range map[string]bool{
		`{"url":"https://x.test"}`:                      false,
		`{"url":"${input.url}","timeout":"${input.t}"}`: false,
		`{"url":"ftp://x"}`:                             true,
		`{"method":"FETCH","url":"https://x.test"}`:     true,
		`{"url":"https://x.test","bogus":1}`:            true,
		`{}`:                                            true,
		`{"url":"https://x.test","timeout":"ten secs"}`: true,
	} {
		if err := e.ValidateConfig(json.RawMessage(raw)); (err != nil) != wantErr {
			t.Errorf("%s: err = %v, wantErr %v", raw, err, wantErr)
		}
	}
}
