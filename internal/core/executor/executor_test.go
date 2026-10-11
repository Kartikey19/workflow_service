package executor

import (
	"errors"
	"fmt"
	"testing"
)

func TestExecutionErrors(t *testing.T) {
	cause := errors.New("execution failed")
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"retryable", &RetryableError{Code: "timeout", Err: cause}, "timeout: execution failed"},
		{"permanent", &PermanentError{Code: "bad_config", Err: cause}, "bad_config: execution failed"},
		{"retryable without cause", &RetryableError{Code: "timeout"}, "timeout"},
		{"permanent without cause", &PermanentError{Code: "bad_config"}, "bad_config"},
		{"retryable without code", &RetryableError{Err: cause}, "execution failed"},
		{"permanent without code", &PermanentError{Err: cause}, "execution failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Fatalf("Error() = %q, want %q", got, tt.want)
			}
		})
	}

	retryable := &RetryableError{Code: "timeout", Err: cause}
	permanent := &PermanentError{Code: "bad_config", Err: cause}
	for _, err := range []error{retryable, permanent} {
		if !errors.Is(fmt.Errorf("execute node: %w", err), cause) {
			t.Fatal("wrapped execution error lost its cause")
		}
	}
	var retryTarget *RetryableError
	if !errors.As(fmt.Errorf("execute node: %w", retryable), &retryTarget) || retryTarget != retryable {
		t.Fatal("wrapped retryable error lost its classification")
	}
	var permanentTarget *PermanentError
	if !errors.As(fmt.Errorf("execute node: %w", permanent), &permanentTarget) || permanentTarget != permanent {
		t.Fatal("wrapped permanent error lost its classification")
	}
}
