package supervisor

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync"
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
  - name: fatal-task
    command: /bin/false
    cwd: /tmp
    retry:
      enabled: true
      max_attempts: 3
      no_retry_exit_codes: [1]
      backoff: {type: fixed, initial: 10ms, max: 10ms, jitter: 0}
  - name: sleeper
    command: /bin/sleep
    args: ["30"]
    cwd: /tmp
    kill_grace: 1s
  - name: api
    type: service
    command: /bin/true
    cwd: /tmp
    retry:
      backoff: {type: fixed, initial: 10ms, max: 10ms, jitter: 0}
    logs:
      rotate:
        max_files: 2
  - name: narrow-api
    type: service
    command: /bin/true
    cwd: /tmp
    retry:
      retry_on: [exit]
      backoff: {type: fixed, initial: 10ms, max: 10ms, jitter: 0}
    logs:
      rotate:
        max_files: 2
  - name: surviving-api
    type: service
    command: /bin/sleep
    args: ["30"]
    cwd: /tmp
    kill_grace: 1s
    retry:
      on_shutdown: true
      backoff: {type: fixed, initial: 10ms, max: 10ms, jitter: 0}
    logs:
      rotate:
        max_files: 2
  - name: fatal-api
    type: service
    command: /bin/false
    cwd: /tmp
    retry:
      no_retry_exit_codes: [1]
      backoff: {type: fixed, initial: 10ms, max: 10ms, jitter: 0}
    logs:
      rotate:
        max_files: 2
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
	sup.SetOnSettle(func(p *core.Process) { h.finished <- p })

	return h
}

// startService persists a service in STARTING, the state the scheduler hands
// over, with the unlimited ceiling its worker resolves to.
func (h *harness) startService(t *testing.T, id, worker, command string, args []string) *core.Process {
	t.Helper()

	p := h.build(id, worker, command, args)
	p.Type = core.TypeService
	p.MaxAttempts = config.AttemptsUnlimited

	h.launch(t, p)

	return p
}

// start persists an execution in STARTING, the state the scheduler hands over.
func (h *harness) start(t *testing.T, id, worker, command string, args []string) *core.Process {
	t.Helper()

	p := h.build(id, worker, command, args)
	h.launch(t, p)

	return p
}

func (h *harness) launch(t *testing.T, p *core.Process) {
	t.Helper()

	if err := h.store.CreateProcess(t.Context(), p); err != nil {
		t.Fatalf("CreateProcess() returned %v, want nil", err)
	}

	if err := h.supervisor.Start(t.Context(), p); err != nil {
		t.Fatalf("Start() returned %v, want nil", err)
	}
}

func (h *harness) build(id, worker, command string, args []string) *core.Process {
	return &core.Process{
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

	changed := make(chan *core.Process, 1)

	h.supervisor.SetOnSettle(func(p *core.Process) { changed <- p })

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

// For a service there is no successful exit, only an exit (docs/SPEC.md §4).
// The same command that finishes a task has to restart a service.
func TestSupervisor_Start_serviceExitIsAbnormal(t *testing.T) {
	t.Parallel()

	t.Run("a clean exit becomes a restart", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t, nil)
		h.startService(t, "svc_1", "api", "/bin/true", nil)

		finished := h.awaitFinish(t)

		if finished.State != core.StateRetrying {
			t.Errorf("state = %s/%s, want %s", finished.State, finished.Reason, core.StateRetrying)
		}

		if finished.ExitCode == nil || *finished.ExitCode != 0 {
			t.Errorf("exit_code = %v, want 0", finished.ExitCode)
		}

		if finished.RetryAt == nil {
			t.Error("retry_at is nil, want the restart to be scheduled")
		}
	})

	t.Run("the same exit still completes a task", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t, nil)
		h.start(t, "proc_1", "hello", "/bin/true", nil)

		finished := h.awaitFinish(t)

		if finished.State != core.StateCompleted {
			t.Errorf("state = %s, want %s", finished.State, core.StateCompleted)
		}
	})

	t.Run("no_retry_exit_codes still stops a service", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t, nil)
		h.startService(t, "svc_1", "fatal-api", "/bin/false", nil)

		finished := h.awaitFinish(t)

		if finished.State != core.StateFailed || finished.Reason != core.ReasonNoRetryExit {
			t.Errorf("state = %s/%s, want FAILED/no_retry_exit_code", finished.State, finished.Reason)
		}
	})

	t.Run("a restart is counted", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t, nil)
		observed := &countingMetrics{}
		h.supervisor.SetMetrics(observed)

		h.startService(t, "svc_1", "api", "/bin/true", nil)
		h.awaitFinish(t)

		if got := observed.restarts(); got != 1 {
			t.Errorf("ServiceRestarted called %d times, want 1", got)
		}
	})

	// An unlimited ceiling has to survive an attempt counter well past what any
	// task would be allowed.
	t.Run("restarts past any task ceiling", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t, nil)

		p := h.build("svc_1", "api", "/bin/true", nil)
		p.Type = core.TypeService
		p.MaxAttempts = config.AttemptsUnlimited
		p.Attempt = 10_000

		h.launch(t, p)

		finished := h.awaitFinish(t)

		if finished.State != core.StateRetrying {
			t.Errorf("state = %s/%s, want %s", finished.State, finished.Reason, core.StateRetrying)
		}
	})
}

// countingMetrics records only what the service tests assert on.
type countingMetrics struct {
	mu        sync.Mutex
	restarted int
}

func (m *countingMetrics) AttemptStarted(string)                         {}
func (m *countingMetrics) AttemptFinished(string, string, time.Duration) {}

func (m *countingMetrics) ServiceRestarted(string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.restarted++
}

func (m *countingMetrics) restarts() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.restarted
}

// The fatal-exit path reaches FAILED through CRASHED for a task too: a running
// process that ends badly has crashed, and RUNNING -> FAILED is not an edge the
// state machine defines.
func TestSupervisor_Start_fatalExitCode(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.start(t, "proc_1", "fatal-task", "/bin/false", nil)

	finished := h.awaitFinish(t)

	if finished.State != core.StateFailed || finished.Reason != core.ReasonNoRetryExit {
		t.Errorf("state = %s/%s, want FAILED/no_retry_exit_code", finished.State, finished.Reason)
	}
}

// A service whose process did not outlive the daemon comes back, even when its
// policy was narrowed to the trigger only a service can declare.
func TestSupervisor_Reconcile_service(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	ctx := t.Context()

	interrupted := &core.Process{
		ID:           "svc_interrupted",
		Worker:       "narrow-api",
		Type:         core.TypeService,
		State:        core.StateRunning,
		Command:      "/bin/true",
		Cwd:          "/tmp",
		Attempt:      1,
		MaxAttempts:  config.AttemptsUnlimited,
		PID:          os.Getpid(),
		PIDStartTime: 1,
		CreatedAt:    time.Now().UTC(),
	}

	if err := h.store.CreateProcess(ctx, interrupted); err != nil {
		t.Fatalf("CreateProcess() returned %v, want nil", err)
	}

	if err := h.supervisor.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() returned %v, want nil", err)
	}

	recovered, err := h.store.GetProcess(ctx, "svc_interrupted")
	if err != nil {
		t.Fatalf("GetProcess() returned %v, want nil", err)
	}

	if recovered.State != core.StateRetrying {
		t.Errorf("interrupted service is %s/%s, want %s", recovered.State, recovered.Reason, core.StateRetrying)
	}
}

// retry.on_shutdown puts a service back in the queue so the next daemon start
// picks it up, which is the one path where a service is legitimately QUEUED.
func TestSupervisor_Shutdown_service(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.startService(t, "svc_1", "surviving-api", "/bin/sleep", []string{"30"})

	if err := h.supervisor.Shutdown(t.Context(), 500*time.Millisecond); err != nil {
		t.Fatalf("Shutdown() returned %v, want nil", err)
	}

	finished := h.awaitFinish(t)

	if finished.State != core.StateQueued || finished.Reason != core.ReasonShutdown {
		t.Errorf("state = %s/%s, want QUEUED/shutdown", finished.State, finished.Reason)
	}
}

// Stopping a service while it waits out a backoff has to reach the scheduler
// with the settled execution: it is holding a slot that nothing else will free.
func TestSupervisor_Stop_retryingService(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)

	settled := make(chan *core.Process, 1)

	h.supervisor.SetOnSettle(func(p *core.Process) { settled <- p })

	p := &core.Process{
		ID:          "svc_1",
		Worker:      "api",
		Type:        core.TypeService,
		State:       core.StateRetrying,
		Command:     "/bin/true",
		Cwd:         "/tmp",
		Attempt:     3,
		MaxAttempts: config.AttemptsUnlimited,
		CreatedAt:   time.Now().UTC(),
	}

	if err := h.store.CreateProcess(t.Context(), p); err != nil {
		t.Fatalf("CreateProcess() returned %v, want nil", err)
	}

	if err := h.supervisor.Stop(t.Context(), "svc_1", time.Second); err != nil {
		t.Fatalf("Stop() returned %v, want nil", err)
	}

	select {
	case reported := <-settled:
		if reported.State != core.StateCanceled {
			t.Errorf("settled execution is %s, want %s", reported.State, core.StateCanceled)
		}
	case <-time.After(time.Second):
		t.Fatal("the scheduler was never told the service settled")
	}
}
