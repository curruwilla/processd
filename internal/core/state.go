// Package core holds the Processd domain: executions, their lifecycle and the
// rules that decide how a lifecycle may advance. It performs no I/O.
package core

import "slices"

// State is the lifecycle state of an execution. See docs/SPEC.md §7.
type State string

// The lifecycle states an execution can be in.
const (
	StateCreated   State = "CREATED"
	StateQueued    State = "QUEUED"
	StateStarting  State = "STARTING"
	StateRunning   State = "RUNNING"
	StateStopping  State = "STOPPING"
	StateCrashed   State = "CRASHED"
	StateRetrying  State = "RETRYING"
	StateCompleted State = "COMPLETED"
	StateFailed    State = "FAILED"
	StateCanceled  State = "CANCELED"
)

// allowedTransitions is the authoritative state machine of docs/SPEC.md §7.1.
// A transition missing from this table is a bug, never a silent no-op.
var allowedTransitions = map[State][]State{
	StateCreated:   {StateQueued, StateStarting, StateCanceled},
	StateQueued:    {StateStarting, StateCanceled, StateFailed},
	StateStarting:  {StateRunning, StateCrashed},
	StateRunning:   {StateCompleted, StateCrashed, StateStopping},
	StateStopping:  {StateCanceled, StateFailed, StateCrashed, StateQueued},
	StateCrashed:   {StateRetrying, StateFailed},
	StateRetrying:  {StateStarting, StateQueued, StateCanceled},
	StateCompleted: {},
	StateFailed:    {},
	StateCanceled:  {},
}

// IsTerminal reports whether the state is final. Terminal states are immutable:
// re-running the same work is a new execution with a new ID.
func (s State) IsTerminal() bool {
	switch s {
	case StateCompleted, StateFailed, StateCanceled:
		return true
	default:
		return false
	}
}

// IsActive reports whether the execution still occupies a concurrency slot.
func (s State) IsActive() bool {
	switch s {
	case StateStarting, StateRunning, StateStopping:
		return true
	default:
		return false
	}
}

// Valid reports whether s is a known state.
func (s State) Valid() bool {
	_, ok := allowedTransitions[s]
	return ok
}

// CanTransition reports whether from -> to is allowed by the state machine.
func CanTransition(from, to State) bool {
	next, ok := allowedTransitions[from]
	if !ok {
		return false
	}

	return slices.Contains(next, to)
}

// States returns every known state, in no particular order. Used to validate
// API filters.
func States() []State {
	all := make([]State, 0, len(allowedTransitions))
	for s := range allowedTransitions {
		all = append(all, s)
	}

	return all
}
