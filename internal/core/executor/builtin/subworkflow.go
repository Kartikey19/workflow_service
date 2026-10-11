package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kartikey/workflow_service/internal/core/executor"
)

// SubWorkflow runs another published workflow of the same organization as a
// child run and waits for it (02 §4.10). It starts the child through a
// ChildStarter, then parks on a wait keyed by the child's run ID; the engine
// completes that wait when the child ends. It needs a starter, so it is
// registered by the application rather than listed in All.
type SubWorkflow struct {
	Starter executor.ChildStarter
}

const defaultSubWorkflowTimeout = 720 * time.Hour

func (SubWorkflow) Type() string { return "workflow" }

func (SubWorkflow) Describe() executor.Description {
	return executor.Description{
		Title: "Sub-workflow", Category: "control",
		Description: "Runs another workflow of this organization as a child run and waits for it to finish. " +
			"The node fails if the child fails or is cancelled; cancelling this run cancels the child.",
		Example: `- id: onboard
  type: workflow
  needs: [create_account]
  config:
    name: onboard-customer
    input: {customer: '${nodes.create_account.output.body.id}'}
    timeout: 2h`,
		Output: `{"run_id": "<child run>", "status": "succeeded", "output": <the child's outputs>}`,
	}
}

func (SubWorkflow) ValidateConfig(raw json.RawMessage) error {
	return executor.CheckFields(raw, []executor.Field{
		{Name: "name", Kind: "string", Required: true},
		{Name: "version", Kind: "integer"},
		{Name: "input", Kind: "object"},
		{Name: "timeout", Kind: "duration"},
	})
}

func (s SubWorkflow) Execute(ctx context.Context, req executor.Request) (executor.Result, error) {
	var config struct {
		Name    string         `json:"name"`
		Version int            `json:"version"`
		Input   map[string]any `json:"input"`
		Timeout string         `json:"timeout"`
	}
	if err := executor.DecodeConfig(req.Config, &config); err != nil {
		return executor.Result{}, err
	}
	if config.Name == "" {
		return executor.Result{}, executor.Permanent("bad_config", errors.New("name resolved to an empty string"))
	}
	if config.Version < 0 {
		return executor.Result{}, executor.Permanent("bad_config", errors.New("version must be positive"))
	}
	timeout := defaultSubWorkflowTimeout
	if config.Timeout != "" {
		parsed, err := time.ParseDuration(config.Timeout)
		if err != nil || parsed <= 0 {
			return executor.Result{}, executor.Permanent("bad_config", errors.New("timeout must be positive, such as 2h"))
		}
		timeout = parsed
	}
	if s.Starter == nil {
		return executor.Result{}, executor.Permanent("bad_config", errors.New("sub-workflows are not available on this worker"))
	}
	child, err := s.Starter.StartChild(ctx, executor.ChildRequest{
		TenantID: req.TenantID, ParentRunID: req.RunID, Workflow: config.Name, Version: config.Version,
		Input: config.Input, IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		return executor.Result{}, err
	}
	metrics := map[string]any{"child_run_id": child.String(), "child_workflow": config.Name}
	return executor.Result{
		Metrics: metrics,
		Control: &executor.Control{Wait: &executor.WaitSpec{
			Key: fmt.Sprintf("subworkflow:%s", child), Expires: time.Now().Add(timeout),
		}},
	}, nil
}
