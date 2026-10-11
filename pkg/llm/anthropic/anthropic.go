// Package anthropic is a plain net/http client for the Anthropic Messages API.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/kartikey/workflow_service/pkg/httpx"
	"github.com/kartikey/workflow_service/pkg/llm"
)

const (
	// DefaultBaseURL is the public API.
	DefaultBaseURL = "https://api.anthropic.com"
	apiVersion     = "2023-06-01"
	maxResponse    = 8 << 20
)

// Client implements llm.Client. The zero value is not usable; use New.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

var _ llm.Client = (*Client)(nil)

// New returns a client. An empty baseURL selects DefaultBaseURL; a nil
// httpClient selects one without its own timeout, so the caller's context
// bounds every call.
func New(apiKey, baseURL string, httpClient *http.Client) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, http: httpClient}
}

type wireRequest struct {
	Model       string        `json:"model"`
	MaxTokens   int           `json:"max_tokens"`
	System      string        `json:"system,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	Messages    []wireMessage `json:"messages"`
	Tools       []wireTool    `json:"tools,omitempty"`
}

type wireMessage struct {
	Role    string      `json:"role"`
	Content []wireBlock `json:"content"`
}

type wireBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type wireResponse struct {
	Model      string      `json:"model"`
	StopReason string      `json:"stop_reason"`
	Content    []wireBlock `json:"content"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type wireError struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Complete sends one Messages API request.
func (c *Client) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	body, err := json.Marshal(toWire(req))
	if err != nil {
		return llm.Response{}, fmt.Errorf("encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return llm.Response{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", apiVersion)

	res, err := c.http.Do(httpReq)
	if err != nil {
		return llm.Response{}, transportError(ctx, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if err != nil {
		return llm.Response{}, &llm.Error{Code: "read_error", Message: err.Error(), Retryable: true}
	}
	if len(data) > maxResponse {
		return llm.Response{}, &llm.Error{Status: res.StatusCode, Code: "response_too_large", Message: "response exceeds the size limit"}
	}
	if res.StatusCode != http.StatusOK {
		return llm.Response{}, statusError(res, data)
	}
	var wire wireResponse
	if err := json.Unmarshal(data, &wire); err != nil {
		return llm.Response{}, &llm.Error{Status: res.StatusCode, Code: "bad_response", Message: "response is not valid JSON"}
	}
	return fromWire(wire), nil
}

func toWire(req llm.Request) wireRequest {
	wire := wireRequest{Model: req.Model, MaxTokens: req.MaxTokens, System: req.System, Temperature: req.Temperature}
	for _, message := range req.Messages {
		out := wireMessage{Role: message.Role}
		for _, block := range message.Content {
			switch block.Type {
			case llm.BlockText:
				out.Content = append(out.Content, wireBlock{Type: "text", Text: block.Text})
			case llm.BlockToolUse:
				input := block.ToolInput
				if len(input) == 0 {
					input = json.RawMessage(`{}`)
				}
				out.Content = append(out.Content, wireBlock{Type: "tool_use", ID: block.ToolUseID, Name: block.ToolName, Input: input})
			case llm.BlockToolResult:
				out.Content = append(out.Content, wireBlock{Type: "tool_result", ToolUseID: block.ToolUseID, Content: block.ToolContent, IsError: block.IsError})
			}
		}
		wire.Messages = append(wire.Messages, out)
	}
	for _, tool := range req.Tools {
		schema := tool.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		wire.Tools = append(wire.Tools, wireTool{Name: tool.Name, Description: tool.Description, InputSchema: schema})
	}
	return wire
}

func fromWire(wire wireResponse) llm.Response {
	out := llm.Response{
		StopReason: wire.StopReason, Model: wire.Model,
		Usage: llm.Usage{InputTokens: wire.Usage.InputTokens, OutputTokens: wire.Usage.OutputTokens},
	}
	for _, block := range wire.Content {
		switch block.Type {
		case "text":
			out.Content = append(out.Content, llm.Block{Type: llm.BlockText, Text: block.Text})
		case "tool_use":
			out.Content = append(out.Content, llm.Block{Type: llm.BlockToolUse, ToolUseID: block.ID, ToolName: block.Name, ToolInput: block.Input})
		}
	}
	return out
}

// statusError classifies a non-200 reply: rate limits (429), server errors
// (5xx) and "overloaded" (529) are transient; other 4xx are not.
func statusError(res *http.Response, data []byte) error {
	var wire wireError
	_ = json.Unmarshal(data, &wire)
	message := wire.Error.Message
	if message == "" {
		message = strings.TrimSpace(string(data[:min(len(data), 200)]))
	}
	return &llm.Error{
		Status: res.StatusCode, Code: wire.Error.Type, Message: message,
		Retryable:  res.StatusCode == http.StatusTooManyRequests || res.StatusCode == http.StatusRequestTimeout || res.StatusCode >= 500,
		RetryAfter: httpx.ParseRetryAfter(res.Header.Get("Retry-After")),
	}
}

func transportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var netErr net.Error
	code := "connection_error"
	if errors.As(err, &netErr) && netErr.Timeout() {
		code = "timeout"
	}
	return &llm.Error{Code: code, Message: err.Error(), Retryable: true}
}
