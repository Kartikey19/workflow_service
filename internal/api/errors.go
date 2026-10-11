package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/kartikey/workflow_service/internal/services/runs"
	"github.com/kartikey/workflow_service/internal/services/workflows"
	"github.com/kartikey/workflow_service/pkg/auth"
	"github.com/kartikey/workflow_service/pkg/exception"
	"go.uber.org/zap"
)

// writeError maps domain errors to exceptions; anything unknown is a logged 500
// with no internal detail in the body.
func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var ex *exception.Exception
	var compileErr *workflows.CompileError
	var limitErr *workflows.LimitError
	var inputErr *runs.InputError
	var payloadErr *runs.PayloadError
	var maxBytes *http.MaxBytesError
	switch {
	case errors.As(err, &ex):
	case errors.As(err, &compileErr):
		ex = exception.New(http.StatusBadRequest, "spec_invalid", "Workflow specification is invalid", compileErr.Error())
		ex.Errors = errorDiagnostics(compileErr)
	case errors.As(err, &limitErr):
		ex = exception.PaymentRequired(limitErr.Error() + "; upgrade the plan or archive unused workflows")
	case errors.As(err, &inputErr):
		ex = exception.New(http.StatusBadRequest, "input_invalid", "Run input is invalid", inputErr.Error())
		ex.Errors = inputErr.Fields
	case errors.As(err, &payloadErr):
		ex = exception.New(http.StatusUnprocessableEntity, "payload_invalid", "Signal payload does not match the wait schema", payloadErr.Error())
		ex.Errors = payloadErr.Fields
	case errors.As(err, &maxBytes):
		ex = exception.New(http.StatusRequestEntityTooLarge, "too_large", "Request body is too large", fmt.Sprintf("the limit is %d bytes", maxBytes.Limit))
	case errors.Is(err, runs.ErrNotFound):
		ex = exception.NotFound("the requested resource")
	case errors.Is(err, runs.ErrArchived), errors.Is(err, workflows.ErrArchived):
		ex = exception.Conflict("workflow_archived", "the workflow is archived; restore it or publish under a new name")
	case errors.Is(err, runs.ErrIdempotencyConflict):
		ex = exception.Conflict("idempotency_conflict", "this Idempotency-Key was already used with a different request body")
	case errors.Is(err, runs.ErrRunTerminal):
		ex = exception.Conflict("run_terminal", "the run has already finished")
	case errors.Is(err, runs.ErrNoWaiter):
		ex = exception.New(http.StatusNotFound, "no_waiter", "No live waiter for this key",
			"no run is waiting on this key; signals that arrive before the wait are rejected, so retry later")
	case errors.Is(err, auth.ErrForbidden):
		ex = exception.Forbidden("your role does not allow this action")
	case errors.Is(err, context.Canceled):
		return
	default:
		h.logger.Error("request failed", zap.String("route", r.Pattern), zap.Error(err))
		ex = exception.Internal()
	}
	exception.Write(w, r, ex)
}

func errorDiagnostics(err *workflows.CompileError) []any {
	out := make([]any, 0, len(err.Diagnostics))
	for _, d := range err.Diagnostics {
		if d.Severity == "error" {
			out = append(out, d)
		}
	}
	return out
}
