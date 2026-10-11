package llm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestTransportErrorPreservesContextAndClassifiesFailures(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		code string
	}{
		{"connection", errors.New("connection refused"), "connection_error"},
		{"timeout", fmt.Errorf("dial: %w", &net.DNSError{IsTimeout: true}), "timeout"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var failure *Error
			err := TransportError(context.Background(), tt.err)
			if !errors.As(err, &failure) || failure.Code != tt.code || !failure.Retryable || failure.Message != tt.err.Error() {
				t.Fatalf("transport failure = %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := TransportError(ctx, errors.New("request failed")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled call = %v", err)
	}
	ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := TransportError(ctx, errors.New("request failed")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired call = %v", err)
	}
}

func TestStatusErrorRetryPolicyAndProviderDetails(t *testing.T) {
	for _, tt := range []struct {
		status    int
		retryable bool
	}{
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusForbidden, false},
		{http.StatusRequestTimeout, true},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusServiceUnavailable, true},
		{529, true}, // Anthropic's overloaded response.
	} {
		t.Run(http.StatusText(tt.status)+fmt.Sprint(tt.status), func(t *testing.T) {
			res := &http.Response{StatusCode: tt.status, Header: http.Header{"Retry-After": {"3"}}}
			var failure *Error
			err := StatusError(res, "provider_code", "provider detail", []byte("fallback"))
			if !errors.As(err, &failure) || failure.Status != tt.status || failure.Code != "provider_code" || failure.Message != "provider detail" || failure.Retryable != tt.retryable || failure.RetryAfter != 3*time.Second {
				t.Fatalf("status failure = %+v", failure)
			}
		})
	}
}

func TestStatusErrorBoundsFallbackBody(t *testing.T) {
	res := &http.Response{StatusCode: http.StatusBadGateway}
	for _, size := range []int{0, 10, 200, 300} {
		body := make([]byte, size)
		for i := range body {
			body[i] = 'x'
		}
		var failure *Error
		if err := StatusError(res, "", "", body); !errors.As(err, &failure) || len(failure.Message) != min(size, 200) {
			t.Fatalf("fallback body size %d = %+v", size, failure)
		}
	}
}
