package core

import (
	"errors"
	"fmt"
)

// Sentinel errors for conditions the API and the scheduler branch on.
var (
	ErrNotFound         = errors.New("execution not found")
	ErrWorkerNotFound   = errors.New("worker not found")
	ErrWorkerDisabled   = errors.New("worker is disabled")
	ErrLockHeld         = errors.New("lock is held by another execution")
	ErrQueueFull        = errors.New("queue is full")
	ErrNoCapacity       = errors.New("no free slot for a service")
	ErrShuttingDown     = errors.New("daemon is shutting down")
	ErrUnsupportedType  = errors.New("execution type is not supported")
	ErrRawCommandDenied = errors.New("raw command execution is disabled")
	ErrNotRunning       = errors.New("execution is not running")
	ErrIdempotencyReuse = errors.New("idempotency key reused with a different request")
	// ErrIdempotencyClaimed reports a key another request took first. It never
	// reaches a client: the request that lost the race replays what the winner
	// produced, which is what the key was for.
	ErrIdempotencyClaimed = errors.New("idempotency key is already claimed")
	// ErrIdempotencyInFlight reports a key whose execution is being submitted
	// right now, so there is nothing to replay yet. It is deliberately not a
	// failure: repeating the request with the same key is the way out.
	ErrIdempotencyInFlight = errors.New("idempotency key is in flight")
)

// TransitionError reports an attempt to move an execution along an edge the
// state machine does not define.
type TransitionError struct {
	ID   string
	From State
	To   State
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("invalid transition %s -> %s for %s", e.From, e.To, e.ID)
}

// ParamError reports a request parameter rejected before any process starts.
type ParamError struct {
	Param  string
	Reason string
}

func (e *ParamError) Error() string {
	return fmt.Sprintf("param %q %s", e.Param, e.Reason)
}
