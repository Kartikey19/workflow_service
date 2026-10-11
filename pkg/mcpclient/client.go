// Package mcpclient wraps the official MCP SDK with bounded HTTP responses,
// origin-bound credentials and explicit session ownership. No subprocesses run.
package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/kartikey/workflow_service/pkg/vault"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const MaxBytes = 1 << 20
const MaxTools = 128

type Config struct{ URL, Transport, Token string }

func Validate(cfg Config) error {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return errors.New("MCP url must be an absolute http or https URL without credentials, query or fragment")
	}
	if cfg.Transport != "" && cfg.Transport != "streamable_http" && cfg.Transport != "sse" {
		return errors.New("MCP transport must be streamable_http or sse")
	}
	if len(cfg.Token) > 16384 || strings.ContainsAny(cfg.Token, "\r\n\x00") {
		return errors.New("invalid MCP bearer token")
	}
	return nil
}

type Session struct {
	session *mcp.ClientSession
	token   string
}

// Connect initializes a session. Close must be called, even after a later error.
// The caller must bound ctx; HTTP clients used for SSE must not have a whole-
// response timeout shorter than the caller's context.
func Connect(ctx context.Context, cfg Config, client *http.Client) (*Session, error) {
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	if client == nil {
		return nil, errors.New("MCP requires an explicit HTTP client")
	}
	origin, _ := url.Parse(cfg.URL)
	checked := *client
	base := checked.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	checked.Transport = &guardedTransport{base: base, origin: origin, token: cfg.Token}
	checked.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	var transport mcp.Transport
	if cfg.Transport == "sse" {
		transport = &mcp.SSEClientTransport{Endpoint: cfg.URL, HTTPClient: &checked, MaxEventSize: MaxBytes}
	} else {
		transport = &mcp.StreamableClientTransport{Endpoint: cfg.URL, HTTPClient: &checked, MaxRetries: -1, DisableStandaloneSSE: true, MaxEventSize: MaxBytes}
	}
	clientMCP := mcp.NewClient(&mcp.Implementation{Name: "workflow-service", Version: "1"}, nil)
	session, err := clientMCP.Connect(ctx, transport, nil)
	if err != nil {
		return nil, redact(err, cfg.Token)
	}
	return &Session{session: session, token: cfg.Token}, nil
}
func (s *Session) Close() { _ = s.session.Close() }

type Tool struct {
	Name, Description string
	InputSchema       json.RawMessage
}

func (s *Session) ListTools(ctx context.Context) ([]Tool, error) {
	out := []Tool{}
	cursors := map[string]bool{}
	cursor := ""
	for {
		result, err := s.session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, redact(err, s.token)
		}
		for _, tool := range result.Tools {
			if tool == nil {
				return nil, errors.New("MCP server returned a null tool")
			}
			if len(out) >= MaxTools {
				return nil, errors.New("MCP server offers too many tools (maximum 128)")
			}
			schema, err := json.Marshal(tool.InputSchema)
			if err != nil {
				return nil, errors.New("invalid MCP tool schema")
			}
			if len(schema) > 32<<10 || len(tool.Description) > 8192 {
				return nil, errors.New("MCP tool description or schema is too large")
			}
			if s.token != "" && (strings.Contains(tool.Name+tool.Description, s.token) || strings.Contains(string(schema), string(mustJSON(s.token))[1:len(mustJSON(s.token))-1])) {
				return nil, errors.New("MCP tool metadata contains credentials")
			}
			out = append(out, Tool{Name: tool.Name, Description: tool.Description, InputSchema: schema})
		}
		cursor = result.NextCursor
		if cursor == "" {
			return out, nil
		}
		if cursors[cursor] || len(cursors) >= MaxTools {
			return nil, errors.New("invalid MCP tools pagination")
		}
		cursors[cursor] = true
	}
}
func (s *Session) Call(ctx context.Context, name string, arguments json.RawMessage) (json.RawMessage, bool, error) {
	var object map[string]any
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	if len(arguments) > MaxBytes || json.Unmarshal(arguments, &object) != nil || object == nil {
		return nil, false, errors.New("MCP arguments must be an object of at most 1 MiB")
	}
	if name == "" || len(name) > 128 {
		return nil, false, errors.New("MCP tool name is required and must be at most 128 characters")
	}
	result, err := s.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: object})
	if err != nil {
		return nil, false, redact(err, s.token)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, false, errors.New("encode MCP result")
	}
	if len(raw) > MaxBytes {
		return nil, false, errors.New("MCP result exceeds 1 MiB")
	}
	// A malicious server can echo credentials; never hand them to the model/store.
	if s.token != "" {
		raw = vault.RedactJSON(raw, s.token)
	}
	if len(raw) > MaxBytes {
		return nil, false, errors.New("redacted MCP result exceeds 1 MiB")
	}
	return raw, result.IsError, nil
}
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

type guardedTransport struct {
	base   http.RoundTripper
	origin *url.URL
	token  string
}

func (t *guardedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.EqualFold(req.URL.Scheme, t.origin.Scheme) || !strings.EqualFold(req.URL.Host, t.origin.Host) || req.URL.User != nil {
		return nil, errors.New("MCP endpoint changed origin")
	}
	copyReq := req.Clone(req.Context())
	if t.token != "" {
		copyReq.Header.Set("Authorization", "Bearer "+t.token)
	}
	res, err := t.base.RoundTrip(copyReq)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 400 {
		res.Body.Close()
		return nil, &StatusError{Status: res.StatusCode}
	}
	res.Body = &boundedBody{ReadCloser: res.Body, remaining: MaxBytes}
	return res, nil
}

type boundedBody struct {
	io.ReadCloser
	remaining int
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		var probe [1]byte
		n, err := b.ReadCloser.Read(probe[:])
		if n != 0 {
			return 0, errors.New("MCP response exceeds 1 MiB")
		}
		return 0, err
	}
	if len(p) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= n
	return n, err
}

type redactedError struct {
	cause error
	text  string
}

func (e *redactedError) Error() string { return e.text }
func (e *redactedError) Unwrap() error { return e.cause }
func redact(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	return &redactedError{cause: err, text: strings.ReplaceAll(err.Error(), token, "[redacted]")}
}

// ToolError separates a remote tool's failure from a transport failure.
var ErrTool = errors.New("MCP tool returned an error")

func ToolError() error { return fmt.Errorf("%w; inspect the tool result", ErrTool) }

// StatusError preserves retry classification without retaining an untrusted body.
type StatusError struct{ Status int }

func (e *StatusError) Error() string { return fmt.Sprintf("MCP HTTP status %d", e.Status) }
func IsPermanent(err error) bool {
	var status *StatusError
	if errors.As(err, &status) {
		return status.Status >= 400 && status.Status < 500 && status.Status != 408 && status.Status != 429
	}
	var rpc *jsonrpc.Error
	return errors.As(err, &rpc)
}
