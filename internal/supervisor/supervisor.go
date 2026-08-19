// Package supervisor runs a single execution: it starts the process, captures
// its output, waits for it to end and decides what the outcome means.
package supervisor

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/curruwilla/processd/internal/config"
	"github.com/curruwilla/processd/internal/core"
	"github.com/curruwilla/processd/internal/logstore"
	"github.com/curruwilla/processd/internal/runner"
	"github.com/curruwilla/processd/internal/store"
)

var errNotImplemented = errors.New("not implemented")

// Supervisor owns the lifecycle of running executions.
type Supervisor struct {
	cfg    config.Config
	store  store.Store
	runner runner.Runner
	logs   *logstore.Store
	log    *slog.Logger
}

// New wires a supervisor.
func New(
	cfg config.Config,
	st store.Store,
	run runner.Runner,
	logs *logstore.Store,
	log *slog.Logger,
) *Supervisor {
	return &Supervisor{cfg: cfg, store: st, runner: run, logs: logs, log: log}
}

// Start runs one attempt of an execution and supervises it until it reaches a
// terminal state or is scheduled for a retry.
//
// The sequence is fixed: open the attempt logs, start the process group, record
// (pid, pid_start_time), wait, classify the outcome, persist, and either finish
// or hand the execution back to the scheduler for a retry.
func (s *Supervisor) Start(ctx context.Context, p *core.Process) error {
	// TODO(spec §9, §12): implement start, wait, classify and retry handoff.
	return errNotImplemented
}

// Signal forwards a signal from the allowlist to a running execution.
func (s *Supervisor) Signal(ctx context.Context, id, signal string) error {
	// TODO(spec §6.7): resolve the handle and signal the process group.
	return errNotImplemented
}

// Stop terminates a running execution gracefully, escalating to SIGKILL after
// grace. A user-requested stop never triggers a retry.
func (s *Supervisor) Stop(ctx context.Context, id string, grace time.Duration) error {
	// TODO(spec §6.6): transition to STOPPING, signal the group, then CANCELED.
	return errNotImplemented
}

// Reconcile rebuilds supervision state after a daemon restart.
//
// Processes started by a previous daemon are not children of this one: they
// cannot be waited on. Each unfinished execution is therefore fingerprinted
// against /proc; a surviving orphan is handled per orphan_policy, and anything
// else is treated as CRASHED with reason daemon_restart (docs/SPEC.md §13.2).
func (s *Supervisor) Reconcile(ctx context.Context) error {
	// TODO(spec §13.2): load unfinished executions, verify (pid, start time),
	// apply orphan_policy, then requeue or fail them.
	//
	// Until executions can start, there is nothing to reconcile, so this is a
	// no-op rather than an error: the daemon lifecycle must stay correct.
	s.log.Warn("state reconciliation is not implemented yet")

	return nil
}

// Shutdown asks every running execution to stop, waits for grace, then kills
// what is left.
func (s *Supervisor) Shutdown(ctx context.Context, grace time.Duration) error {
	// TODO(spec §11): SIGTERM every process group, wait for grace, SIGKILL the
	// rest, then mark the executions CANCELED with reason shutdown.
	//
	// No execution can be running yet, so this is a no-op rather than an error:
	// a clean SIGTERM must still exit zero.
	s.log.Warn("execution shutdown is not implemented yet")

	return nil
}
