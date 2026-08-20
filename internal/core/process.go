package core

import (
	"maps"
	"slices"
	"time"
)

// Type distinguishes one-shot work from long-running work. Their retry, queue
// and terminal-state semantics are opposites, so the distinction is explicit in
// the model even though the MVP only accepts TypeTask. See docs/SPEC.md §4.
type Type string

const (
	// TypeTask is a one-shot execution: it terminates, and success is final.
	TypeTask Type = "task"
	// TypeService is a long-running execution where any exit is abnormal.
	// Reserved: accepted by the model, rejected by the API until implemented.
	TypeService Type = "service"
)

// Reason qualifies a state that would otherwise be ambiguous.
type Reason string

// Reasons the daemon records alongside a state.
const (
	ReasonUserRequest   Reason = "user_request"
	ReasonTimeout       Reason = "timeout"
	ReasonMaxAttempts   Reason = "max_attempts"
	ReasonQueueTimeout  Reason = "queue_timeout"
	ReasonShutdown      Reason = "shutdown"
	ReasonDaemonRestart Reason = "daemon_restart"
	ReasonStartError    Reason = "start_error"
	ReasonNoRetryExit   Reason = "no_retry_exit_code"
	ReasonLockConflict  Reason = "lock_conflict"
	ReasonOrphaned      Reason = "orphaned"
)

// Process is one execution, identified by a stable logical ID that survives
// retries and daemon restarts. The PID is auxiliary data (see docs/SPEC.md §8).
type Process struct {
	ID       string
	Worker   string
	Type     Type
	State    State
	Reason   Reason
	Attempt  int
	Lock     string
	Metadata map[string]string

	// Effective definition, resolved at creation time and frozen afterwards so
	// that reloading workers.d never mutates a running execution.
	Command     string
	Args        []string
	Env         map[string]string
	Cwd         string
	User        string
	Group       string
	Timeout     time.Duration
	MaxAttempts int

	// Runtime facts of the current attempt.
	PID          int
	PIDStartTime uint64
	ExitCode     *int
	Signal       string
	LogTruncated bool

	CreatedAt  time.Time
	QueuedAt   *time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
	RetryAt    *time.Time
}

// Duration returns how long the current attempt ran, or 0 when it has not
// finished yet.
func (p *Process) Duration() time.Duration {
	if p.StartedAt == nil || p.FinishedAt == nil {
		return 0
	}

	return p.FinishedAt.Sub(*p.StartedAt)
}

// TransitionTo validates the edge against the state machine and applies it.
// The caller is responsible for persisting the process afterwards: in-memory
// state is a cache, the store is the source of truth.
func (p *Process) TransitionTo(to State, reason Reason) error {
	if !CanTransition(p.State, to) {
		return &TransitionError{ID: p.ID, From: p.State, To: to}
	}

	p.State = to
	if reason != "" {
		p.Reason = reason
	}

	return nil
}

// ClearAttempt resets the per-attempt runtime facts before a retry starts.
func (p *Process) ClearAttempt() {
	p.PID = 0
	p.PIDStartTime = 0
	p.ExitCode = nil
	p.Signal = ""
	p.LogTruncated = false
	p.StartedAt = nil
	p.FinishedAt = nil
}

// Clone returns a deep copy of the execution.
//
// The scheduler hands an execution to the supervisor and then answers the
// request that created it; both would otherwise read and write the same struct
// from different goroutines. Ownership is therefore transferred by value: the
// supervisor mutates its own copy, and the store stays the shared truth.
func (p *Process) Clone() *Process {
	if p == nil {
		return nil
	}

	clone := *p
	clone.Args = slices.Clone(p.Args)
	clone.Env = maps.Clone(p.Env)
	clone.Metadata = maps.Clone(p.Metadata)
	clone.ExitCode = clonePointer(p.ExitCode)
	clone.QueuedAt = clonePointer(p.QueuedAt)
	clone.StartedAt = clonePointer(p.StartedAt)
	clone.FinishedAt = clonePointer(p.FinishedAt)
	clone.RetryAt = clonePointer(p.RetryAt)

	return &clone
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}

	copied := *value

	return &copied
}
