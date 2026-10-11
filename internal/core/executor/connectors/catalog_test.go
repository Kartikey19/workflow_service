package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/pkg/netguard"
)

type savedConnector struct{ kind, token string }

func (s savedConnector) Connection(_ context.Context, _ uuid.UUID, name string, kinds ...string) (executor.Connection, error) {
	for _, kind := range kinds {
		if kind == s.kind {
			return executor.Connection{Name: name, Kind: s.kind, Secret: map[string]string{"access_token": s.token}}, nil
		}
	}
	return executor.Connection{}, executor.ErrConnectionNotFound
}
func TestWorkspaceFailures(t *testing.T) {
	for _, tt := range []struct {
		name       string
		status     int
		body, code string
		retry      bool
	}{
		{"slack-logical-auth", 200, `{"ok":false,"error":"invalid_auth"}`, "slack_error", false},
		{"slack-logical-limit", 200, `{"ok":false,"error":"ratelimited"}`, "rate_limited", true},
		{"http-limit", 429, `{"error":"rate limited"}`, "rate_limited", true},
		{"outage", 503, `{"error":"outage"}`, "upstream_5xx", true},
		{"invalid-json", 200, `broken`, "invalid_response", true},
		{"response-cap", 200, strings.Repeat("x", maxResponse+1), "response_too_large", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer upstream.Close()
			catalog := NewCatalog(savedConnector{"slack", "test-token"}, netguard.Policy{AllowPrivate: true}, Endpoints{"slack_api": upstream.URL}, "")
			_, err := catalog.Invoke(context.Background(), executor.Request{}, "slack.history", "saved", json.RawMessage(`{"channel":"C1"}`))
			var retry *executor.RetryableError
			var permanent *executor.PermanentError
			if tt.retry {
				if !errors.As(err, &retry) || retry.Code != tt.code {
					t.Fatalf("error %v", err)
				}
				if tt.status == 429 && retry.RetryAfter.Seconds() != 2 {
					t.Error("lost Retry-After")
				}
			} else if !errors.As(err, &permanent) || permanent.Code != tt.code {
				t.Fatalf("error %v", err)
			}
		})
	}
}
