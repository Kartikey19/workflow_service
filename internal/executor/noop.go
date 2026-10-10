package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

type Noop struct{}

var _ Executor = Noop{}

func (Noop) Type() string {
	return "noop"
}

func (Noop) ValidateConfig(raw json.RawMessage) error {
	// Accept an empty JSON object; reject invalid JSON and config fields.
	var cfg map[string]json.RawMessage

	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("noop must be a json object %w", err)
	}

	if cfg == nil {
		return errors.New("noop config must be a json object")
	}

	if len(cfg) > 0 {
		return errors.New("noop config must be empty")
	}

	return nil

}

func (Noop) Describe() Description {
	return Description{
		Title: "No-op", Category: "control",
		Description: "Does nothing and succeeds. Useful as a join point.",
		Example:     "- id: done\n  type: noop\n  needs: [a, b]\n  config: {}",
	}
}

func (Noop) Execute(ctx context.Context, req Request) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	return Result{
		Output: json.RawMessage(`{}`),
	}, nil
}
