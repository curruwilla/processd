package queue

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/curruwilla/processd/internal/config"
	"github.com/curruwilla/processd/internal/core"
	"github.com/curruwilla/processd/internal/store"
	"github.com/curruwilla/processd/internal/store/sqlite"
)

// fakeStarter records the executions handed to it, leaving them in STARTING so
// their slot stays held.
type fakeStarter struct {
	mu      sync.Mutex
	started []string
	err     error
}

func (f *fakeStarter) Start(_ context.Context, p *core.Process) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return f.err
	}

	f.started = append(f.started, p.ID)

	return nil
}

func (f *fakeStarter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.started)
}

const workersFile = `
version: 1
workers:
  - name: invoice
    command: /bin/echo
    args: ["--id={{id}}"]
    params:
      id: {required: true, pattern: "^[0-9]+$"}
    max_processes: 2
    lock_conflict: queue
  - name: exclusive
    command: /bin/echo
    lock_conflict: reject
`

func newScheduler(t *testing.T, tune func(*config.Config)) (*Scheduler, store.Store, *fakeStarter) {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "workers.yaml"), []byte(workersFile), 0o600); err != nil {
		t.Fatalf("writing workers: %v", err)
	}

	registry, err := config.LoadWorkers(dir)
	if err != nil {
		t.Fatalf("LoadWorkers() returned %v, want nil", err)
	}

	db, err := sqlite.Open(filepath.Join(t.TempDir(), "processd.db"))
	if err != nil {
		t.Fatalf("Open() returned %v, want nil", err)
	}

	t.Cleanup(func() {
		_ = db.Close()
	})

	cfg := config.Default()
	cfg.MaxProcesses = 2
	cfg.Queue.MaxDepth = 3

	if tune != nil {
		tune(&cfg)
	}

	starter := &fakeStarter{}

	return New(cfg, db, registry, starter, slog.New(slog.DiscardHandler)), db, starter
}

func newSubmission(id, worker, lock string) *core.Process {
	return &core.Process{
		ID:          id,
		Worker:      worker,
		Type:        core.TypeTask,
		State:       core.StateCreated,
		Command:     "/bin/echo",
		Cwd:         "/tmp",
		Lock:        lock,
		MaxAttempts: 1,
		CreatedAt:   time.Now().UTC(),
	}
}

func TestScheduler_Submit(t *testing.T) {
	t.Parallel()

	t.Run("starts when a slot is free", func(t *testing.T) {
		t.Parallel()

		scheduler, db, starter := newScheduler(t, nil)
		p := newSubmission("proc_1", "invoice", "")

		if err := scheduler.Submit(t.Context(), p); err != nil {
			t.Fatalf("Submit() returned %v, want nil", err)
		}

		if p.State != core.StateStarting {
			t.Errorf("state = %s, want %s", p.State, core.StateStarting)
		}

		if p.Attempt != 1 {
			t.Errorf("attempt = %d, want 1", p.Attempt)
		}

		if starter.count() != 1 {
			t.Errorf("starter ran %d executions, want 1", starter.count())
		}

		stored, err := db.GetProcess(t.Context(), "proc_1")
		if err != nil {
			t.Fatalf("GetProcess() returned %v, want nil", err)
		}

		if stored.State != core.StateStarting {
			t.Errorf("persisted state = %s, want %s", stored.State, core.StateStarting)
		}
	})

	t.Run("queues when the node is full", func(t *testing.T) {
		t.Parallel()

		scheduler, _, starter := newScheduler(t, nil)

		for i := range 3 {
			p := newSubmission("proc_"+string(rune('a'+i)), "invoice", "")
			if err := scheduler.Submit(t.Context(), p); err != nil {
				t.Fatalf("Submit() returned %v, want nil", err)
			}

			// The node allows two concurrent executions; the third waits.
			want := core.StateStarting
			if i == 2 {
				want = core.StateQueued
			}

			if p.State != want {
				t.Errorf("execution %d state = %s, want %s", i, p.State, want)
			}
		}

		if starter.count() != 2 {
			t.Errorf("starter ran %d executions, want 2", starter.count())
		}
	})

	t.Run("refuses when the queue is full", func(t *testing.T) {
		t.Parallel()

		scheduler, _, _ := newScheduler(t, func(cfg *config.Config) { cfg.Queue.MaxDepth = 1 })

		// Two slots plus one queue place, then the queue is at its bound.
		for i := range 3 {
			if err := scheduler.Submit(t.Context(), newSubmission("proc_"+string(rune('a'+i)), "invoice", "")); err != nil {
				t.Fatalf("Submit() returned %v, want nil", err)
			}
		}

		err := scheduler.Submit(t.Context(), newSubmission("proc_over", "invoice", ""))
		if !errors.Is(err, core.ErrQueueFull) {
			t.Errorf("Submit() returned %v, want core.ErrQueueFull", err)
		}
	})

	t.Run("refuses while draining", func(t *testing.T) {
		t.Parallel()

		scheduler, _, _ := newScheduler(t, nil)
		scheduler.Drain()

		err := scheduler.Submit(t.Context(), newSubmission("proc_1", "invoice", ""))
		if !errors.Is(err, core.ErrShuttingDown) {
			t.Errorf("Submit() returned %v, want core.ErrShuttingDown", err)
		}
	})
}

func TestScheduler_Submit_locks(t *testing.T) {
	t.Parallel()

	t.Run("a queueing worker waits for the lock", func(t *testing.T) {
		t.Parallel()

		scheduler, _, starter := newScheduler(t, nil)

		first := newSubmission("proc_1", "invoice", "invoice:1")
		if err := scheduler.Submit(t.Context(), first); err != nil {
			t.Fatalf("Submit() returned %v, want nil", err)
		}

		second := newSubmission("proc_2", "invoice", "invoice:1")
		if err := scheduler.Submit(t.Context(), second); err != nil {
			t.Fatalf("Submit() returned %v, want nil", err)
		}

		if second.State != core.StateQueued {
			t.Errorf("state = %s, want %s", second.State, core.StateQueued)
		}

		if starter.count() != 1 {
			t.Errorf("starter ran %d executions, want 1", starter.count())
		}
	})

	t.Run("a rejecting worker answers immediately", func(t *testing.T) {
		t.Parallel()

		scheduler, db, _ := newScheduler(t, nil)

		if err := scheduler.Submit(t.Context(), newSubmission("proc_1", "exclusive", "batch")); err != nil {
			t.Fatalf("Submit() returned %v, want nil", err)
		}

		second := newSubmission("proc_2", "exclusive", "batch")

		err := scheduler.Submit(t.Context(), second)
		if !errors.Is(err, core.ErrLockHeld) {
			t.Fatalf("Submit() returned %v, want core.ErrLockHeld", err)
		}

		stored, err := db.GetProcess(t.Context(), "proc_2")
		if err != nil {
			t.Fatalf("GetProcess() returned %v, want nil", err)
		}

		if stored.State != core.StateCanceled || stored.Reason != core.ReasonLockConflict {
			t.Errorf("rejected execution is %s/%s, want CANCELED/lock_conflict", stored.State, stored.Reason)
		}
	})
}

func TestScheduler_dispatch(t *testing.T) {
	t.Parallel()

	t.Run("starts queued work once a slot frees up", func(t *testing.T) {
		t.Parallel()

		scheduler, _, starter := newScheduler(t, nil)
		ctx := t.Context()

		running := []*core.Process{}

		for i := range 3 {
			p := newSubmission("proc_"+string(rune('a'+i)), "invoice", "")
			if err := scheduler.Submit(ctx, p); err != nil {
				t.Fatalf("Submit() returned %v, want nil", err)
			}

			running = append(running, p)
		}

		scheduler.OnAttemptFinished(running[0])
		scheduler.dispatch(ctx)

		if starter.count() != 3 {
			t.Errorf("starter ran %d executions, want 3", starter.count())
		}
	})

	t.Run("expires an execution that waited too long", func(t *testing.T) {
		t.Parallel()

		scheduler, db, _ := newScheduler(t, func(cfg *config.Config) {
			cfg.MaxProcesses = 1
			cfg.Queue.ItemTTL = config.Duration(time.Nanosecond)
		})
		ctx := t.Context()

		if err := scheduler.Submit(ctx, newSubmission("proc_running", "invoice", "")); err != nil {
			t.Fatalf("Submit() returned %v, want nil", err)
		}

		queued := newSubmission("proc_queued", "invoice", "")
		if err := scheduler.Submit(ctx, queued); err != nil {
			t.Fatalf("Submit() returned %v, want nil", err)
		}

		if queued.State != core.StateQueued {
			t.Fatalf("state = %s, want %s", queued.State, core.StateQueued)
		}

		scheduler.dispatch(ctx)

		stored, err := db.GetProcess(ctx, "proc_queued")
		if err != nil {
			t.Fatalf("GetProcess() returned %v, want nil", err)
		}

		if stored.State != core.StateFailed || stored.Reason != core.ReasonQueueTimeout {
			t.Errorf("expired execution is %s/%s, want FAILED/queue_timeout", stored.State, stored.Reason)
		}
	})
}
