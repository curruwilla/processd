// Package api exposes the Processd REST API.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/curruwilla/processd/internal/core"
)

// errorBody is the single error shape of the API (docs/SPEC.md §6.2).
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// apiError carries the HTTP status and machine-readable code of a failure.
type apiError struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
}

func (e *apiError) Error() string { return e.Message }

func badRequest(code, message string) *apiError {
	return &apiError{Status: http.StatusBadRequest, Code: code, Message: message}
}

// sentinelStatus places each domain sentinel in the HTTP contract. It is a
// table rather than a switch so that adding a failure mode is one line and the
// whole contract stays readable at a glance (docs/SPEC.md §6.2).
var sentinelStatus = []struct {
	err    error
	status int
	code   string
}{
	{core.ErrNotFound, http.StatusNotFound, "not_found"},
	{core.ErrWorkerNotFound, http.StatusNotFound, "worker_not_found"},
	{core.ErrWorkerDisabled, http.StatusUnprocessableEntity, "worker_disabled"},
	{core.ErrLockHeld, http.StatusConflict, "lock_held"},
	{core.ErrIdempotencyReuse, http.StatusConflict, "idempotency_reuse"},
	{core.ErrNotRunning, http.StatusConflict, "not_running"},
	{core.ErrQueueFull, http.StatusTooManyRequests, "queue_full"},
	{core.ErrShuttingDown, http.StatusServiceUnavailable, "shutting_down"},
	{core.ErrNoCapacity, http.StatusServiceUnavailable, "no_capacity"},
	{core.ErrUnsupportedType, http.StatusBadRequest, "unsupported_type"},
	{core.ErrRawCommandDenied, http.StatusForbidden, "raw_command_denied"},
}

// statusFor maps domain errors to the HTTP contract. Anything unmapped is a
// server error: an unexpected error must never be reported as a client mistake.
func statusFor(err error) *apiError {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		return apiErr
	}

	var paramErr *core.ParamError
	if errors.As(err, &paramErr) {
		return &apiError{
			Status:  http.StatusBadRequest,
			Code:    "param_invalid",
			Message: paramErr.Error(),
			Details: map[string]any{"param": paramErr.Param},
		}
	}

	for _, mapped := range sentinelStatus {
		if errors.Is(err, mapped.err) {
			return &apiError{Status: mapped.status, Code: mapped.code, Message: err.Error()}
		}
	}

	return &apiError{
		Status:  http.StatusInternalServerError,
		Code:    "internal_error",
		Message: "internal error",
	}
}

// writeJSON sends a JSON body with the given status.
func writeJSON(w http.ResponseWriter, log *slog.Logger, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if body == nil {
		return
	}

	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Error("writing response body", slog.Any("error", err))
	}
}

// writeError renders err using the API error contract. The original error is
// logged here and not returned to the caller, so technical details never leak
// to clients and are never logged twice.
func writeError(w http.ResponseWriter, log *slog.Logger, err error) {
	rendered := statusFor(err)

	if rendered.Status >= http.StatusInternalServerError {
		log.Error("request failed", slog.Any("error", err))
	}

	writeJSON(w, log, rendered.Status, errorBody{Error: errorDetail{
		Code:    rendered.Code,
		Message: rendered.Message,
		Details: rendered.Details,
	}})
}
