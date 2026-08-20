package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/curruwilla/processd/internal/core"
	"github.com/curruwilla/processd/internal/store"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()

	db, err := Open(filepath.Join(t.TempDir(), "processd.db"))
	if err != nil {
		t.Fatalf("Open() returned %v, want nil", err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close() returned %v, want nil", err)
		}
	})

	return db
}

func newProcess(id string, state core.State) *core.Process {
	return &core.Process{
		ID:          id,
		Worker:      "invoice",
		Type:        core.TypeTask,
		State:       state,
		Command:     "/bin/echo",
		Args:        []string{"hello"},
		Env:         map[string]string{"APP_ENV": "test"},
		Cwd:         "/tmp",
		MaxAttempts: 3,
		Timeout:     30 * time.Second,
		Metadata:    map[string]string{"origin": "test"},
		CreatedAt:   time.Now().UTC(),
	}
}

func TestStore_CreateProcess(t *testing.T) {
	t.Parallel()

	db := newTestStore(t)
	ctx := t.Context()

	want := newProcess("proc_1", core.StateCreated)
	if err := db.CreateProcess(ctx, want); err != nil {
		t.Fatalf("CreateProcess() returned %v, want nil", err)
	}

	got, err := db.GetProcess(ctx, "proc_1")
	if err != nil {
		t.Fatalf("GetProcess() returned %v, want nil", err)
	}

	if got.Worker != want.Worker || got.Command != want.Command || got.Cwd != want.Cwd {
		t.Errorf("definition round-tripped as %+v, want it to match %+v", got, want)
	}

	if len(got.Args) != 1 || got.Args[0] != "hello" {
		t.Errorf("args = %q, want [hello]", got.Args)
	}

	if got.Env["APP_ENV"] != "test" {
		t.Errorf("env = %v, want APP_ENV=test", got.Env)
	}

	if got.Metadata["origin"] != "test" {
		t.Errorf("metadata = %v, want origin=test", got.Metadata)
	}

	if got.Timeout != 30*time.Second {
		t.Errorf("timeout = %s, want 30s", got.Timeout)
	}

	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("created_at = %s, want %s", got.CreatedAt, want.CreatedAt)
	}
}

func TestStore_GetProcess_missing(t *testing.T) {
	t.Parallel()

	_, err := newTestStore(t).GetProcess(t.Context(), "proc_absent")
	if !errors.Is(err, core.ErrNotFound) {
		t.Errorf("GetProcess() returned %v, want core.ErrNotFound", err)
	}
}

func TestStore_UpdateProcess(t *testing.T) {
	t.Parallel()

	db := newTestStore(t)
	ctx := t.Context()

	p := newProcess("proc_1", core.StateCreated)
	if err := db.CreateProcess(ctx, p); err != nil {
		t.Fatalf("CreateProcess() returned %v, want nil", err)
	}

	started := time.Now().UTC()
	exit := 0
	p.State = core.StateCompleted
	p.Attempt = 2
	p.PID = 4242
	p.PIDStartTime = 999
	p.ExitCode = &exit
	p.Signal = ""
	p.StartedAt = &started
	p.FinishedAt = &started

	if err := db.UpdateProcess(ctx, p); err != nil {
		t.Fatalf("UpdateProcess() returned %v, want nil", err)
	}

	got, err := db.GetProcess(ctx, "proc_1")
	if err != nil {
		t.Fatalf("GetProcess() returned %v, want nil", err)
	}

	if got.State != core.StateCompleted || got.Attempt != 2 || got.PID != 4242 {
		t.Errorf("update round-tripped as state=%s attempt=%d pid=%d", got.State, got.Attempt, got.PID)
	}

	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", got.ExitCode)
	}

	if got.PIDStartTime != 999 {
		t.Errorf("pid start time = %d, want 999", got.PIDStartTime)
	}

	// The attempt row is what makes per-try logs and exit codes addressable.
	var attempts int

	row := db.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM attempts WHERE process_id = ? AND attempt = 2`, "proc_1")
	if err := row.Scan(&attempts); err != nil {
		t.Fatalf("counting attempts: %v", err)
	}

	if attempts != 1 {
		t.Errorf("attempt rows = %d, want 1", attempts)
	}
}

func TestStore_UpdateProcess_missing(t *testing.T) {
	t.Parallel()

	err := newTestStore(t).UpdateProcess(t.Context(), newProcess("proc_absent", core.StateRunning))
	if !errors.Is(err, core.ErrNotFound) {
		t.Errorf("UpdateProcess() returned %v, want core.ErrNotFound", err)
	}
}

func TestStore_ListProcesses(t *testing.T) {
	t.Parallel()

	db := newTestStore(t)
	ctx := t.Context()

	base := time.Now().UTC()

	for i := range 5 {
		p := newProcess("proc_"+string(rune('a'+i)), core.StateCompleted)
		p.CreatedAt = base.Add(time.Duration(i) * time.Second)

		if i%2 == 0 {
			p.Worker = "other"
		}

		if err := db.CreateProcess(ctx, p); err != nil {
			t.Fatalf("CreateProcess() returned %v, want nil", err)
		}
	}

	t.Run("newest first with cursor paging", func(t *testing.T) {
		t.Parallel()

		first, err := db.ListProcesses(ctx, store.Filter{Limit: 2})
		if err != nil {
			t.Fatalf("ListProcesses() returned %v, want nil", err)
		}

		if len(first.Items) != 2 {
			t.Fatalf("page held %d items, want 2", len(first.Items))
		}

		if first.NextCursor == "" {
			t.Fatal("next cursor is empty, want more pages")
		}

		if first.Items[0].CreatedAt.Before(first.Items[1].CreatedAt) {
			t.Error("items are oldest first, want newest first")
		}

		second, err := db.ListProcesses(ctx, store.Filter{Limit: 2, Cursor: first.NextCursor})
		if err != nil {
			t.Fatalf("ListProcesses() returned %v, want nil", err)
		}

		if second.Items[0].ID == first.Items[0].ID {
			t.Error("second page repeats the first, want it to continue")
		}
	})

	t.Run("filters by worker", func(t *testing.T) {
		t.Parallel()

		page, err := db.ListProcesses(ctx, store.Filter{Worker: "invoice"})
		if err != nil {
			t.Fatalf("ListProcesses() returned %v, want nil", err)
		}

		if len(page.Items) != 2 {
			t.Errorf("worker filter returned %d items, want 2", len(page.Items))
		}
	})

	t.Run("filters by state", func(t *testing.T) {
		t.Parallel()

		page, err := db.ListProcesses(ctx, store.Filter{States: []core.State{core.StateRunning}})
		if err != nil {
			t.Fatalf("ListProcesses() returned %v, want nil", err)
		}

		if len(page.Items) != 0 {
			t.Errorf("state filter returned %d items, want 0", len(page.Items))
		}
	})
}

func TestStore_PendingProcesses(t *testing.T) {
	t.Parallel()

	db := newTestStore(t)
	ctx := t.Context()
	now := time.Now().UTC()

	queued := newProcess("proc_queued", core.StateQueued)
	ready := newProcess("proc_ready", core.StateRetrying)
	past := now.Add(-time.Minute)
	ready.RetryAt = &past

	waiting := newProcess("proc_waiting", core.StateRetrying)
	future := now.Add(time.Hour)
	waiting.RetryAt = &future

	running := newProcess("proc_running", core.StateRunning)

	for _, p := range []*core.Process{queued, ready, waiting, running} {
		if err := db.CreateProcess(ctx, p); err != nil {
			t.Fatalf("CreateProcess() returned %v, want nil", err)
		}
	}

	pending, err := db.PendingProcesses(ctx, now)
	if err != nil {
		t.Fatalf("PendingProcesses() returned %v, want nil", err)
	}

	got := map[string]bool{}
	for _, p := range pending {
		got[p.ID] = true
	}

	if !got["proc_queued"] || !got["proc_ready"] {
		t.Errorf("pending = %v, want the queued and the elapsed retry", got)
	}

	if got["proc_waiting"] || got["proc_running"] {
		t.Errorf("pending = %v, want no future retry and no running execution", got)
	}
}

func TestStore_UnfinishedProcesses(t *testing.T) {
	t.Parallel()

	db := newTestStore(t)
	ctx := t.Context()

	for id, state := range map[string]core.State{
		"proc_running":   core.StateRunning,
		"proc_queued":    core.StateQueued,
		"proc_done":      core.StateCompleted,
		"proc_failed":    core.StateFailed,
		"proc_cancelled": core.StateCanceled,
	} {
		if err := db.CreateProcess(ctx, newProcess(id, state)); err != nil {
			t.Fatalf("CreateProcess() returned %v, want nil", err)
		}
	}

	unfinished, err := db.UnfinishedProcesses(ctx)
	if err != nil {
		t.Fatalf("UnfinishedProcesses() returned %v, want nil", err)
	}

	if len(unfinished) != 2 {
		t.Errorf("unfinished = %d executions, want 2", len(unfinished))
	}
}

func TestStore_CountByState(t *testing.T) {
	t.Parallel()

	db := newTestStore(t)
	ctx := t.Context()

	for i := range 3 {
		if err := db.CreateProcess(ctx, newProcess("proc_q"+string(rune('a'+i)), core.StateQueued)); err != nil {
			t.Fatalf("CreateProcess() returned %v, want nil", err)
		}
	}

	if err := db.CreateProcess(ctx, newProcess("proc_run", core.StateRunning)); err != nil {
		t.Fatalf("CreateProcess() returned %v, want nil", err)
	}

	counts, err := db.CountByState(ctx)
	if err != nil {
		t.Fatalf("CountByState() returned %v, want nil", err)
	}

	if counts[core.StateQueued] != 3 || counts[core.StateRunning] != 1 {
		t.Errorf("counts = %v, want 3 queued and 1 running", counts)
	}
}

func TestStore_PurgeHistory(t *testing.T) {
	t.Parallel()

	db := newTestStore(t)
	ctx := t.Context()

	old := time.Now().UTC().Add(-48 * time.Hour)

	stale := newProcess("proc_stale", core.StateCompleted)
	stale.CreatedAt = old
	stale.FinishedAt = &old

	recent := newProcess("proc_recent", core.StateCompleted)
	running := newProcess("proc_running", core.StateRunning)
	running.CreatedAt = old

	for _, p := range []*core.Process{stale, recent, running} {
		if err := db.CreateProcess(ctx, p); err != nil {
			t.Fatalf("CreateProcess() returned %v, want nil", err)
		}
	}

	removed, err := db.PurgeHistory(ctx, time.Now().UTC().Add(-24*time.Hour), 0)
	if err != nil {
		t.Fatalf("PurgeHistory() returned %v, want nil", err)
	}

	if removed != 1 {
		t.Errorf("purge removed %d rows, want 1", removed)
	}

	if _, err := db.GetProcess(ctx, "proc_recent"); err != nil {
		t.Errorf("recent execution was purged: %v", err)
	}

	// An unfinished execution is never history, however old it looks.
	if _, err := db.GetProcess(ctx, "proc_running"); err != nil {
		t.Errorf("running execution was purged: %v", err)
	}
}

func TestStore_PurgeHistory_trimsToMaxRows(t *testing.T) {
	t.Parallel()

	db := newTestStore(t)
	ctx := context.Background()
	base := time.Now().UTC()

	for i := range 5 {
		p := newProcess("proc_"+string(rune('a'+i)), core.StateCompleted)
		p.CreatedAt = base.Add(time.Duration(i) * time.Second)

		if err := db.CreateProcess(ctx, p); err != nil {
			t.Fatalf("CreateProcess() returned %v, want nil", err)
		}
	}

	if _, err := db.PurgeHistory(ctx, base.Add(-time.Hour), 2); err != nil {
		t.Fatalf("PurgeHistory() returned %v, want nil", err)
	}

	counts, err := db.CountByState(ctx)
	if err != nil {
		t.Fatalf("CountByState() returned %v, want nil", err)
	}

	if counts[core.StateCompleted] != 2 {
		t.Errorf("history holds %d rows, want 2", counts[core.StateCompleted])
	}
}

func TestStore_PendingCount(t *testing.T) {
	t.Parallel()

	db := newTestStore(t)
	ctx := t.Context()

	states := map[string]core.State{
		"proc_q1":   core.StateQueued,
		"proc_q2":   core.StateQueued,
		"proc_r1":   core.StateRetrying,
		"proc_run":  core.StateRunning,
		"proc_done": core.StateCompleted,
		"proc_fail": core.StateFailed,
	}

	for id, state := range states {
		if err := db.CreateProcess(ctx, newProcess(id, state)); err != nil {
			t.Fatalf("CreateProcess() returned %v, want nil", err)
		}
	}

	count, err := db.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount() returned %v, want nil", err)
	}

	// Queued plus retrying, and nothing else: history must not affect admission.
	if count != 3 {
		t.Errorf("PendingCount() = %d, want 3", count)
	}
}

func TestStore_CountActiveByState(t *testing.T) {
	t.Parallel()

	db := newTestStore(t)
	ctx := t.Context()

	states := []core.State{
		core.StateQueued, core.StateQueued, core.StateRunning,
		core.StateCompleted, core.StateFailed, core.StateCanceled,
	}

	for i, state := range states {
		if err := db.CreateProcess(ctx, newProcess(fmt.Sprintf("proc_%d", i), state)); err != nil {
			t.Fatalf("CreateProcess() returned %v, want nil", err)
		}
	}

	active, err := db.CountActiveByState(ctx)
	if err != nil {
		t.Fatalf("CountActiveByState() returned %v, want nil", err)
	}

	if len(active) != 2 || active[core.StateQueued] != 2 || active[core.StateRunning] != 1 {
		t.Errorf("CountActiveByState() = %v, want only the non-terminal states", active)
	}

	all, err := db.CountByState(ctx)
	if err != nil {
		t.Fatalf("CountByState() returned %v, want nil", err)
	}

	if len(all) != 5 {
		t.Errorf("CountByState() = %v, want every state including the terminal ones", all)
	}
}

func TestStore_CountPendingByWorker(t *testing.T) {
	t.Parallel()

	db := newTestStore(t)
	ctx := t.Context()

	queued := newProcess("proc_1", core.StateQueued)
	retrying := newProcess("proc_2", core.StateRetrying)
	running := newProcess("proc_3", core.StateRunning)
	other := newProcess("proc_4", core.StateQueued)
	other.Worker = "report"

	for _, p := range []*core.Process{queued, retrying, running, other} {
		if err := db.CreateProcess(ctx, p); err != nil {
			t.Fatalf("CreateProcess() returned %v, want nil", err)
		}
	}

	counts, err := db.CountPendingByWorker(ctx)
	if err != nil {
		t.Fatalf("CountPendingByWorker() returned %v, want nil", err)
	}

	if counts["invoice"] != 2 || counts["report"] != 1 || len(counts) != 2 {
		t.Errorf("CountPendingByWorker() = %v, want 2 pending for invoice and 1 for report", counts)
	}
}

func TestStore_Ping(t *testing.T) {
	t.Parallel()

	db := newTestStore(t)

	if err := db.Ping(t.Context()); err != nil {
		t.Errorf("Ping() returned %v, want nil", err)
	}
}
