package schedule

import (
	"context"
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

// fakeSubmitter records what the runner asked to execute.
type fakeSubmitter struct {
	mu        sync.Mutex
	submitted []*core.Process
	err       error
}

func (f *fakeSubmitter) Submit(_ context.Context, p *core.Process) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return f.err
	}

	f.submitted = append(f.submitted, p)

	return nil
}

func (f *fakeSubmitter) all() []*core.Process {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]*core.Process(nil), f.submitted...)
}

func (f *fakeSubmitter) count() int { return len(f.all()) }

func newRunner(t *testing.T, workersYAML string) (*Runner, *fakeSubmitter, store.Store) {
	t.Helper()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "workers.yaml"), []byte(workersYAML), 0o600); err != nil {
		t.Fatalf("writing workers file: %v", err)
	}

	registry, err := config.LoadWorkers(dir)
	if err != nil {
		t.Fatalf("loading workers: %v", err)
	}

	db, err := sqlite.Open(filepath.Join(t.TempDir(), "processd.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}

	t.Cleanup(func() {
		_ = db.Close()
	})

	submitter := &fakeSubmitter{}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	return New(func() *config.Registry { return registry }, submitter, db, log), submitter, db
}

const dailyWorker = `
version: 1
workers:
  - name: invoice
    command: /bin/echo
    schedule:
      cron: "0 3 * * *"
`

func TestRunner_PlansTheNextOccurrence(t *testing.T) {
	t.Parallel()

	runner, submitter, _ := newRunner(t, dailyWorker)

	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	runner.reset(t.Context(), now)

	status, ok := runner.Status("invoice")
	if !ok {
		t.Fatal("Status reported no schedule for a scheduled worker")
	}

	want := time.Date(2026, 8, 20, 3, 0, 0, 0, time.UTC)
	if status.NextRun == nil || !status.NextRun.Equal(want) {
		t.Fatalf("NextRun = %v, want %v", status.NextRun, want)
	}

	// Nothing is due yet.
	runner.fireDue(t.Context(), now)

	if submitter.count() != 0 {
		t.Fatalf("submitted %d executions before the occurrence", submitter.count())
	}
}

func TestRunner_UnscheduledWorkerHasNoStatus(t *testing.T) {
	t.Parallel()

	runner, _, _ := newRunner(t, `
version: 1
workers:
  - name: manual
    command: /bin/echo
`)

	runner.reset(t.Context(), time.Now())

	if _, ok := runner.Status("manual"); ok {
		t.Fatal("Status reported a schedule for a worker that has none")
	}
}

func TestRunner_FiresAndRecordsTheOccurrence(t *testing.T) {
	t.Parallel()

	runner, submitter, db := newRunner(t, dailyWorker)

	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	runner.reset(t.Context(), now)

	fired := time.Date(2026, 8, 20, 3, 0, 0, 0, time.UTC)
	runner.fireDue(t.Context(), fired)

	submissions := submitter.all()
	if len(submissions) != 1 {
		t.Fatalf("submitted %d executions, want 1", len(submissions))
	}

	process := submissions[0]

	if process.Worker != "invoice" {
		t.Fatalf("worker = %q", process.Worker)
	}

	// A schedule with no lock of its own gets one, so that overlap is decided by
	// lock_conflict instead of being unbounded.
	if process.Lock != "schedule:invoice" {
		t.Fatalf("lock = %q, want the implicit schedule lock", process.Lock)
	}

	if process.Metadata[triggerKey] != triggerSchedule {
		t.Fatalf("metadata %v does not mark the execution as scheduled", process.Metadata)
	}

	if got := process.Metadata[occurrenceKey]; got != fired.Format(time.RFC3339) {
		t.Fatalf("occurrence metadata = %q, want %q", got, fired.Format(time.RFC3339))
	}

	// The firing is persisted against the occurrence, not the wall clock.
	states, err := db.LoadSchedules(t.Context())
	if err != nil {
		t.Fatalf("loading schedules: %v", err)
	}

	state := states["invoice"]
	if state.LastFiredAt == nil || !state.LastFiredAt.Equal(fired) {
		t.Fatalf("LastFiredAt = %v, want %v", state.LastFiredAt, fired)
	}

	// And the schedule moved on rather than firing again on the next pass.
	runner.fireDue(t.Context(), fired.Add(time.Minute))

	if submitter.count() != 1 {
		t.Fatalf("submitted %d executions, want the schedule to have advanced", submitter.count())
	}
}

// TestRunner_SkipsARepeatedLocalOccurrence covers the fall-back hour: two
// instants share one local time, and the schedule means one firing.
func TestRunner_SkipsARepeatedLocalOccurrence(t *testing.T) {
	t.Parallel()

	runner, submitter, _ := newRunner(t, dailyWorker)

	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	runner.reset(t.Context(), now)

	occurrence := time.Date(2026, 8, 20, 3, 0, 0, 0, time.UTC)
	runner.fireDue(t.Context(), occurrence)

	// Rewind the plan to the same occurrence, as the repeated hour would.
	runner.mu.Lock()
	runner.entries["invoice"].occurrence = occurrence
	runner.entries["invoice"].fireAt = occurrence
	runner.mu.Unlock()

	runner.fireDue(t.Context(), occurrence.Add(time.Hour))

	if submitter.count() != 1 {
		t.Fatalf("submitted %d executions, want the repeated local time to be skipped", submitter.count())
	}
}

func TestRunner_RecordsMissedOccurrencesWithoutRunningThem(t *testing.T) {
	t.Parallel()

	runner, submitter, db := newRunner(t, dailyWorker)

	// The daemon last fired four days ago and has been down since.
	lastFired := time.Date(2026, 8, 16, 3, 0, 0, 0, time.UTC)
	if err := db.SaveSchedule(t.Context(), store.ScheduleState{Worker: "invoice", LastFiredAt: &lastFired}); err != nil {
		t.Fatalf("seeding schedule state: %v", err)
	}

	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	runner.reset(t.Context(), now)

	if submitter.count() != 0 {
		t.Fatalf("submitted %d executions, want catch_up: false to run nothing", submitter.count())
	}

	status, _ := runner.Status("invoice")
	if status.MissedRuns != 4 {
		t.Fatalf("MissedRuns = %d, want 4", status.MissedRuns)
	}

	wantMissed := time.Date(2026, 8, 20, 3, 0, 0, 0, time.UTC)
	if status.LastMissedAt == nil || !status.LastMissedAt.Equal(wantMissed) {
		t.Fatalf("LastMissedAt = %v, want %v", status.LastMissedAt, wantMissed)
	}

	// The miss survives a restart: it is a fact about the node, not a log line.
	states, err := db.LoadSchedules(t.Context())
	if err != nil {
		t.Fatalf("loading schedules: %v", err)
	}

	if states["invoice"].MissedRuns != 4 {
		t.Fatalf("persisted MissedRuns = %d, want 4", states["invoice"].MissedRuns)
	}

	wantNext := time.Date(2026, 8, 21, 3, 0, 0, 0, time.UTC)
	if status.NextRun == nil || !status.NextRun.Equal(wantNext) {
		t.Fatalf("NextRun = %v, want %v", status.NextRun, wantNext)
	}
}

// A restart is not a new outage. Counting downtime from the last firing alone
// would report the same window again on every start before the schedule next
// fires, and the number an operator reads would grow with how often the daemon
// was restarted rather than with how much did not run.
func TestRunner_CountsAMissedWindowOnce(t *testing.T) {
	t.Parallel()

	runner, _, db := newRunner(t, dailyWorker)

	lastFired := time.Date(2026, 8, 16, 3, 0, 0, 0, time.UTC)
	if err := db.SaveSchedule(t.Context(), store.ScheduleState{Worker: "invoice", LastFiredAt: &lastFired}); err != nil {
		t.Fatalf("seeding schedule state: %v", err)
	}

	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)

	for restart := 1; restart <= 3; restart++ {
		runner.reset(t.Context(), now)

		status, _ := runner.Status("invoice")
		if status.MissedRuns != 4 {
			t.Fatalf("after restart %d MissedRuns = %d, want 4", restart, status.MissedRuns)
		}
	}

	// A later restart accounts for what passed since, and only that.
	later := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	runner.reset(t.Context(), later)

	status, _ := runner.Status("invoice")
	if status.MissedRuns != 6 {
		t.Errorf("MissedRuns = %d, want 6: the two occurrences since the last pass", status.MissedRuns)
	}
}

// Status is read by the API from its own goroutines while the firing loop keeps
// moving the plan forward.
func TestRunner_StatusIsSafeWhileTheLoopAdvances(t *testing.T) {
	t.Parallel()

	runner, _, _ := newRunner(t, `
version: 1
workers:
  - name: invoice
    command: /bin/echo
    cwd: /tmp
    schedule:
      cron: "* * * * *"
`)

	base := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	runner.reset(t.Context(), base)

	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)

		for {
			select {
			case <-stop:
				return
			default:
				runner.Status("invoice")
			}
		}
	}()

	for i := range 200 {
		runner.fireDue(t.Context(), base.Add(time.Duration(i+2)*time.Minute))
	}

	close(stop)
	<-done
}

func TestRunner_CatchUpRunsTheLastMissedOccurrenceOnce(t *testing.T) {
	t.Parallel()

	runner, submitter, db := newRunner(t, `
version: 1
workers:
  - name: invoice
    command: /bin/echo
    schedule:
      cron: "0 3 * * *"
      catch_up: true
`)

	lastFired := time.Date(2026, 8, 16, 3, 0, 0, 0, time.UTC)
	if err := db.SaveSchedule(t.Context(), store.ScheduleState{Worker: "invoice", LastFiredAt: &lastFired}); err != nil {
		t.Fatalf("seeding schedule state: %v", err)
	}

	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	runner.reset(t.Context(), now)
	runner.fireDue(t.Context(), now)

	if submitter.count() != 1 {
		t.Fatalf("submitted %d executions, want exactly one catch-up firing", submitter.count())
	}

	got := submitter.all()[0].Metadata[occurrenceKey]
	want := time.Date(2026, 8, 20, 3, 0, 0, 0, time.UTC).Format(time.RFC3339)

	if got != want {
		t.Fatalf("caught up occurrence %q, want the most recent missed one %q", got, want)
	}
}

func TestRunner_SkipsADisabledWorker(t *testing.T) {
	t.Parallel()

	runner, submitter, _ := newRunner(t, `
version: 1
workers:
  - name: invoice
    command: /bin/echo
    enabled: false
    schedule:
      cron: "0 3 * * *"
`)

	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	runner.reset(t.Context(), now)
	runner.fireDue(t.Context(), time.Date(2026, 8, 20, 3, 0, 0, 0, time.UTC))

	if submitter.count() != 0 {
		t.Fatalf("submitted %d executions for a disabled worker", submitter.count())
	}
}

func TestRunner_KeepsAWorkerDeclaredLock(t *testing.T) {
	t.Parallel()

	runner, submitter, _ := newRunner(t, `
version: 1
workers:
  - name: invoice
    command: /bin/echo
    lock: billing
    schedule:
      cron: "0 3 * * *"
`)

	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	runner.reset(t.Context(), now)
	runner.fireDue(t.Context(), time.Date(2026, 8, 20, 3, 0, 0, 0, time.UTC))

	if got := submitter.all()[0].Lock; got != "billing" {
		t.Fatalf("lock = %q, want the worker's own lock", got)
	}
}

// TestRunner_AdvancesPastABacklog covers a stalled loop: the occurrences it
// slept through are counted, not fired one per pass.
func TestRunner_AdvancesPastABacklog(t *testing.T) {
	t.Parallel()

	runner, submitter, _ := newRunner(t, `
version: 1
workers:
  - name: ping
    command: /bin/echo
    schedule:
      cron: "* * * * *"
`)

	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	runner.reset(t.Context(), now)

	// The host was suspended for an hour between the plan and the pass.
	late := now.Add(time.Hour)
	runner.fireDue(t.Context(), late)

	if submitter.count() != 1 {
		t.Fatalf("submitted %d executions, want one firing and the rest counted as missed", submitter.count())
	}

	status, _ := runner.Status("ping")
	if status.MissedRuns == 0 {
		t.Fatal("MissedRuns = 0, want the skipped occurrences to be recorded")
	}

	if status.NextRun == nil || !status.NextRun.After(late) {
		t.Fatalf("NextRun = %v, want an occurrence after %v", status.NextRun, late)
	}
}

func TestRunner_ReloadIsNonBlocking(t *testing.T) {
	t.Parallel()

	runner, _, _ := newRunner(t, dailyWorker)

	// Two reloads with nobody reading collapse into one pending rebuild.
	runner.Reload()
	runner.Reload()

	select {
	case <-runner.reloaded:
	default:
		t.Fatal("Reload did not signal the loop")
	}
}
