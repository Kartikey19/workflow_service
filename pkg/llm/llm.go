// Package llm defines a small provider-neutral interface for chat models with
// tool calling. Concrete clients (anthropic, later openai, gemini) live in
// subpackages and use plain net/http, with no vendor SDKs.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Roles of a Message.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Block kinds of Content.
const (
	BlockText       = "text"
	BlockToolUse    = "tool_use"
	BlockToolResult = "tool_result"
)

// Stop reasons a Response may carry; other values pass through unchanged.
const (
	StopEndTurn   = "end_turn"
	StopToolUse   = "tool_use"
	StopMaxTokens = "max_tokens"
)

// Block is one piece of message content.
type Block struct {
	Type string

	Text string // BlockText

	ToolUseID string          // BlockToolUse and BlockToolResult
	ToolName  string          // BlockToolUse
	ToolInput json.RawMessage // BlockToolUse

	ToolContent string // BlockToolResult
	IsError     bool   // BlockToolResult
}

// Message is one turn of the conversation.
type Message struct {
	Role    string
	Content []Block
}

// Tool is a function the model may call. InputSchema is a JSON Schema object.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// Request is one model call.
type Request struct {
	Model       string
	System      string
	Messages    []Message
	Tools       []Tool
	MaxTokens   int
	Temperature *float64
}

// Usage counts tokens billed for one call.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Response is the model's reply.
type Response struct {
	Content    []Block
	StopReason string
	Usage      Usage
	Model      string
}

// Text joins the text blocks of the reply.
func (r Response) Text() string {
	var out string
	for _, block := range r.Content {
		if block.Type == BlockText {
			out += block.Text
		}
	}
	return out
}

// ToolUses returns the tool calls the model asked for, in order.
func (r Response) ToolUses() []Block {
	var out []Block
	for _, block := range r.Content {
		if block.Type == BlockToolUse {
			out = append(out, block)
		}
	}
	return out
}

// Client calls one provider.
type Client interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// Error is a failed provider call. Retryable errors are transient (rate limits,
// overload, 5xx); the rest will fail the same way if repeated.
type Error struct {
	Status     int
	Code       string // provider error type, e.g. "rate_limit_error"
	Message    string
	Retryable  bool
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	return fmt.Sprintf("provider returned HTTP %d (%s): %s", e.Status, e.Code, e.Message)
}
