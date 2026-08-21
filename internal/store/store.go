// Package store defines how execution state is persisted and read back.
//
// The persisted state is the source of truth: in-memory state is a cache that
// must be rebuilt from here after a daemon restart (docs/SPEC.md §7.2, §13.2).
package store

import (
	"context"
	"time"

	"github.com/curruwilla/processd/internal/core"
)

// Filter narrows a process listing. A zero Filter matches everything.
type Filter struct {
	States        []core.State
	Type          core.Type
	Worker        string
	Lock          string
	CreatedAfter  *time.Time
	CreatedBefore *time.Time
	Limit         int
	Cursor        string
}

// Page is one slice of a listing. NextCursor is empty on the last page.
type Page struct {
	Items      []*core.Process
	NextCursor string
}

// Idempotency records the execution produced for an Idempotency-Key so that a
// client retry never starts the same work twice.
type Idempotency struct {
	Key         string
	RequestHash string
	ProcessID   string
	CreatedAt   time.Time
}

// AuditEntry records who asked for what. It is written for every state-changing
// API call, so that an execution can always be traced back to a token.
type AuditEntry struct {
	At        time.Time
	TokenName string
	Action    string
	ProcessID string
	Detail    string
}

// Store persists executions, locks, idempotency keys and the audit trail.
type Store interface {
	CreateProcess(ctx context.Context, p *core.Process) error
	UpdateProcess(ctx context.Context, p *core.Process) error
	GetProcess(ctx context.Context, id string) (*core.Process, error)
	ListProcesses(ctx context.Context, f Filter) (Page, error)

	// PendingProcesses returns the executions the scheduler may start now:
	// everything queued, plus the retries whose backoff has elapsed. Oldest
	// first, so the queue stays fair.
	PendingProcesses(ctx context.Context, now time.Time) ([]*core.Process, error)

	// UnfinishedProcesses returns every execution that was not in a terminal
	// state, used by the startup reconciliation pass.
	UnfinishedProcesses(ctx context.Context) ([]*core.Process, error)

	// PendingCount reports how many executions are waiting for a slot. It is on
	// the admission path of every submission, so it must stay independent of the
	// size of the retained history.
	PendingCount(ctx context.Context) (int, error)

	// CountByState reports how many executions sit in each state. It scans the
	// whole table, so it belongs on the metrics endpoint and nowhere near a
	// request that runs per submission.
	CountByState(ctx context.Context) (map[core.State]int, error)

	// CountActiveByState reports how many executions sit in each non-terminal
	// state. Restricted to those states, it answers from the state index, so it
	// can back an endpoint a dashboard polls; CountByState cannot.
	CountActiveByState(ctx context.Context) (map[core.State]int, error)

	// CountActiveByTypeAndState splits the same counts by execution type. The
	// console needs the split because a state means different things on each
	// side: a RETRYING task waits for a slot, a RETRYING service holds one.
	CountActiveByTypeAndState(ctx context.Context) (map[core.Type]map[core.State]int, error)

	// CountRestarts sums the restarts of the services that are still live. It is
	// how much the node is flapping now, not a lifetime total the retained
	// history would keep inflating.
	CountRestarts(ctx context.Context) (int, error)

	// CountPendingByWorker reports how many executions each worker has waiting
	// for a slot. Like PendingCount, it is restricted to the two pending states
	// so the answer stays an index search instead of a full scan.
	CountPendingByWorker(ctx context.Context) (map[string]int, error)

	// Ping verifies that the database still answers. It backs the deep health
	// check, which must fail when the store is unusable even though the HTTP
	// server itself is fine.
	Ping(ctx context.Context) error

	// AcquireLock claims key for the execution, or returns core.ErrLockHeld.
	// Re-acquiring a lock the same execution already holds succeeds, so a retry
	// never loses its own lock.
	AcquireLock(ctx context.Context, key, processID string) error
	// ReleaseLock frees key if it is still held by the execution.
	ReleaseLock(ctx context.Context, key, processID string) error
	// ActiveLocks rebuilds the lock table view after a restart.
	ActiveLocks(ctx context.Context) (map[string]string, error)

	SaveIdempotency(ctx context.Context, record Idempotency) error
	// FindIdempotency returns a stored key, or core.ErrNotFound.
	FindIdempotency(ctx context.Context, key string) (Idempotency, error)

	AppendAudit(ctx context.Context, entry AuditEntry) error

	// PurgeHistory deletes terminal executions older than before, then trims the
	// history to maxRows. It returns how many rows were removed.
	PurgeHistory(ctx context.Context, before time.Time, maxRows int) (int, error)

	Close() error
}
