package connectors

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/pkg/netguard"
)

type fixedConnections struct {
	conn executor.Connection
	err  error
}

func (f fixedConnections) Connection(_ context.Context, _ uuid.UUID, _ string, _ ...string) (executor.Connection, error) {
	return f.conn, f.err
}

func request(config string) executor.Request {
	return executor.Request{TenantID: uuid.New(), IdempotencyKey: "wf_key", Config: []byte(config)}
}

func TestUploadSizeLimitsAreEnforcedBeforeAnyRequest(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer server.Close()
	ep := Endpoints{"gdrive_api": server.URL, "gdrive_upload": server.URL, "dropbox_content": server.URL, "graph_api": server.URL}
	conns := fixedConnections{conn: executor.Connection{Name: "c", Secret: map[string]string{"access_token": "tok"}}}
	policy := netguard.Policy{AllowPrivate: true}

	big := func(n int) string { return strings.Repeat("a", n) }
	bigB64 := func(n int) string { return base64.StdEncoding.EncodeToString([]byte(big(n))) }
	for name, tt := range map[string]struct {
		run    func() (executor.Result, error)
		reason string
	}{
		"drive text over 8 MiB": {func() (executor.Result, error) {
			return NewGDrive(conns, policy, ep).Execute(context.Background(), request(`{"connection":"c","name":"f","content":"`+big(MaxUploadBytes+1)+`"}`))
		}, "over the"},
		"drive base64 over 8 MiB": {func() (executor.Result, error) {
			return NewGDrive(conns, policy, ep).Execute(context.Background(), request(`{"connection":"c","name":"f","content_base64":"`+bigB64(MaxUploadBytes+10)+`"}`))
		}, "over the"},
		"dropbox over 8 MiB": {func() (executor.Result, error) {
			return NewDropbox(conns, policy, ep).Execute(context.Background(), request(`{"connection":"c","path":"/f","content":"`+big(MaxUploadBytes+1)+`"}`))
		}, "over the"},
		"onedrive over 4 MiB": {func() (executor.Result, error) {
			return NewOneDrive(conns, policy, ep).Execute(context.Background(), request(`{"connection":"c","path":"f","content":"`+big(maxOneDriveBytes+1)+`"}`))
		}, "over the"},
		"onedrive base64 bad": {func() (executor.Result, error) {
			return NewOneDrive(conns, policy, ep).Execute(context.Background(), request(`{"connection":"c","path":"f","content_base64":"%%%"}`))
		}, "not valid base64"},
		"dropbox relative path": {func() (executor.Result, error) {
			return NewDropbox(conns, policy, ep).Execute(context.Background(), request(`{"connection":"c","path":"f","content":"x"}`))
		}, "starting with"},
		"dropbox directory path": {func() (executor.Result, error) {
			return NewDropbox(conns, policy, ep).Execute(context.Background(), request(`{"connection":"c","path":"/dir/","content":"x"}`))
		}, "starting with"},
		"drive slash in name": {func() (executor.Result, error) {
			return NewGDrive(conns, policy, ep).Execute(context.Background(), request(`{"connection":"c","name":"a/b","content":"x"}`))
		}, "file name"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tt.run()
			var permanent *executor.PermanentError
			if !errors.As(err, &permanent) || permanent.Code != "bad_config" || !strings.Contains(err.Error(), tt.reason) {
				t.Fatalf("err = %v, want a permanent bad_config containing %q", err, tt.reason)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("the provider was called %d times for requests that must be refused locally", calls)
	}
}

func TestConnectionLookupFailuresMapToTheRetryModel(t *testing.T) {
	policy := netguard.Policy{AllowPrivate: true}
	cfg := `{"connection":"c","path":"f","content":"x"}`
	for name, tt := range map[string]struct {
		conns     executor.Connections
		retryable bool
		code      string
	}{
		"not found":       {fixedConnections{err: executor.ErrConnectionNotFound}, false, "E_CONNECTION_NOT_FOUND"},
		"needs reconnect": {fixedConnections{err: executor.ErrConnectionNeedsAuth}, false, "E_CONNECTION_REAUTH"},
		"no token":        {fixedConnections{conn: executor.Connection{Secret: map[string]string{}}}, false, "E_CONNECTION_REAUTH"},
		"database down":   {fixedConnections{err: errors.New("db down")}, true, "connection_lookup"},
		"no connections":  {nil, false, "bad_config"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewOneDrive(tt.conns, policy, nil).Execute(context.Background(), request(cfg))
			var retryable *executor.RetryableError
			var permanent *executor.PermanentError
			switch {
			case tt.retryable && errors.As(err, &retryable) && retryable.Code == tt.code:
			case !tt.retryable && errors.As(err, &permanent) && permanent.Code == tt.code:
			default:
				t.Fatalf("err = %#v, want code %s (retryable=%v)", err, tt.code, tt.retryable)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	rate := http.Header{"Retry-After": {"7"}}
	for name, tt := range map[string]struct {
		status    int
		header    http.Header
		body      string
		retryable bool
		code      string
	}{
		"429 with Retry-After":  {429, rate, "", true, "rate_limited"},
		"503":                   {503, nil, "", true, "upstream_5xx"},
		"408":                   {408, nil, "", true, "upstream_5xx"},
		"google 403 rate limit": {403, nil, `{"error":{"errors":[{"reason":"userRateLimitExceeded"}]}}`, true, "rate_limited"},
		"403 forbidden":         {403, nil, `{"error":{"message":"insufficient scope"}}`, false, "forbidden"},
		"401 credentials":       {401, nil, "", false, "auth"},
		"404":                   {404, nil, "", false, "not_found"},
		"409 conflict":          {409, nil, `{"error_summary":"path/conflict/file"}`, false, "upstream_4xx"},
	} {
		t.Run(name, func(t *testing.T) {
			err := classify(tt.status, tt.header, []byte(tt.body), "Service")
			var retryable *executor.RetryableError
			var permanent *executor.PermanentError
			switch {
			case tt.retryable && errors.As(err, &retryable) && retryable.Code == tt.code:
				if tt.header != nil && retryable.RetryAfter.Seconds() != 7 {
					t.Fatalf("RetryAfter = %v", retryable.RetryAfter)
				}
			case !tt.retryable && errors.As(err, &permanent) && permanent.Code == tt.code:
			default:
				t.Fatalf("err = %#v, want %s (retryable=%v)", err, tt.code, tt.retryable)
			}
		})
	}
}

func TestAsciiJSONAndEndpoints(t *testing.T) {
	got := asciiJSON([]byte(`{"path":"/é/😀"}`))
	if want := "{\"path\":\"/\\u00e9/\\ud83d\\ude00\"}"; got != want {
		t.Fatalf("asciiJSON = %s, want %s", got, "{\"path\":\"/\\u00e9/\\ud83d\\ude00\"}")
	}
	ep, err := ParseEndpoints(`{"gmail_api":"http://proxy.local/"}`)
	if err != nil || ep.URL("gmail_api", "") != "http://proxy.local" || ep.URL("dropbox_api", "") != "https://api.dropboxapi.com" {
		t.Fatalf("endpoints = %v, %v", ep, err)
	}
	if got := ep.URL("microsoft_token", "contoso"); got != "https://login.microsoftonline.com/contoso/oauth2/v2.0/token" {
		t.Fatalf("tenant substitution = %s", got)
	}
	if _, err := ParseEndpoints(`{"nope":"x"}`); err == nil {
		t.Fatal("an unknown key was accepted")
	}
}
