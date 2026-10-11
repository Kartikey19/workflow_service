package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

type fakeReadinessChecker struct {
	err    error
	called bool
}

func (f *fakeReadinessChecker) Ping(context.Context) error {
	f.called = true
	return f.err
}

func TestHealthzDoesNotCheckDatabase(t *testing.T) {
	checker := &fakeReadinessChecker{}
	server := newTestServer(t, checker)

	response := serve(server, http.MethodGet, "/healthz")

	if response.Code != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want %d", response.Code, http.StatusOK)
	}
	if checker.called {
		t.Fatal("GET /healthz called Ping, want no database check")
	}
	assertStatus(t, response, "ok")
}

func TestReadyzReportsReadyWhenDatabaseIsAvailable(t *testing.T) {
	checker := &fakeReadinessChecker{}
	server := newTestServer(t, checker)

	response := serve(server, http.MethodGet, "/readyz")

	if response.Code != http.StatusOK {
		t.Fatalf("GET /readyz status = %d, want %d", response.Code, http.StatusOK)
	}
	if !checker.called {
		t.Fatal("GET /readyz did not call Ping")
	}
	assertStatus(t, response, "ready")
}

func TestReadyzDoesNotExposeDatabaseError(t *testing.T) {
	checker := &fakeReadinessChecker{
		err: errors.New("password authentication failed for super-secret-user"),
	}
	server := newTestServer(t, checker)

	response := serve(server, http.MethodGet, "/readyz")

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	assertStatus(t, response, "not_ready")
	if strings.Contains(response.Body.String(), "super-secret-user") {
		t.Fatalf("GET /readyz leaked database error: %q", response.Body.String())
	}
}

func TestHealthRoutesRejectPost(t *testing.T) {
	server := newTestServer(t, &fakeReadinessChecker{})

	response := serve(server, http.MethodPost, "/healthz")

	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
}

func TestNewValidatesRequiredDependencies(t *testing.T) {
	_, err := New(Config{Address: "127.0.0.1:0"}, nil, zap.NewNop())
	if err == nil || !strings.Contains(err.Error(), "readiness checker is required") {
		t.Fatalf("New() error = %v, want missing readiness checker error", err)
	}

	_, err = New(Config{}, &fakeReadinessChecker{}, zap.NewNop())
	if err == nil || !strings.Contains(err.Error(), "HTTP_ADDRESS is required") {
		t.Fatalf("New() error = %v, want missing address error", err)
	}
}

func newTestServer(t *testing.T, checker ReadinessChecker) *Server {
	t.Helper()

	server, err := New(Config{
		Address:          "127.0.0.1:0",
		ReadinessTimeout: time.Second,
	}, checker, zap.NewNop())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return server
}

func serve(server *Server, method, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)
	return response
}

func assertStatus(t *testing.T, response *httptest.ResponseRecorder, want string) {
	t.Helper()

	var body statusResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Status != want {
		t.Fatalf("response status = %q, want %q", body.Status, want)
	}
}
