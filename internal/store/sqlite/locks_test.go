package sqlite

import (
	"errors"
	"testing"

	"github.com/curruwilla/processd/internal/core"
)

// newLockedStore returns a store where proc_1 already holds the given key.
func newLockedStore(t *testing.T, key string) *Store {
	t.Helper()

	db := newTestStore(t)

	for _, id := range []string{"proc_1", "proc_2"} {
		if err := db.CreateProcess(t.Context(), newProcess(id, core.StateStarting)); err != nil {
			t.Fatalf("CreateProcess() returned %v, want nil", err)
		}
	}

	if err := db.AcquireLock(t.Context(), key, "proc_1"); err != nil {
		t.Fatalf("AcquireLock() returned %v, want nil", err)
	}

	return db
}

func TestStore_AcquireLock_refusesAnotherExecution(t *testing.T) {
	t.Parallel()

	db := newLockedStore(t, "invoice:1")

	err := db.AcquireLock(t.Context(), "invoice:1", "proc_2")
	if !errors.Is(err, core.ErrLockHeld) {
		t.Errorf("AcquireLock() returned %v, want core.ErrLockHeld", err)
	}
}

func TestStore_AcquireLock_isIdempotentForTheHolder(t *testing.T) {
	t.Parallel()

	db := newLockedStore(t, "invoice:1")

	// A retry keeps its lock across the backoff, so re-acquiring must succeed.
	if err := db.AcquireLock(t.Context(), "invoice:1", "proc_1"); err != nil {
		t.Errorf("AcquireLock() returned %v, want nil", err)
	}
}

func TestStore_AcquireLock_emptyKeyIsNotALock(t *testing.T) {
	t.Parallel()

	db := newTestStore(t)

	if err := db.AcquireLock(t.Context(), "", "proc_1"); err != nil {
		t.Errorf("AcquireLock() returned %v, want nil", err)
	}

	locks, err := db.ActiveLocks(t.Context())
	if err != nil {
		t.Fatalf("ActiveLocks() returned %v, want nil", err)
	}

	if len(locks) != 0 {
		t.Errorf("locks = %v, want none", locks)
	}
}

func TestStore_ReleaseLock(t *testing.T) {
	t.Parallel()

	db := newLockedStore(t, "invoice:1")

	if err := db.ReleaseLock(t.Context(), "invoice:1", "proc_1"); err != nil {
		t.Fatalf("ReleaseLock() returned %v, want nil", err)
	}

	if err := db.AcquireLock(t.Context(), "invoice:1", "proc_2"); err != nil {
		t.Errorf("AcquireLock() after release returned %v, want nil", err)
	}
}

func TestStore_ReleaseLock_byNonHolderIsANoOp(t *testing.T) {
	t.Parallel()

	db := newLockedStore(t, "invoice:1")

	if err := db.ReleaseLock(t.Context(), "invoice:1", "proc_2"); err != nil {
		t.Fatalf("ReleaseLock() returned %v, want nil", err)
	}

	locks, err := db.ActiveLocks(t.Context())
	if err != nil {
		t.Fatalf("ActiveLocks() returned %v, want nil", err)
	}

	if locks["invoice:1"] != "proc_1" {
		t.Errorf("locks = %v, want invoice:1 still held by proc_1", locks)
	}
}
