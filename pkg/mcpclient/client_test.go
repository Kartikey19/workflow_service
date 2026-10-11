package mcpclient

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

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestTransports(t *testing.T) {
	for _, transport := range []string{"streamable_http", "streamable_sse", "sse"} {
		t.Run(transport, func(t *testing.T) {
			server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, &mcp.ServerOptions{PageSize: 1})
			server.AddTool(&mcp.Tool{Name: "echo", Description: "Echo", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(req.Params.Arguments)}}}, nil
			})
			server.AddTool(&mcp.Tool{Name: "fail", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "bad input"}}}, nil
			})
			var handler http.Handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{JSONResponse: transport != "streamable_sse"})
			if transport == "sse" {
				handler = mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return server }, nil)
			}
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer secret-token" {
					w.WriteHeader(401)
					return
				}
				handler.ServeHTTP(w, r)
			}))
			defer httpServer.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client := httpServer.Client()
			session, err := Connect(ctx, Config{URL: httpServer.URL, Transport: func() string {
				if transport == "streamable_sse" {
					return "streamable_http"
				}
				return transport
			}(), Token: "secret-token"}, client)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			tools, err := session.ListTools(ctx)
			if err != nil || len(tools) != 2 {
				t.Fatalf("list = %v %v", tools, err)
			}
			raw, failed, err := session.Call(ctx, "echo", json.RawMessage(`{"text":"secret-token"}`))
			if err != nil || failed || strings.Contains(string(raw), "secret-token") || !strings.Contains(string(raw), "[redacted]") {
				t.Fatalf("call = %s %v %v", raw, failed, err)
			}
			_, failed, err = session.Call(ctx, "fail", nil)
			if err != nil || !failed {
				t.Fatalf("tool failure = %v %v", failed, err)
			}
		})
	}
}
func TestSecurityAndBounds(t *testing.T) {
	for _, target := range []string{"file:///tmp/x", "https://user:password@example.com/mcp", "https://example.com/?token=secret", "https://example.com/#fragment"} {
		if Validate(Config{URL: target}) == nil {
			t.Fatalf("accepted %s", target)
		}
	}
	var hits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; w.WriteHeader(200) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer redirect.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if s, err := Connect(ctx, Config{URL: redirect.URL, Token: "private"}, redirect.Client()); err == nil {
		s.Close()
		t.Fatal("redirect succeeded")
	}
	if hits != 0 {
		t.Fatal("followed a redirect")
	}
	ureq, _ := http.NewRequest("POST", target.URL, nil)
	originReq, _ := http.NewRequest("POST", redirect.URL, nil)
	transport := &guardedTransport{base: http.DefaultTransport, origin: originReq.URL, token: "private"}
	if _, err := transport.RoundTrip(ureq); err == nil {
		t.Fatal("cross-origin endpoint accepted")
	}
	body := &boundedBody{ReadCloser: io.NopCloser(strings.NewReader(strings.Repeat("x", MaxBytes+1))), remaining: MaxBytes}
	if _, err := io.ReadAll(body); err == nil {
		t.Fatal("oversized body accepted")
	}
	blocked := errors.New("blocked private-token")
	if err := redact(blocked, "private-token"); !errors.Is(err, blocked) || strings.Contains(err.Error(), "private-token") {
		t.Fatalf("redaction: %v", err)
	}
}

func TestUnauthorizedAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := Connect(ctx, Config{URL: server.URL}, server.Client())
	if !IsPermanent(err) {
		t.Fatalf("auth should be permanent: %v", err)
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.Copy(io.Discard, r.Body); <-r.Context().Done() }))
	defer slow.Close()
	short, stop := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer stop()
	_, err = Connect(short, Config{URL: slow.URL}, slow.Client())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: %v", err)
	}
}
