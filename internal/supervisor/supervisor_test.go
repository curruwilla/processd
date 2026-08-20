package supervisor

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/curruwilla/processd/internal/config"
	"github.com/curruwilla/processd/internal/core"
	"github.com/curruwilla/processd/internal/logstore"
	"github.com/curruwilla/processd/internal/runner"
	"github.com/curruwilla/processd/internal/store"
	"github.com/curruwilla/processd/internal/store/sqlite"
)

const workersFile = `
version: 1
workers:
  - name: hello
    command: /bin/echo
    args: ["hello"]
    cwd: /tmp
  - name: flaky
    command: /bin/false
    cwd: /tmp
    retry:
      enabled: true
      max_attempts: 3
      backoff: {type: fixed, initial: 10ms, max: 10ms, jitter: 0}
  - name: doomed
    command: /bin/false
    cwd: /tmp
  - name: sleeper
    command: /bin/sleep
    args: ["30"]
    cwd: /tmp
    kill_grace: 1s
`

type harness struct {
	supervisor *Supervisor
	store      store.Store
	finished   chan *core.Process
}

func newHarness(t *testing.T, tune func(*config.Config)) *harness {
	t.Helper()

	workersDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workersDir, "w.yaml"), []byte(workersFile), 0o600); err != nil {
		t.Fatalf("writing workers: %v", err)
	}

	registry, err := config.LoadWorkers(workersDir)
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
	cfg.AllowRootProcesses = true

	if tune != nil {
		tune(&cfg)
	}

	logs := logstore.New(t.TempDir(), 1<<20)
	sup := New(cfg, db, runner.NewExecRunner(), logs, slog.New(slog.DiscardHandler))

	h := &harness{supervisor: sup, store: db, finished: make(chan *core.Process, 8)}

	sup.SetWorkers(func() *config.Registry { return registry })
	sup.SetOnFinish(func(p *core.Process) { h.finished <- p })

	return h
}

// start persists an execution in STARTING, the state the scheduler hands over.
func (h *harness) start(t *testing.T, id, worker, command string, args []string) *core.Process {
	t.Helper()

	p := &core.Process{
		ID:          id,
		Worker:      worker,
		Type:        core.TypeTask,
		State:       core.StateStarting,
		Command:     command,
		Args:        args,
		Cwd:         "/tmp",
		Attempt:     1,
		MaxAttempts: 3,
		CreatedAt:   time.Now().UTC(),
	}

	if err := h.store.CreateProcess(t.Context(), p); err != nil {
		t.Fatalf("CreateProcess() returned %v, want nil", err)
	}

	if err := h.supervisor.Start(t.Context(), p); err != nil {
		t.Fatalf("Start() returned %v, want nil", err)
	}

	return p
}

func (h *harness) awaitFinish(t *testing.T) *core.Process {
	t.Helper()

	select {
	case p := <-h.finished:
		return p
	case <-time.After(10 * time.Second):
		t.Fatal("execution did not finish in time")
		return nil
	}
}

func TestSupervisor_Start_completes(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.start(t, "proc_ok", "hello", "/bin/echo", []string{"hello"})

	finished := h.awaitFinish(t)

	if finished.State != core.StateCompleted {
		t.Errorf("state = %s, want %s", finished.State, core.StateCompleted)
	}

	if finished.ExitCode == nil || *finished.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", finished.ExitCode)
	}

	stored, err := h.store.GetProcess(t.Context(), "proc_ok")
	if err != nil {
		t.Fatalf("GetProcess() returned %v, want nil", err)
	}

	if stored.State != core.StateCompleted || stored.FinishedAt == nil {
		t.Errorf("persisted execution is %s, finished_at=%v", stored.State, stored.FinishedAt)
	}
}

func TestSupervisor_Start_retriesFailure(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.start(t, "proc_flaky", "flaky", "/bin/false", nil)

	finished := h.awaitFinish(t)

	if finished.State != core.StateRetrying {
		t.Fatalf("state = %s, want %s", finished.State, core.StateRetrying)
	}

	if finished.RetryAt == nil {
		t.Error("retry_at is nil, want the backoff deadline")
	}

	if finished.ExitCode == nil || *finished.ExitCode == 0 {
		t.Errorf("exit code = %v, want a failure code", finished.ExitCode)
	}
}

func TestSupervisor_Start_failsWithoutRetry(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.start(t, "proc_doomed", "doomed", "/bin/false", nil)

	finished := h.awaitFinish(t)

	if finished.State != core.StateFailed {
		t.Errorf("state = %s, want %s", finished.State, core.StateFailed)
	}
}

func TestSupervisor_Start_reportsStartFailure(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.start(t, "proc_missing", "hello", "/bin/does-not-exist", nil)

	finished := h.awaitFinish(t)

	if finished.State != core.StateFailed || finished.Reason != core.ReasonStartError {
		t.Errorf("execution is %s/%s, want FAILED/start_error", finished.State, finished.Reason)
	}
}

func TestSupervisor_Stop(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.start(t, "proc_sleep", "sleeper", "/bin/sleep", []string{"30"})

	if err := h.supervisor.Stop(t.Context(), "proc_sleep", time.Second); err != nil {
		t.Fatalf("Stop() returned %v, want nil", err)
	}

	finished := h.awaitFinish(t)

	if finished.State != core.StateCanceled || finished.Reason != core.ReasonUserRequest {
		t.Errorf("execution is %s/%s, want CANCELED/user_request", finished.State, finished.Reason)
	}

	if finished.Signal != "SIGTERM" {
		t.Errorf("signal = %q, want SIGTERM", finished.Signal)
	}
}

func TestSupervisor_Stop_queuedExecution(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)

	changed := make(chan struct{}, 1)

	h.supervisor.SetOnChange(func() { changed <- struct{}{} })

	p := &core.Process{
		ID:        "proc_queued",
		Worker:    "hello",
		Type:      core.TypeTask,
		State:     core.StateQueued,
		Command:   "/bin/echo",
		Cwd:       "/tmp",
		CreatedAt: time.Now().UTC(),
	}

	if err := h.store.CreateProcess(t.Context(), p); err != nil {
		t.Fatalf("CreateProcess() returned %v, want nil", err)
	}

	if err := h.supervisor.Stop(t.Context(), "proc_queued", time.Second); err != nil {
		t.Fatalf("Stop() returned %v, want nil", err)
	}

	stored, err := h.store.GetProcess(t.Context(), "proc_queued")
	if err != nil {
		t.Fatalf("GetProcess() returned %v, want nil", err)
	}

	if stored.State != core.StateCanceled {
		t.Errorf("state = %s, want %s", stored.State, core.StateCanceled)
	}

	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Error("cancelling a queued execution did not notify the scheduler")
	}
}

func TestSupervisor_timeout(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)

	p := &core.Process{
		ID:          "proc_timeout",
		Worker:      "sleeper",
		Type:        core.TypeTask,
		State:       core.StateStarting,
		Command:     "/bin/sleep",
		Args:        []string{"30"},
		Cwd:         "/tmp",
		Attempt:     1,
		MaxAttempts: 1,
		Timeout:     100 * time.Millisecond,
		CreatedAt:   time.Now().UTC(),
	}

	if err := h.store.CreateProcess(t.Context(), p); err != nil {
		t.Fatalf("CreateProcess() returned %v, want nil", err)
	}

	if err := h.supervisor.Start(t.Context(), p); err != nil {
		t.Fatalf("Start() returned %v, want nil", err)
	}

	finished := h.awaitFinish(t)

	if finished.State != core.StateFailed || finished.Reason != core.ReasonTimeout {
		t.Errorf("execution is %s/%s, want FAILED/timeout", finished.State, finished.Reason)
	}
}

func TestSupervisor_Shutdown(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.start(t, "proc_sleep", "sleeper", "/bin/sleep", []string{"30"})

	if err := h.supervisor.Shutdown(t.Context(), time.Second); err != nil {
		t.Fatalf("Shutdown() returned %v, want nil", err)
	}

	finished := h.awaitFinish(t)

	if finished.State != core.StateCanceled || finished.Reason != core.ReasonShutdown {
		t.Errorf("execution is %s/%s, want CANCELED/shutdown", finished.State, finished.Reason)
	}

	if h.supervisor.Running() != 0 {
		t.Errorf("%d executions still supervised, want 0", h.supervisor.Running())
	}
}

func TestSupervisor_Reconcile(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	ctx := t.Context()

	// A PID that no longer matches its recorded start time: the process the
	// previous daemon started is gone.
	interrupted := &core.Process{
		ID:           "proc_interrupted",
		Worker:       "doomed",
		Type:         core.TypeTask,
		State:        core.StateRunning,
		Command:      "/bin/false",
		Cwd:          "/tmp",
		Attempt:      1,
		MaxAttempts:  1,
		PID:          os.Getpid(),
		PIDStartTime: 1,
		CreatedAt:    time.Now().UTC(),
	}

	created := &core.Process{
		ID:        "proc_created",
		Worker:    "hello",
		Type:      core.TypeTask,
		State:     core.StateCreated,
		Command:   "/bin/echo",
		Cwd:       "/tmp",
		CreatedAt: time.Now().UTC(),
	}

	for _, p := range []*core.Process{interrupted, created} {
		if err := h.store.CreateProcess(ctx, p); err != nil {
			t.Fatalf("CreateProcess() returned %v, want nil", err)
		}
	}

	if err := h.supervisor.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() returned %v, want nil", err)
	}

	gone, err := h.store.GetProcess(ctx, "proc_interrupted")
	if err != nil {
		t.Fatalf("GetProcess() returned %v, want nil", err)
	}

	if gone.State != core.StateFailed || gone.Reason != core.ReasonDaemonRestart {
		t.Errorf("interrupted execution is %s/%s, want FAILED/daemon_restart", gone.State, gone.Reason)
	}

	queued, err := h.store.GetProcess(ctx, "proc_created")
	if err != nil {
		t.Fatalf("GetProcess() returned %v, want nil", err)
	}

	if queued.State != core.StateQueued {
		t.Errorf("accepted execution is %s, want %s", queued.State, core.StateQueued)
	}
}
