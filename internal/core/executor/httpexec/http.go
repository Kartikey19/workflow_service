// Package httpexec implements the http node type (02 §4.1).
package httpexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/pkg/netguard"
)

const (
	defaultTimeout   = 30 * time.Second
	maxResponseBytes = 256 << 10
)

type Executor struct {
	policy netguard.Policy
}

func New(policy netguard.Policy) *Executor { return &Executor{policy: policy} }

func (*Executor) Type() string { return "http" }

func (*Executor) Describe() executor.Description {
	return executor.Description{
		Title: "HTTP request", Category: "integration",
		Description: "Calls an HTTP API. 5xx, 429 and timeouts are retried; other 4xx fail. Sends Idempotency-Key by default so retries are safe.",
		Example: `- id: create_ticket
  type: http
  timeout: 10s
  config:
    method: POST
    url: https://api.example.com/tickets
    headers:
      Authorization: "Bearer ${secrets.EXAMPLE_TOKEN}"
    body: {title: ${input.title}, priority: ${input.priority}}
    expect_status: [200, 201]`,
		Output: `{"status": 201, "headers": {...}, "body": <parsed JSON or text>}`,
	}
}

type config struct {
	Method             string            `json:"method"`
	URL                string            `json:"url"`
	Headers            map[string]string `json:"headers"`
	Query              map[string]any    `json:"query"`
	Body               json.RawMessage   `json:"body"`
	Timeout            string            `json:"timeout"`
	ExpectStatus       []int             `json:"expect_status"`
	SendIdempotencyKey *bool             `json:"send_idempotency_key"`
}

func (*Executor) ValidateConfig(raw json.RawMessage) error {
	if err := executor.CheckFields(raw, []executor.Field{
		{Name: "method", Kind: "string", Enum: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD"}},
		{Name: "url", Kind: "string", Required: true},
		{Name: "headers", Kind: "object"},
		{Name: "query", Kind: "object"},
		{Name: "body", Kind: "any"},
		{Name: "timeout", Kind: "duration"},
		{Name: "expect_status", Kind: "array"},
		{Name: "send_idempotency_key", Kind: "bool"},
	}); err != nil {
		return err
	}
	var c map[string]any
	_ = json.Unmarshal(raw, &c)
	if text, ok := c["url"].(string); ok && !executor.IsExpression(text) {
		if err := checkURL(text); err != nil {
			return err
		}
	}
	return nil
}

func checkURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("url must be an absolute http or https URL")
	}
	return nil
}

func (e *Executor) Execute(ctx context.Context, req executor.Request) (executor.Result, error) {
	var c config
	if err := executor.DecodeConfig(req.Config, &c); err != nil {
		return executor.Result{}, err
	}
	if err := checkURL(c.URL); err != nil {
		return executor.Result{}, executor.Permanent("bad_config", err)
	}
	method := strings.ToUpper(c.Method)
	if method == "" {
		method = http.MethodGet
		if len(c.Body) > 0 && string(c.Body) != "null" {
			method = http.MethodPost
		}
	}
	timeout := defaultTimeout
	if c.Timeout != "" {
		if parsed, err := time.ParseDuration(c.Timeout); err == nil && parsed > 0 {
			timeout = parsed
		}
	}
	target, _ := url.Parse(c.URL)
	if len(c.Query) > 0 {
		values := target.Query()
		for key, value := range c.Query {
			values.Set(key, scalar(value))
		}
		target.RawQuery = values.Encode()
	}

	var body io.Reader
	contentType := ""
	if len(c.Body) > 0 && string(c.Body) != "null" {
		var text string
		if json.Unmarshal(c.Body, &text) == nil {
			body = strings.NewReader(text) // a string body is sent verbatim
			contentType = "text/plain; charset=utf-8"
		} else {
			body = bytes.NewReader(c.Body)
			contentType = "application/json"
		}
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return executor.Result{}, executor.Permanent("bad_config", err)
	}
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	httpReq.Header.Set("User-Agent", "workflow-service/1")
	for key, value := range c.Headers {
		httpReq.Header.Set(key, value)
	}
	if c.SendIdempotencyKey == nil || *c.SendIdempotencyKey {
		// Stable across attempts, so a retried POST is deduplicated upstream (I5).
		httpReq.Header.Set("Idempotency-Key", req.IdempotencyKey)
	}

	started := time.Now()
	resp, err := e.policy.Client(timeout).Do(httpReq)
	if err != nil {
		return executor.Result{}, classifyTransport(ctx, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return executor.Result{}, executor.Retryable("upstream_read", fmt.Errorf("read response: %w", err))
	}
	if len(data) > maxResponseBytes {
		return executor.Result{}, executor.Permanent("response_too_large", fmt.Errorf("response body exceeds %d bytes", maxResponseBytes))
	}

	headers := make(map[string]string, len(resp.Header))
	for key := range resp.Header {
		headers[strings.ToLower(key)] = resp.Header.Get(key)
	}
	output, err := executor.JSON(map[string]any{
		"status": resp.StatusCode, "headers": headers, "body": decodeBody(resp.Header.Get("Content-Type"), data),
	})
	if err != nil {
		return executor.Result{}, err
	}
	metrics := map[string]any{"http_status": resp.StatusCode, "response_bytes": len(data), "latency_ms": time.Since(started).Milliseconds()}
	result := executor.Result{Output: output, Metrics: metrics}

	if statusAccepted(resp.StatusCode, c.ExpectStatus) {
		return result, nil
	}
	cause := fmt.Errorf("unexpected HTTP status %d from %s", resp.StatusCode, target.Host)
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return result, &executor.RetryableError{Code: "rate_limited", Err: cause, RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout:
		return result, &executor.RetryableError{Code: "upstream_5xx", Err: cause, RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	default:
		return result, executor.Permanent("upstream_4xx", cause)
	}
}

func statusAccepted(status int, expected []int) bool {
	if len(expected) > 0 {
		return slices.Contains(expected, status)
	}
	return status >= 200 && status < 300
}

func classifyTransport(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, netguard.ErrBlocked):
		return executor.Permanent("egress_blocked", err)
	case ctx.Err() != nil:
		return ctx.Err() // DeadlineExceeded → timeout (retryable); Canceled → permanent
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return executor.Retryable("timeout", err)
	}
	return executor.Retryable("connection_error", err)
}

func retryAfter(header string) time.Duration {
	if header == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && seconds > 0 {
		return min(time.Duration(seconds)*time.Second, time.Hour)
	}
	if at, err := http.ParseTime(header); err == nil {
		return min(max(time.Until(at), 0), time.Hour)
	}
	return 0
}

func decodeBody(contentType string, data []byte) any {
	if len(data) == 0 {
		return nil
	}
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") || (mediaType == "" && json.Valid(data)) {
		var decoded any
		if json.Unmarshal(data, &decoded) == nil {
			return decoded
		}
	}
	return string(data)
}

func scalar(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		encoded, _ := json.Marshal(v)
		return string(encoded)
	}
}
