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

// Idempotency records the response produced for an Idempotency-Key so that a
// client retry never starts the same work twice.
type Idempotency struct {
	Key         string
	RequestHash string
	ProcessID   string
	CreatedAt   time.Time
}

// Store persists executions, locks and idempotency keys.
type Store interface {
	CreateProcess(ctx context.Context, p *core.Process) error
	UpdateProcess(ctx context.Context, p *core.Process) error
	GetProcess(ctx context.Context, id string) (*core.Process, error)
	ListProcesses(ctx context.Context, f Filter) (Page, error)

	// UnfinishedProcesses returns every execution that was not in a terminal
	// state, used by the startup reconciliation pass.
	UnfinishedProcesses(ctx context.Context) ([]*core.Process, error)

	// AcquireLock claims key for the execution, or returns core.ErrLockHeld.
	AcquireLock(ctx context.Context, key, processID string) error
	// ReleaseLock frees key if it is still held by the execution.
	ReleaseLock(ctx context.Context, key, processID string) error
	// ActiveLocks rebuilds the lock table view after a restart.
	ActiveLocks(ctx context.Context) (map[string]string, error)

	SaveIdempotency(ctx context.Context, record Idempotency) error
	FindIdempotency(ctx context.Context, key string) (Idempotency, error)

	// PurgeHistory deletes terminal executions older than before, keeping at
	// most maxRows rows. It returns how many rows were removed.
	PurgeHistory(ctx context.Context, before time.Time, maxRows int) (int, error)

	Close() error
}
