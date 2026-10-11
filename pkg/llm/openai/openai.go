// Package openai is a plain net/http client for the OpenAI Chat Completions
// API. The same wire format is spoken by many other providers (Groq, Together,
// OpenRouter, Ollama, vLLM and others), so one client serves them all through a
// base URL.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/kartikey/workflow_service/pkg/llm"
)

const (
	// DefaultBaseURL is the OpenAI API root, including the version path.
	DefaultBaseURL = "https://api.openai.com/v1"
	maxResponse    = 8 << 20
)

// Client implements llm.Client and llm.ModelLister.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
	// compatible selects max_tokens, which every OpenAI-compatible server
	// accepts, instead of OpenAI's newer max_completion_tokens.
	compatible bool
}

var (
	_ llm.Client      = (*Client)(nil)
	_ llm.ModelLister = (*Client)(nil)
)

// New returns a client for OpenAI itself. An empty baseURL selects DefaultBaseURL.
func New(apiKey, baseURL string, httpClient *http.Client) *Client {
	return newClient(apiKey, baseURL, httpClient, false)
}

// NewCompatible returns a client for an OpenAI-compatible server at baseURL,
// which must include the version path (for example https://api.groq.com/openai/v1).
func NewCompatible(apiKey, baseURL string, httpClient *http.Client) *Client {
	return newClient(apiKey, baseURL, httpClient, true)
}

func newClient(apiKey, baseURL string, httpClient *http.Client, compatible bool) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, http: httpClient, compatible: compatible}
}

type wireRequest struct {
	Model               string        `json:"model"`
	Messages            []wireMessage `json:"messages"`
	Tools               []wireTool    `json:"tools,omitempty"`
	MaxTokens           int           `json:"max_tokens,omitempty"`
	MaxCompletionTokens int           `json:"max_completion_tokens,omitempty"`
	Temperature         *float64      `json:"temperature,omitempty"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type wireResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string      `json:"finish_reason"`
		Message      wireMessage `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

type wireError struct {
	Error struct {
		Type    string `json:"type"`
		Code    any    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func str(s string) *string { return &s }

// Complete sends one chat completion.
func (c *Client) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	body, err := json.Marshal(c.toWire(req))
	if err != nil {
		return llm.Response{}, fmt.Errorf("encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return llm.Response{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.authorize(httpReq)
	data, res, err := c.do(ctx, httpReq)
	if err != nil {
		return llm.Response{}, err
	}
	var wire wireResponse
	if err := json.Unmarshal(data, &wire); err != nil || len(wire.Choices) == 0 {
		return llm.Response{}, &llm.Error{Status: res.StatusCode, Code: "bad_response", Message: "response has no choices"}
	}
	return fromWire(wire), nil
}

func (c *Client) authorize(r *http.Request) {
	if c.apiKey != "" { // a local server such as Ollama needs none
		r.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
}

func (c *Client) do(ctx context.Context, r *http.Request) ([]byte, *http.Response, error) {
	res, err := c.http.Do(r)
	if err != nil {
		return nil, nil, llm.TransportError(ctx, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if err != nil {
		return nil, nil, &llm.Error{Code: "read_error", Message: err.Error(), Retryable: true}
	}
	if len(data) > maxResponse {
		return nil, nil, &llm.Error{Status: res.StatusCode, Code: "response_too_large", Message: "response exceeds the size limit"}
	}
	if res.StatusCode != http.StatusOK {
		return nil, nil, statusError(res, data)
	}
	return data, res, nil
}

func (c *Client) toWire(req llm.Request) wireRequest {
	wire := wireRequest{Model: req.Model, Temperature: req.Temperature}
	if c.compatible {
		wire.MaxTokens = req.MaxTokens
	} else {
		wire.MaxCompletionTokens = req.MaxTokens
	}
	if req.System != "" {
		wire.Messages = append(wire.Messages, wireMessage{Role: "system", Content: str(req.System)})
	}
	for _, message := range req.Messages {
		switch message.Role {
		case llm.RoleAssistant:
			out := wireMessage{Role: "assistant"}
			var text strings.Builder
			for _, block := range message.Content {
				switch block.Type {
				case llm.BlockText:
					text.WriteString(block.Text)
				case llm.BlockToolUse:
					call := wireToolCall{ID: block.ToolUseID, Type: "function"}
					call.Function.Name = block.ToolName
					call.Function.Arguments = string(orEmptyObject(block.ToolInput))
					out.ToolCalls = append(out.ToolCalls, call)
				}
			}
			if text.Len() > 0 {
				out.Content = str(text.String())
			}
			wire.Messages = append(wire.Messages, out)
		default:
			// A user turn is text, tool results, or both. Each tool result is
			// its own "tool" message; remaining text is a user message.
			var text strings.Builder
			for _, block := range message.Content {
				switch block.Type {
				case llm.BlockText:
					text.WriteString(block.Text)
				case llm.BlockToolResult:
					content := block.ToolContent
					if block.IsError {
						content = "Error: " + content
					}
					wire.Messages = append(wire.Messages, wireMessage{Role: "tool", ToolCallID: block.ToolUseID, Content: str(content)})
				}
			}
			if text.Len() > 0 {
				wire.Messages = append(wire.Messages, wireMessage{Role: "user", Content: str(text.String())})
			}
		}
	}
	for _, tool := range req.Tools {
		wt := wireTool{Type: "function"}
		wt.Function.Name, wt.Function.Description = tool.Name, tool.Description
		wt.Function.Parameters = orEmptySchema(tool.InputSchema)
		wire.Tools = append(wire.Tools, wt)
	}
	return wire
}

func orEmptyObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}

func orEmptySchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return raw
}

func fromWire(wire wireResponse) llm.Response {
	choice := wire.Choices[0]
	out := llm.Response{
		Model: wire.Model, Usage: llm.Usage{InputTokens: wire.Usage.PromptTokens, OutputTokens: wire.Usage.CompletionTokens},
	}
	if choice.Message.Content != nil && *choice.Message.Content != "" {
		out.Content = append(out.Content, llm.Block{Type: llm.BlockText, Text: *choice.Message.Content})
	}
	for _, call := range choice.Message.ToolCalls {
		args := json.RawMessage(call.Function.Arguments)
		if !json.Valid(args) {
			args = json.RawMessage(`{}`)
		}
		out.Content = append(out.Content, llm.Block{Type: llm.BlockToolUse, ToolUseID: call.ID, ToolName: call.Function.Name, ToolInput: args})
	}
	switch choice.FinishReason {
	case "tool_calls", "function_call":
		out.StopReason = llm.StopToolUse
	case "length":
		out.StopReason = llm.StopMaxTokens
	case "stop", "":
		out.StopReason = llm.StopEndTurn
		if len(choice.Message.ToolCalls) > 0 {
			out.StopReason = llm.StopToolUse // some servers send "stop" with tool calls
		}
	default:
		out.StopReason = choice.FinishReason
	}
	return out
}

// ListModels returns the model IDs this key can use (GET /models).
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	c.authorize(httpReq)
	data, res, err := c.do(ctx, httpReq)
	if err != nil {
		return nil, err
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, &llm.Error{Status: res.StatusCode, Code: "bad_response", Message: "response is not a model list"}
	}
	models := make([]string, 0, len(list.Data))
	for _, model := range list.Data {
		models = append(models, model.ID)
	}
	return models, nil
}

// statusError classifies a non-200 reply: 429, 408 and 5xx are transient.
func statusError(res *http.Response, data []byte) error {
	var wire wireError
	_ = json.Unmarshal(data, &wire)
	code := wire.Error.Type
	if code == "" {
		code = fmt.Sprint(wire.Error.Code)
		if code == "<nil>" {
			code = ""
		}
	}
	return llm.StatusError(res, code, wire.Error.Message, data)
}
