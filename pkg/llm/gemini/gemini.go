// Package gemini is a plain net/http client for the Google Gemini
// generateContent API. Gemini function calls carry no IDs, so the client mints
// stable ones ("call_<n>") and maps tool results back to function names.
package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/kartikey/workflow_service/pkg/httpx"
	"github.com/kartikey/workflow_service/pkg/llm"
)

const (
	// DefaultBaseURL is the Generative Language API root.
	DefaultBaseURL = "https://generativelanguage.googleapis.com"
	maxResponse    = 8 << 20
)

// Client implements llm.Client and llm.ModelLister.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

var (
	_ llm.Client      = (*Client)(nil)
	_ llm.ModelLister = (*Client)(nil)
)

// New returns a client. An empty baseURL selects DefaultBaseURL.
func New(apiKey, baseURL string, httpClient *http.Client) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, http: httpClient}
}

type part struct {
	Text             string        `json:"text,omitempty"`
	FunctionCall     *functionCall `json:"functionCall,omitempty"`
	FunctionResponse *functionResp `json:"functionResponse,omitempty"`
}

type functionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type functionResp struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type content struct {
	Role  string `json:"role,omitempty"`
	Parts []part `json:"parts"`
}

type wireRequest struct {
	Contents          []content `json:"contents"`
	SystemInstruction *content  `json:"systemInstruction,omitempty"`
	Tools             []struct {
		FunctionDeclarations []declaration `json:"functionDeclarations"`
	} `json:"tools,omitempty"`
	GenerationConfig struct {
		MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
		Temperature     *float64 `json:"temperature,omitempty"`
	} `json:"generationConfig"`
}

type declaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type wireResponse struct {
	Candidates []struct {
		Content      content `json:"content"`
		FinishReason string  `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
	} `json:"usageMetadata"`
	ModelVersion string `json:"modelVersion"`
}

type wireError struct {
	Error struct {
		Code    int    `json:"code"`
		Status  string `json:"status"`
		Message string `json:"message"`
	} `json:"error"`
}

// Complete sends one generateContent request.
func (c *Client) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	wire, err := toWire(req)
	if err != nil {
		return llm.Response{}, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return llm.Response{}, fmt.Errorf("encode request: %w", err)
	}
	endpoint := fmt.Sprintf("%s/v1beta/models/%s:generateContent", c.baseURL, url.PathEscape(strings.TrimPrefix(req.Model, "models/")))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return llm.Response{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", c.apiKey)
	data, res, err := c.do(ctx, httpReq)
	if err != nil {
		return llm.Response{}, err
	}
	var decoded wireResponse
	if err := json.Unmarshal(data, &decoded); err != nil || len(decoded.Candidates) == 0 {
		return llm.Response{}, &llm.Error{Status: res.StatusCode, Code: "bad_response", Message: "response has no candidates (the prompt may have been blocked)"}
	}
	return fromWire(decoded, req.Model), nil
}

func (c *Client) do(ctx context.Context, r *http.Request) ([]byte, *http.Response, error) {
	res, err := c.http.Do(r)
	if err != nil {
		return nil, nil, transportError(ctx, err)
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

// toWire maps the conversation. Gemini's roles are "user" and "model"; tool
// results are functionResponse parts that name the function, found through the
// tool_use block that carried the same ID.
func toWire(req llm.Request) (wireRequest, error) {
	var wire wireRequest
	wire.GenerationConfig.MaxOutputTokens = req.MaxTokens
	wire.GenerationConfig.Temperature = req.Temperature
	if req.System != "" {
		wire.SystemInstruction = &content{Parts: []part{{Text: req.System}}}
	}
	names := map[string]string{}
	for _, message := range req.Messages {
		for _, block := range message.Content {
			if block.Type == llm.BlockToolUse {
				names[block.ToolUseID] = block.ToolName
			}
		}
	}
	for _, message := range req.Messages {
		out := content{Role: "user"}
		if message.Role == llm.RoleAssistant {
			out.Role = "model"
		}
		for _, block := range message.Content {
			switch block.Type {
			case llm.BlockText:
				out.Parts = append(out.Parts, part{Text: block.Text})
			case llm.BlockToolUse:
				args := block.ToolInput
				if len(args) == 0 {
					args = json.RawMessage(`{}`)
				}
				out.Parts = append(out.Parts, part{FunctionCall: &functionCall{Name: block.ToolName, Args: args}})
			case llm.BlockToolResult:
				name, ok := names[block.ToolUseID]
				if !ok {
					return wireRequest{}, fmt.Errorf("tool result %q has no matching tool call", block.ToolUseID)
				}
				result := map[string]any{"result": block.ToolContent}
				if block.IsError {
					result = map[string]any{"error": block.ToolContent}
				}
				encoded, _ := json.Marshal(result)
				out.Parts = append(out.Parts, part{FunctionResponse: &functionResp{Name: name, Response: encoded}})
			}
		}
		if len(out.Parts) > 0 {
			wire.Contents = append(wire.Contents, out)
		}
	}
	if len(req.Tools) > 0 {
		declarations := make([]declaration, 0, len(req.Tools))
		for _, tool := range req.Tools {
			declarations = append(declarations, declaration{Name: tool.Name, Description: tool.Description, Parameters: sanitizeSchema(tool.InputSchema)})
		}
		wire.Tools = append(wire.Tools, struct {
			FunctionDeclarations []declaration `json:"functionDeclarations"`
		}{declarations})
	}
	return wire, nil
}

// sanitizeSchema drops the JSON Schema keywords Gemini's OpenAPI subset rejects
// and omits an empty object schema (Gemini refuses object schemas without
// properties).
func sanitizeSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var schema map[string]any
	if json.Unmarshal(raw, &schema) != nil {
		return nil
	}
	cleaned := clean(schema).(map[string]any)
	if props, _ := cleaned["properties"].(map[string]any); cleaned["type"] == "object" && len(props) == 0 {
		return nil
	}
	encoded, _ := json.Marshal(cleaned)
	return encoded
}

var unsupported = map[string]bool{"additionalProperties": true, "$schema": true, "default": true, "pattern": true, "minLength": true, "maxLength": true, "const": true}

func clean(v any) any {
	switch value := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for k, child := range value {
			if unsupported[k] {
				continue
			}
			out[k] = clean(child)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, child := range value {
			out[i] = clean(child)
		}
		return out
	default:
		return v
	}
}

func fromWire(wire wireResponse, requested string) llm.Response {
	candidate := wire.Candidates[0]
	model := wire.ModelVersion
	if model == "" {
		model = requested
	}
	out := llm.Response{Model: model, Usage: llm.Usage{
		InputTokens: wire.UsageMetadata.PromptTokenCount, OutputTokens: wire.UsageMetadata.CandidatesTokenCount,
	}}
	calls := 0
	for _, p := range candidate.Content.Parts {
		switch {
		case p.FunctionCall != nil:
			calls++
			args := p.FunctionCall.Args
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			out.Content = append(out.Content, llm.Block{
				Type: llm.BlockToolUse, ToolUseID: fmt.Sprintf("call_%d", calls), ToolName: p.FunctionCall.Name, ToolInput: args,
			})
		case p.Text != "":
			out.Content = append(out.Content, llm.Block{Type: llm.BlockText, Text: p.Text})
		}
	}
	switch {
	case calls > 0:
		out.StopReason = llm.StopToolUse
	case candidate.FinishReason == "MAX_TOKENS":
		out.StopReason = llm.StopMaxTokens
	default:
		out.StopReason = llm.StopEndTurn
	}
	return out
}

// ListModels returns the models that can generate content (GET /v1beta/models).
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1beta/models?pageSize=200", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("x-goog-api-key", c.apiKey)
	data, res, err := c.do(ctx, httpReq)
	if err != nil {
		return nil, err
	}
	var list struct {
		Models []struct {
			Name    string   `json:"name"`
			Methods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, &llm.Error{Status: res.StatusCode, Code: "bad_response", Message: "response is not a model list"}
	}
	models := make([]string, 0, len(list.Models))
	for _, model := range list.Models {
		if len(model.Methods) > 0 && !contains(model.Methods, "generateContent") {
			continue
		}
		models = append(models, strings.TrimPrefix(model.Name, "models/"))
	}
	return models, nil
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func statusError(res *http.Response, data []byte) error {
	var wire wireError
	_ = json.Unmarshal(data, &wire)
	message := wire.Error.Message
	if message == "" {
		message = strings.TrimSpace(string(data[:min(len(data), 200)]))
	}
	return &llm.Error{
		Status: res.StatusCode, Code: wire.Error.Status, Message: message,
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
