package connectors

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/pkg/httpx"
	"github.com/kartikey/workflow_service/pkg/netguard"
)

const (
	requestTimeout = 60 * time.Second
	// MaxUploadBytes bounds the content of one drive upload: simple (non-resumable)
	// uploads read it all into memory.
	MaxUploadBytes = 8 << 20
	maxResponse    = 1 << 20
)

// base is what every connector needs.
type base struct {
	conns  executor.Connections
	policy netguard.Policy
	ep     Endpoints
}

func (b base) client() *http.Client { return b.policy.Client(requestTimeout) }

// connection fetches the named connection, registers its tokens for scrubbing
// and maps lookup failures onto the retry model.
func (b base) connection(ctx context.Context, req executor.Request, name, kind string) (executor.Connection, error) {
	if name == "" {
		return executor.Connection{}, executor.Permanent("bad_config", errors.New("connection is required"))
	}
	if b.conns == nil {
		return executor.Connection{}, executor.Permanent("bad_config", errors.New("saved connections are not available on this worker"))
	}
	conn, err := b.conns.Connection(ctx, req.TenantID, name, kind)
	switch {
	case errors.Is(err, executor.ErrConnectionNotFound):
		return executor.Connection{}, executor.Permanent("E_CONNECTION_NOT_FOUND", fmt.Errorf("no %s connection named %q", kind, name))
	case errors.Is(err, executor.ErrConnectionNeedsAuth):
		return executor.Connection{}, executor.Permanent("E_CONNECTION_REAUTH", fmt.Errorf("connection %q lost access; reconnect it", name))
	case err != nil:
		return executor.Connection{}, executor.Retryable("connection_lookup", err)
	}
	for _, v := range conn.Secret {
		req.RedactSecrets(v)
	}
	if conn.Secret["access_token"] == "" {
		return executor.Connection{}, executor.Permanent("E_CONNECTION_REAUTH", fmt.Errorf("connection %q has no access token; reconnect it", name))
	}
	return conn, nil
}

// content is the data of an upload: text, or base64 for binary.
type content struct {
	Content       *string `json:"content"`
	ContentBase64 *string `json:"content_base64"`
}

func (c content) bytes(limit int) ([]byte, error) {
	switch {
	case c.Content != nil && c.ContentBase64 != nil:
		return nil, errors.New("set only one of content and content_base64")
	case c.Content != nil:
		if len(*c.Content) > limit {
			return nil, fmt.Errorf("content is over the %d byte limit", limit)
		}
		return []byte(*c.Content), nil
	case c.ContentBase64 != nil:
		if base64.StdEncoding.DecodedLen(len(*c.ContentBase64)) > limit+3 {
			return nil, fmt.Errorf("content is over the %d byte limit", limit)
		}
		data, err := base64.StdEncoding.DecodeString(*c.ContentBase64)
		if err != nil {
			return nil, errors.New("content_base64 is not valid base64")
		}
		return data, nil
	default:
		return nil, errors.New("content or content_base64 is required")
	}
}

// contentFields are the fields every upload node shares.
var contentFields = []executor.Field{
	{Name: "content", Kind: "string"},
	{Name: "content_base64", Kind: "string"},
}

func requireContent(raw json.RawMessage) error {
	var c map[string]any
	_ = json.Unmarshal(raw, &c)
	_, text := c["content"]
	_, binary := c["content_base64"]
	switch {
	case text && binary:
		return errors.New("set only one of content and content_base64")
	case !text && !binary:
		return errors.New("content or content_base64 is required")
	}
	return nil
}

// do sends the request, reads a bounded response and classifies failures.
func (b base) do(ctx context.Context, req *http.Request, what string) ([]byte, error) {
	res, err := b.client().Do(req)
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case errors.Is(err, netguard.ErrBlocked):
			return nil, executor.Permanent("egress_blocked", err)
		}
		return nil, executor.Retryable("connection_error", fmt.Errorf("%s could not be reached", what))
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if err != nil {
		return nil, executor.Retryable("upstream_read", fmt.Errorf("read the response from %s", what))
	}
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return data, nil
	}
	return nil, classify(res.StatusCode, res.Header, data, what)
}

// classify maps an HTTP failure from a provider onto the retry model: rate
// limits and outages are transient; bad credentials, missing things and rejected
// requests are not.
func classify(status int, header http.Header, body []byte, what string) error {
	cause := fmt.Errorf("%s answered HTTP %d: %s", what, status, summarize(body))
	retryAfter := httpx.ParseRetryAfter(header.Get("Retry-After"))
	switch {
	case status == http.StatusTooManyRequests:
		return &executor.RetryableError{Code: "rate_limited", Err: cause, RetryAfter: retryAfter}
	case status >= 500 || status == http.StatusRequestTimeout:
		return &executor.RetryableError{Code: "upstream_5xx", Err: cause, RetryAfter: retryAfter}
	case status == http.StatusForbidden && isRateLimit(body):
		return &executor.RetryableError{Code: "rate_limited", Err: cause, RetryAfter: retryAfter}
	case status == http.StatusUnauthorized:
		return executor.Permanent("auth", fmt.Errorf("%s rejected the credentials; reconnect the connection", what))
	case status == http.StatusForbidden:
		return executor.Permanent("forbidden", cause)
	case status == http.StatusNotFound:
		return executor.Permanent("not_found", cause)
	default:
		return executor.Permanent("upstream_4xx", cause)
	}
}

// isRateLimit recognises Google's 403 rate-limit replies, which are transient.
func isRateLimit(body []byte) bool {
	text := string(body)
	return strings.Contains(text, "rateLimitExceeded") || strings.Contains(text, "userRateLimitExceeded") ||
		strings.Contains(text, "RATE_LIMIT_EXCEEDED")
}

// summarize extracts a short message from a JSON error body, else a clipped
// copy of the text.
func summarize(body []byte) string {
	var decoded struct {
		Error   any    `json:"error"`
		Message string `json:"error_summary"`
	}
	if json.Unmarshal(body, &decoded) == nil {
		switch e := decoded.Error.(type) {
		case map[string]any:
			if m, ok := e["message"].(string); ok && m != "" {
				return m
			}
		case string:
			return e
		}
		if decoded.Message != "" {
			return decoded.Message
		}
	}
	text := strings.TrimSpace(string(body[:min(len(body), 200)]))
	if text == "" {
		return "(no body)"
	}
	return text
}

// authorize sets the bearer token.
func authorize(req *http.Request, conn executor.Connection) {
	req.Header.Set("Authorization", "Bearer "+conn.Secret["access_token"])
}
