package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

type observedListener struct {
	net.Listener
	closed chan struct{}
	once   sync.Once
}

func (l *observedListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { close(l.closed) })
	return err
}
func listenForTest(t *testing.T) *observedListener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return &observedListener{Listener: l, closed: make(chan struct{})}
}
func await[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for lifecycle completion")
		var zero T
		return zero
	}
}
func TestRunDrainsActiveRequestBeforeReturning(t *testing.T) {
	server := newTestServer(t, &fakeReadinessChecker{})
	listener := listenForTest(t)
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	server.httpServer.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- r.Context()
		<-release
		_, _ = io.WriteString(w, "finished")
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, listener, time.Second) }()
	response := make(chan string, 1)
	go func() {
		client := &http.Client{Timeout: 3 * time.Second}
		res, err := client.Get("http://" + listener.Addr().String())
		if err != nil {
			response <- err.Error()
			return
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		response <- string(body)
	}()
	requestCtx := await(t, entered)
	cancel()
	await(t, listener.closed)
	if requestCtx.Err() != nil {
		t.Fatal("request cancelled before grace period ended")
	}
	select {
	case err := <-done:
		t.Fatalf("returned before handler completed: %v", err)
	default:
	}
	releaseOnce.Do(func() { close(release) })
	if got := await(t, response); got != "finished" {
		t.Fatalf("response = %q", got)
	}
	if err := await(t, done); err != nil {
		t.Fatal(err)
	}
}
func TestRunForcesCancellationAfterGrace(t *testing.T) {
	server := newTestServer(t, &fakeReadinessChecker{})
	listener := listenForTest(t)
	entered := make(chan struct{})
	exited := make(chan struct{})
	server.httpServer.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(exited)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, listener, 10*time.Millisecond) }()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		client := &http.Client{Timeout: 3 * time.Second}
		res, err := client.Get("http://" + listener.Addr().String())
		if err == nil {
			res.Body.Close()
		}
	}()
	await(t, entered)
	cancel()
	if err := await(t, done); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run = %v, want deadline exceeded", err)
	}
	await(t, exited)
	await(t, requestDone)
}
func TestRunReturnsListenerFailure(t *testing.T) {
	server := newTestServer(t, &fakeReadinessChecker{})
	listener := listenForTest(t)
	_ = listener.Close()
	if err := server.Run(context.Background(), listener, time.Second); err == nil {
		t.Fatal("expected listener failure")
	}
}
