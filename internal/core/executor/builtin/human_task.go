package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kartikey/workflow_service/internal/core/executor"
)

// HumanTask parks the node as a worklist item until a person completes it or
// the SLA passes (02 §4.7). It is a wait_event with a derived key plus a task
// read-model row, so completion is a signal and escalation is the ordinary
// on_timeout path.
type HumanTask struct{}

const defaultTaskSLA = 7 * 24 * time.Hour

func (HumanTask) Type() string { return "human_task" }

func (HumanTask) Describe() executor.Description {
	return executor.Description{
		Title: "Human task", Category: "control",
		Description: "Creates a task in a queue and pauses until a person completes it (POST /v1/tasks/{id}/complete). " +
			"If the SLA passes first, on_timeout routes the run to an escalation node. Output is the completion payload.",
		Example: `- id: nurse_review
  type: human_task
  needs: [create_care_plan]
  on_timeout: escalate_to_physician
  config:
    queue: rn-review
    form: care_plan_v2
    sla: 24h
    payload: {patient: '${input.patient_id}'}`,
		Output: "the completion payload",
	}
}

func (HumanTask) ValidateConfig(raw json.RawMessage) error {
	return executor.CheckFields(raw, []executor.Field{
		{Name: "queue", Kind: "string", Required: true},
		{Name: "form", Kind: "string"},
		{Name: "assignee", Kind: "string"},
		{Name: "sla", Kind: "duration"},
		{Name: "payload", Kind: "any"},
	})
}

func (HumanTask) Execute(ctx context.Context, req executor.Request) (executor.Result, error) {
	var config struct {
		Queue    string          `json:"queue"`
		Form     string          `json:"form"`
		Assignee string          `json:"assignee"`
		SLA      string          `json:"sla"`
		Payload  json.RawMessage `json:"payload"`
	}
	if err := executor.DecodeConfig(req.Config, &config); err != nil {
		return executor.Result{}, err
	}
	if config.Queue == "" {
		return executor.Result{}, executor.Permanent("bad_config", errors.New("queue resolved to an empty string"))
	}
	sla := defaultTaskSLA
	if config.SLA != "" {
		parsed, err := time.ParseDuration(config.SLA)
		if err != nil || parsed <= 0 {
			return executor.Result{}, executor.Permanent("bad_config", errors.New("sla must be positive, such as 24h"))
		}
		sla = parsed
	}
	// Unique per node execution target, so two runs (or two foreach branches)
	// never share a signal key.
	key := fmt.Sprintf("task:%s:%s", req.RunID, req.NodeID)
	if req.BranchKey != "" {
		key += ":" + req.BranchKey
	}
	return executor.Result{Control: &executor.Control{Wait: &executor.WaitSpec{
		Key: key, Expires: time.Now().Add(sla),
		Task: &executor.TaskSpec{Queue: config.Queue, Form: config.Form, Assignee: config.Assignee, Payload: config.Payload},
	}}}, nil
}
