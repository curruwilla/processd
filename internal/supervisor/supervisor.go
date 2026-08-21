// Package supervisor runs single executions: it starts the process, captures
// its output, waits for it to end and decides what the outcome means.
package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/curruwilla/processd/internal/config"
	"github.com/curruwilla/processd/internal/core"
	"github.com/curruwilla/processd/internal/logstore"
	"github.com/curruwilla/processd/internal/retry"
	"github.com/curruwilla/processd/internal/runner"
	"github.com/curruwilla/processd/internal/store"
)

// defaultKillGrace applies when a worker sets none.
const defaultKillGrace = 15 * time.Second

// execution is one running attempt and everything needed to control it.
type execution struct {
	process *core.Process
	handle  *runner.Handle
	logs    *logstore.Attempt
	worker  *config.Worker
	grace   time.Duration

	mu sync.Mutex
	// intent records why the daemon asked the process to stop, so that the exit
	// can be classified as a cancellation rather than a crash.
	intent core.Reason
}

func (e *execution) setIntent(reason core.Reason) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.intent != "" {
		return false
	}

	e.intent = reason

	return true
}

// Metrics records what each attempt did. Only the supervisor follows an
// attempt from its start to its outcome, so the counters are fed from here.
type Metrics interface {
	AttemptStarted(worker string)
	AttemptFinished(worker, state string, elapsed time.Duration)
	ServiceRestarted(worker string)
}

// nopMetrics is the default observer: the supervisor runs the same with or
// without one attached.
type nopMetrics struct{}

func (nopMetrics) AttemptStarted(string)                         {}
func (nopMetrics) AttemptFinished(string, string, time.Duration) {}
func (nopMetrics) ServiceRestarted(string)                       {}

// Supervisor owns the lifecycle of running executions.
type Supervisor struct {
	cfg     config.Config
	store   store.Store
	runner  runner.Runner
	logs    *logstore.Store
	metrics Metrics
	log     *slog.Logger

	mu       sync.Mutex
	running  map[string]*execution
	draining bool

	// attempts tracks the supervision goroutines, so shutdown can wait for them.
	attempts sync.WaitGroup

	// workers resolves the current worker definitions. Retry and log policy are
	// read at attempt time; the command line itself is frozen on the execution.
	workers func() *config.Registry
	// onSettle hands an execution back to the scheduler whenever it stops
	// occupying the node, so the slot it held is released and the next eligible
	// item is looked at.
	onSettle func(*core.Process)
}

// New wires a supervisor.
func New(
	cfg config.Config,
	st store.Store,
	run runner.Runner,
	logs *logstore.Store,
	log *slog.Logger,
) *Supervisor {
	return &Supervisor{
		cfg:      cfg,
		store:    st,
		runner:   run,
		logs:     logs,
		metrics:  nopMetrics{},
		log:      log,
		running:  map[string]*execution{},
		workers:  func() *config.Registry { return nil },
		onSettle: func(*core.Process) {},
	}
}

// SetWorkers injects the worker registry lookup.
func (s *Supervisor) SetWorkers(workers func() *config.Registry) { s.workers = workers }

// SetMetrics injects the observer fed by every attempt.
func (s *Supervisor) SetMetrics(m Metrics) { s.metrics = m }

// SetOnSettle injects the callback invoked whenever an execution stops
// occupying the node, whatever the outcome. The scheduler uses it to free the
// slot and look for new work.
func (s *Supervisor) SetOnSettle(fn func(*core.Process)) { s.onSettle = fn }

// Running reports how many attempts are under supervision.
func (s *Supervisor) Running() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.running)
}

// Start runs one attempt of an execution and supervises it in the background.
//
// The caller has already moved the execution to STARTING and holds its slot and
// lock. Start returns as soon as the process is running: the outcome arrives
// through the onSettle callback.
func (s *Supervisor) Start(ctx context.Context, caller *core.Process) error {
	s.mu.Lock()
	draining := s.draining
	s.mu.Unlock()

	if draining {
		return core.ErrShuttingDown
	}

	// The caller keeps its own copy: from here on the execution is supervised
	// from another goroutine, and sharing the struct would be a data race.
	p := caller.Clone()
	worker := s.worker(p.Worker)

	attemptLogs, err := s.logs.Create(p.ID, p.Attempt, p.CreatedAt, logPolicy(worker))
	if err != nil {
		return s.startFailed(ctx, p, worker, err)
	}

	handle, err := s.runner.Start(ctx, runner.Spec{
		Command:        p.Command,
		Args:           p.Args,
		Cwd:            p.Cwd,
		Env:            p.Env,
		EnvPassthrough: passthrough(worker),
		User:           p.User,
		Group:          p.Group,
		Stdout:         attemptLogs.Stdout,
		Stderr:         attemptLogs.Stderr,
		AllowRoot:      s.cfg.AllowRootProcesses,
	})
	if err != nil {
		_ = attemptLogs.Close()
		return s.startFailed(ctx, p, worker, err)
	}

	startedAt := time.Now().UTC()
	p.PID = handle.PID
	p.PIDStartTime = handle.PIDStartTime
	p.StartedAt = &startedAt

	if err := p.TransitionTo(core.StateRunning, ""); err != nil {
		return err
	}

	if err := s.store.UpdateProcess(ctx, p); err != nil {
		return err
	}

	exec := &execution{
		process: p,
		handle:  handle,
		logs:    attemptLogs,
		worker:  worker,
		grace:   killGrace(worker),
	}

	s.mu.Lock()
	s.running[p.ID] = exec
	s.mu.Unlock()

	s.metrics.AttemptStarted(p.Worker)

	s.log.Info("execution started",
		slog.String("process", p.ID),
		slog.String("worker", p.Worker),
		slog.Int("attempt", p.Attempt),
		slog.Int("pid", p.PID),
	)

	s.attempts.Add(1)

	// The attempt outlives the request that created it, so the supervision
	// goroutine deliberately does not inherit the caller's context.
	//nolint:gosec // see comment above
	go s.supervise(exec)

	return nil
}

// startFailed records an attempt that never became a process.
func (s *Supervisor) startFailed(ctx context.Context, p *core.Process, worker *config.Worker, cause error) error {
	s.log.Error("starting execution",
		slog.String("process", p.ID),
		slog.String("worker", p.Worker),
		slog.Any("error", cause),
	)

	finishedAt := time.Now().UTC()
	p.FinishedAt = &finishedAt

	if err := p.TransitionTo(core.StateCrashed, core.ReasonStartError); err != nil {
		return err
	}

	s.settle(ctx, p, worker, config.RetryOnStartError, core.ReasonStartError)

	if err := s.store.UpdateProcess(ctx, p); err != nil {
		return err
	}

	// An attempt that never became a process has an outcome but no duration.
	s.metrics.AttemptFinished(p.Worker, string(p.State), 0)
	s.onSettle(p)

	return nil
}

// supervise waits for the attempt to end and records the outcome.
func (s *Supervisor) supervise(exec *execution) {
	defer s.attempts.Done()

	// The attempt outlives the request that created it, so it never inherits a
	// request context: only Stop and Shutdown may end it early.
	ctx := context.Background()
	p := exec.process

	timer := s.armTimeout(ctx, exec)

	result, waitErr := s.runner.Wait(ctx, exec.handle)

	if timer != nil {
		timer.Stop()
	}

	if err := exec.logs.Close(); err != nil {
		s.log.Error("closing attempt logs", slog.String("process", p.ID), slog.Any("error", err))
	}

	s.mu.Lock()
	delete(s.running, p.ID)
	s.mu.Unlock()

	if waitErr != nil {
		s.log.Error("waiting for execution", slog.String("process", p.ID), slog.Any("error", waitErr))
	}

	s.finish(ctx, exec, result)
}

// armTimeout stops the process group once the execution's timeout elapses.
func (s *Supervisor) armTimeout(ctx context.Context, exec *execution) *time.Timer {
	timeout := exec.process.Timeout
	if timeout <= 0 {
		return nil
	}

	return time.AfterFunc(timeout, func() {
		if !exec.setIntent(core.ReasonTimeout) {
			return
		}

		s.log.Warn("execution timed out",
			slog.String("process", exec.process.ID),
			slog.Duration("timeout", timeout),
		)

		s.beginStop(ctx, exec, core.ReasonTimeout, exec.grace)
	})
}

// finish classifies the outcome, persists it and hands the execution back.
func (s *Supervisor) finish(ctx context.Context, exec *execution, result runner.Result) {
	p := exec.process

	exec.mu.Lock()

	finishedAt := time.Now().UTC()
	exitCode := result.ExitCode
	p.FinishedAt = &finishedAt
	p.ExitCode = &exitCode
	p.Signal = result.Signal
	p.LogTruncated = exec.logs.Truncated()

	s.classify(ctx, exec, exec.intent, result)

	err := s.store.UpdateProcess(ctx, p)

	exec.mu.Unlock()

	if err != nil {
		s.log.Error("persisting execution outcome", slog.String("process", p.ID), slog.Any("error", err))
	}

	s.metrics.AttemptFinished(p.Worker, string(p.State), p.Duration())

	s.log.Info("execution finished",
		slog.String("process", p.ID),
		slog.String("state", string(p.State)),
		slog.String("reason", string(p.Reason)),
		slog.Int("attempt", p.Attempt),
		slog.Int("exit_code", result.ExitCode),
		slog.String("signal", result.Signal),
	)

	s.onSettle(p)
}

// classify moves the execution into the state its outcome implies.
func (s *Supervisor) classify(ctx context.Context, exec *execution, intent core.Reason, result runner.Result) {
	p := exec.process
	policy := retryPolicy(exec.worker)

	// An intent is recorded before beginStop applies it, and the process may end
	// on its own in between — a service exiting exactly as the daemon starts
	// draining, for instance. The execution is then still RUNNING here, so every
	// intent-driven outcome is routed through STOPPING rather than jumping to a
	// state the machine does not connect to RUNNING.
	if intent != "" && p.State == core.StateRunning {
		s.transition(p, core.StateStopping, intent)
	}

	switch intent {
	case core.ReasonUserRequest:
		s.transition(p, core.StateCanceled, core.ReasonUserRequest)
		s.releaseLock(ctx, p)

		return
	case core.ReasonShutdown:
		s.classifyShutdown(ctx, p, policy)

		return
	case core.ReasonTimeout:
		if retry.Allowed(policy, config.RetryOnTimeout, p.Attempt) {
			s.transition(p, core.StateCrashed, core.ReasonTimeout)
			s.settle(ctx, p, exec.worker, config.RetryOnTimeout, core.ReasonTimeout)

			return
		}

		s.transition(p, core.StateFailed, core.ReasonTimeout)
		s.releaseLock(ctx, p)

		return
	default:
		// Nothing asked this process to stop: classify what it reported.
	}

	// A service that exited has stopped doing its job, whatever it reported on
	// the way out: for it there is no successful exit, only an exit
	// (docs/SPEC.md §4). A task is the opposite, and its success is final.
	if p.Type != core.TypeService && result.Signal == "" && retry.Succeeded(policy, result.ExitCode) {
		s.transition(p, core.StateCompleted, "")
		s.releaseLock(ctx, p)

		return
	}

	// no_retry_exit_codes applies to both: an exit code that says "my
	// configuration is wrong" is not worth restarting into forever.
	//
	// The failure is still reached through CRASHED. A running process that ends
	// badly has crashed, whether or not the policy then allows another attempt,
	// and RUNNING -> FAILED is not an edge the state machine defines.
	if retry.Fatal(policy, result.ExitCode) {
		s.transition(p, core.StateCrashed, "")
		s.transition(p, core.StateFailed, core.ReasonNoRetryExit)
		s.releaseLock(ctx, p)

		return
	}

	s.transition(p, core.StateCrashed, "")
	s.settle(ctx, p, exec.worker, exitTrigger(p.Type, result), core.ReasonMaxAttempts)
}

// exitTrigger names the failure class an ended attempt belongs to.
func exitTrigger(kind core.Type, result runner.Result) config.RetryTrigger {
	if result.Signal != "" {
		return config.RetryOnSignal
	}

	if kind == core.TypeService {
		return config.RetryOnExit
	}

	return config.RetryOnNonZeroExit
}

// classifyShutdown records an execution stopped because the daemon is going
// down. Workers that opt into on_shutdown go back to the queue so the next
// daemon start picks them up.
func (s *Supervisor) classifyShutdown(ctx context.Context, p *core.Process, policy config.Retry) {
	if policy.IsEnabled() && policy.OnShutdown {
		s.transition(p, core.StateQueued, core.ReasonShutdown)

		return
	}

	s.transition(p, core.StateCanceled, core.ReasonShutdown)
	s.releaseLock(ctx, p)
}

// settle turns a CRASHED execution into either a retry or a definitive failure.
// The lock is kept across a retry: releasing it between attempts would let
// another execution steal it mid-backoff.
func (s *Supervisor) settle(
	ctx context.Context,
	p *core.Process,
	worker *config.Worker,
	trigger config.RetryTrigger,
	failureReason core.Reason,
) {
	policy := retryPolicy(worker)

	if p.StartedAt != nil && p.FinishedAt != nil && retry.CounterReset(policy, p.FinishedAt.Sub(*p.StartedAt)) {
		p.Attempt = 0
	}

	if !retry.Allowed(policy, trigger, p.Attempt) {
		s.transition(p, core.StateFailed, failureReason)
		s.releaseLock(ctx, p)

		return
	}

	retryAt := time.Now().UTC().Add(retry.Delay(policy.Backoff, p.Attempt))
	p.RetryAt = &retryAt

	s.transition(p, core.StateRetrying, "")

	if p.Type == core.TypeService {
		s.metrics.ServiceRestarted(p.Worker)
	}
}

// transition applies a state change, logging the ones the state machine does
// not define instead of silently accepting them.
func (s *Supervisor) transition(p *core.Process, to core.State, reason core.Reason) {
	if err := p.TransitionTo(to, reason); err != nil {
		s.log.Error("invalid state transition", slog.String("process", p.ID), slog.Any("error", err))
	}
}

func (s *Supervisor) releaseLock(ctx context.Context, p *core.Process) {
	if p.Lock == "" {
		return
	}

	if err := s.store.ReleaseLock(ctx, p.Lock, p.ID); err != nil {
		s.log.Error("releasing lock", slog.String("process", p.ID), slog.Any("error", err))
	}
}

// worker resolves a worker definition, tolerating one that was removed by a
// reload while an execution was running.
func (s *Supervisor) worker(name string) *config.Worker {
	registry := s.workers()
	if registry == nil || name == "" {
		return nil
	}

	worker, err := registry.Get(name)
	if err != nil {
		return nil
	}

	return worker
}

func retryPolicy(worker *config.Worker) config.Retry {
	if worker == nil {
		return config.Retry{}
	}

	return worker.Retry
}

func passthrough(worker *config.Worker) []string {
	if worker == nil {
		return nil
	}

	return worker.EnvPassthrough
}

func killGrace(worker *config.Worker) time.Duration {
	if worker == nil || worker.KillGrace == 0 {
		return defaultKillGrace
	}

	return worker.KillGrace.Duration()
}

func logPolicy(worker *config.Worker) logstore.Policy {
	if worker == nil {
		return logstore.Policy{}
	}

	return logstore.Policy{
		MaxBytesPerStream: worker.Logs.MaxBytesPerStream.Bytes(),
		MaxFiles:          worker.Logs.Rotate.MaxFiles,
	}
}

// errNotRunning wraps the domain error with the execution id.
func errNotRunning(id string) error {
	return fmt.Errorf("%s: %w", id, core.ErrNotRunning)
}
