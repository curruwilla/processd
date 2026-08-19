package core

import (
	"errors"
	"testing"
)

func TestState_IsTerminal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state State
		want  bool
	}{
		{name: "completed is terminal", state: StateCompleted, want: true},
		{name: "failed is terminal", state: StateFailed, want: true},
		{name: "canceled is terminal", state: StateCanceled, want: true},
		{name: "crashed is transitional", state: StateCrashed, want: false},
		{name: "retrying is transitional", state: StateRetrying, want: false},
		{name: "running is transitional", state: StateRunning, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.state.IsTerminal(); got != tt.want {
				t.Errorf("%s.IsTerminal() = %t, want %t", tt.state, got, tt.want)
			}
		})
	}
}

func TestState_IsActive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state State
		want  bool
	}{
		{name: "starting holds a slot", state: StateStarting, want: true},
		{name: "running holds a slot", state: StateRunning, want: true},
		{name: "stopping holds a slot", state: StateStopping, want: true},
		{name: "queued holds no slot", state: StateQueued, want: false},
		{name: "retrying holds no slot", state: StateRetrying, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.state.IsActive(); got != tt.want {
				t.Errorf("%s.IsActive() = %t, want %t", tt.state, got, tt.want)
			}
		})
	}
}

func TestCanTransition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		from State
		to   State
		want bool
	}{
		{name: "created to queued", from: StateCreated, to: StateQueued, want: true},
		{name: "queued to starting", from: StateQueued, to: StateStarting, want: true},
		{name: "starting to running", from: StateStarting, to: StateRunning, want: true},
		{name: "running to completed", from: StateRunning, to: StateCompleted, want: true},
		{name: "crashed to retrying", from: StateCrashed, to: StateRetrying, want: true},
		{name: "retrying back to queued", from: StateRetrying, to: StateQueued, want: true},
		{name: "queued expires into failed", from: StateQueued, to: StateFailed, want: true},

		{name: "running cannot jump to starting", from: StateRunning, to: StateStarting, want: false},
		{name: "completed is immutable", from: StateCompleted, to: StateRunning, want: false},
		{name: "canceled is immutable", from: StateCanceled, to: StateRetrying, want: false},
		{name: "queued cannot complete without running", from: StateQueued, to: StateCompleted, want: false},
		{name: "unknown state has no edges", from: State("BOGUS"), to: StateRunning, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := CanTransition(tt.from, tt.to); got != tt.want {
				t.Errorf("CanTransition(%s, %s) = %t, want %t", tt.from, tt.to, got, tt.want)
			}
		})
	}
}

func TestProcess_TransitionTo(t *testing.T) {
	t.Parallel()

	t.Run("applies an allowed edge and records the reason", func(t *testing.T) {
		t.Parallel()

		p := &Process{ID: "proc_1", State: StateRunning}

		if err := p.TransitionTo(StateStopping, ReasonUserRequest); err != nil {
			t.Fatalf("TransitionTo() returned %v, want nil", err)
		}

		if p.State != StateStopping {
			t.Errorf("state = %s, want %s", p.State, StateStopping)
		}

		if p.Reason != ReasonUserRequest {
			t.Errorf("reason = %s, want %s", p.Reason, ReasonUserRequest)
		}
	})

	t.Run("rejects an edge outside the state machine", func(t *testing.T) {
		t.Parallel()

		p := &Process{ID: "proc_1", State: StateCompleted}

		err := p.TransitionTo(StateRunning, "")
		if err == nil {
			t.Fatal("TransitionTo() returned nil, want an error")
		}

		var transitionErr *TransitionError
		if !errors.As(err, &transitionErr) {
			t.Fatalf("error is %T, want *TransitionError", err)
		}

		if p.State != StateCompleted {
			t.Errorf("state = %s, want it unchanged as %s", p.State, StateCompleted)
		}
	})

	t.Run("keeps the previous reason when none is given", func(t *testing.T) {
		t.Parallel()

		p := &Process{ID: "proc_1", State: StateCrashed, Reason: ReasonStartError}

		if err := p.TransitionTo(StateRetrying, ""); err != nil {
			t.Fatalf("TransitionTo() returned %v, want nil", err)
		}

		if p.Reason != ReasonStartError {
			t.Errorf("reason = %s, want it preserved as %s", p.Reason, ReasonStartError)
		}
	})
}
