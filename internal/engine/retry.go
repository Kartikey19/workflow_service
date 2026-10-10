package engine

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/kartikey/workflow_service/internal/executor"
	"github.com/kartikey/workflow_service/internal/spec"
	"github.com/kartikey/workflow_service/internal/store/gen"
)

// classify maps an executor error to a stable code and retry eligibility (03 §6.3).
// Unknown errors are retryable: an unclassified failure is more often a flaky
// dependency than a bug, and max_attempts still bounds it.
func classify(err error) (code string, retryable bool, retryAfter time.Duration) {
	var re *executor.RetryableError
	var pe *executor.PermanentError
	switch {
	case errors.As(err, &re):
		return re.Code, true, re.RetryAfter
	case errors.As(err, &pe):
		return pe.Code, false, 0
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", true, 0
	case errors.Is(err, context.Canceled):
		return "cancelled", false, 0
	default:
		return "unknown", true, 0
	}
}

// failedOutcome writes the failed attempt and, when the error is retryable and
// budget remains, schedules attempt+1 (03 §6.1).
func failedOutcome(ne gen.NodeExecution, node spec.NodeSpec, err error, metrics []byte) outcome {
	code, retryable, retryAfter := classify(err)
	o := outcome{status: "failed", code: code, err: errorJSON(code, err, retryable), metrics: metrics}
	if retryable && int(ne.Attempt) < node.Retry.MaxAttempts && retryAllowed(node.Retry, code) {
		// An upstream Retry-After wins over a shorter computed backoff.
		o.retryAt = time.Now().Add(max(backoff(node.Retry, int(ne.Attempt)), retryAfter))
	}
	return o
}

// retryAllowed applies retry_on; an empty list allows every retryable code.
func retryAllowed(policy spec.RetrySpec, code string) bool {
	return len(policy.RetryOn) == 0 || slices.Contains(policy.RetryOn, code)
}

// backoff returns the delay before the attempt after `attempt` (03 §6.2).
func backoff(policy spec.RetrySpec, attempt int) time.Duration {
	initial := time.Duration(policy.InitialNS)
	maximum := time.Duration(policy.MaxNS)
	delay := initial
	switch policy.Backoff {
	case "fixed":
	case "linear":
		delay = initial * time.Duration(attempt)
	default: // exponential; doubling stops at the cap so it cannot overflow
		for i := 1; i < attempt && (maximum <= 0 || delay < maximum); i++ {
			delay *= 2
		}
	}
	if maximum > 0 {
		delay = min(delay, maximum)
	}
	// Jitter spreads retries of many runs against the same failing upstream.
	if policy.Jitter > 0 {
		delay = time.Duration(float64(delay) * (1 - policy.Jitter + rand.Float64()*2*policy.Jitter))
	}
	return delay
}

// errorJSON is the node_executions.error shape: {code, message, retryable}.
func errorJSON(code string, err error, retryable bool) []byte {
	encoded, _ := json.Marshal(map[string]any{
		"code": code, "message": err.Error(), "retryable": retryable,
	})
	return encoded
}
