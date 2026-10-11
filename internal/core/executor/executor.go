package executor

import (
	"context"
	"encoding/json"
	"time"
	"uuid"
)

type Executor interface {
	// Type is the `type:` value in YAML. Must be unique in the registry.
	Type() string

	// ValidateConfig is called at PUBLISH time with the raw, unresolved config
	// (expressions still present as literal ${...} strings). It checks shape,
	// required fields, and ranges — never anything requiring I/O.
	ValidateConfig(raw json.RawMessage) error

	// Execute runs one attempt. ctx carries the per-attempt timeout.
	Execute(ctx context.Context, req Request) (Result, error)
}

type Request struct {
	RunID           uuid.UUID
	NodeExecutionID uuid.UUID
	TenantID        uuid.UUID
	NodeID          string
	BranchKey       string
	Attempt         int

	// Config is fully resolved; no ${...} expressions remain.
	Config json.RawMessage

	// IdempotencyKey is STABLE across attempts. Forward it to any downstream
	// system that supports deduplication.
	IdempotencyKey string

	// Deadline mirrors ctx's deadline; useful for setting client timeouts.
	Deadline time.Time

	// Emit records a progress event. Best-effort, non-transactional.
	// Reserved for streaming node output in v2.
	Emit func(ctx context.Context, kind string, payload any)
}

type Result struct {
	Output  json.RawMessage
	Metrics map[string]any // duration_ms is added by the engine; add your own

	// Control, if non-nil, asks the engine to do something other than
	// "mark succeeded and advance". Exactly one field may be set.
	Control *Control
}

type Control struct {
	Goto  string     // branch: enqueue only this successor, skip the other arms
	Sleep *time.Time // sleep: park until this instant
	Wait  *WaitSpec  // wait_event: park until signalled or timed out
}

type WaitSpec struct {
	Key     string
	Expires time.Time
	Schema  json.RawMessage // optional; validates the signal payload
	// Task, when set, makes the wait a human task: the engine records a worklist
	// row in the same transaction that registers the wait (02 §4.7).
	Task *TaskSpec
}

// TaskSpec describes a human task for the worklist read model.
type TaskSpec struct {
	Queue    string
	Form     string
	Assignee string
	Payload  json.RawMessage // shown to whoever works the task
}

// Retryable: transient. The engine will schedule attempt+1 if budget remains.
type RetryableError struct {
	Code       string // "timeout" | "upstream_5xx" | "rate_limited" | ...
	Err        error
	RetryAfter time.Duration // optional; honours an upstream Retry-After header
}

func (e *RetryableError) Error() string {
	if e.Err == nil {
		return e.Code
	}
	if e.Code == "" {
		return e.Err.Error()
	}
	return e.Code + ": " + e.Err.Error()
}

func (e *RetryableError) Unwrap() error {
	return e.Err
}

// Permanent: no retry, regardless of budget. 4xx, validation failures, bad config.
type PermanentError struct {
	Code string // "bad_request" | "not_found" | "invalid_output" | ...
	Err  error
}

func (e *PermanentError) Error() string {
	if e.Err == nil {
		return e.Code
	}
	if e.Code == "" {
		return e.Err.Error()
	}
	return e.Code + ": " + e.Err.Error()
}

func (e *PermanentError) Unwrap() error {
	return e.Err
}

// ChildRequest asks for a child run for a sub-workflow node (02 §4.10).
type ChildRequest struct {
	TenantID    uuid.UUID
	ParentRunID uuid.UUID
	Workflow    string
	Version     int // 0 pins the latest version
	Input       map[string]any
	// IdempotencyKey is the parent node execution's key, stable across attempts,
	// so a retried or redelivered attempt gets the run the first one created.
	IdempotencyKey string
}

// ChildStarter starts child runs. Executors reach the run service only through
// this interface, never the store. Failures that retrying cannot fix come back
// as *PermanentError.
type ChildStarter interface {
	StartChild(ctx context.Context, req ChildRequest) (uuid.UUID, error)
}

// Wait keys with these prefixes are created by the engine for its own node
// types. They cannot be chosen by a wait_event or delivered by POST
// /v1/signals, so a caller cannot complete a task or a sub-workflow wait around
// the checks those paths make.
var ReservedWaitKeyPrefixes = []string{"task:", "subworkflow:"}
