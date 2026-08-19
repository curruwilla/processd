package api

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/curruwilla/processd/internal/core"
)

func TestStatusFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "not found", err: core.ErrNotFound, wantStatus: http.StatusNotFound, wantCode: "not_found"},
		{
			name:       "wrapped worker not found",
			err:        fmt.Errorf("invoice: %w", core.ErrWorkerNotFound),
			wantStatus: http.StatusNotFound,
			wantCode:   "worker_not_found",
		},
		{name: "lock held", err: core.ErrLockHeld, wantStatus: http.StatusConflict, wantCode: "lock_held"},
		{name: "queue full", err: core.ErrQueueFull, wantStatus: http.StatusTooManyRequests, wantCode: "queue_full"},
		{
			name:       "shutting down",
			err:        core.ErrShuttingDown,
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "shutting_down",
		},
		{
			name:       "raw command denied",
			err:        core.ErrRawCommandDenied,
			wantStatus: http.StatusForbidden,
			wantCode:   "raw_command_denied",
		},
		{
			name:       "worker disabled",
			err:        core.ErrWorkerDisabled,
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "worker_disabled",
		},
		{
			name:       "param error carries the param name",
			err:        &core.ParamError{Param: "id", Reason: "is required"},
			wantStatus: http.StatusBadRequest,
			wantCode:   "param_invalid",
		},
		{
			name:       "unmapped errors are server errors",
			err:        errors.New("disk on fire"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal_error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := statusFor(tt.err)

			if got.Status != tt.wantStatus {
				t.Errorf("status = %d, want %d", got.Status, tt.wantStatus)
			}

			if got.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", got.Code, tt.wantCode)
			}
		})
	}
}

func TestStatusFor_hidesInternalDetail(t *testing.T) {
	t.Parallel()

	rendered := statusFor(errors.New("dial tcp 10.0.0.5:5432: connection refused"))

	if rendered.Message != "internal error" {
		t.Errorf("message = %q, want the technical detail to stay out of the response", rendered.Message)
	}
}
