package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/curruwilla/processd/internal/config"
	"github.com/curruwilla/processd/internal/core"
	"github.com/curruwilla/processd/internal/runner"
)

// shutdownSlack is how much longer than the grace period Shutdown waits for the
// supervision goroutines to record their outcome after SIGKILL.
const shutdownSlack = 5 * time.Second

// Reconcile rebuilds supervision state after a daemon restart.
//
// Processes started by a previous daemon are not children of this one, so they
// cannot be waited on: their exit code is unrecoverable. Each unfinished
// execution is fingerprinted against /proc, a surviving orphan is handled per
// orphan_policy, and anything else is treated as CRASHED with reason
// daemon_restart (docs/SPEC.md §13.2).
func (s *Supervisor) Reconcile(ctx context.Context) error {
	unfinished, err := s.store.UnfinishedProcesses(ctx)
	if err != nil {
		return err
	}

	recovered := 0

	for _, p := range unfinished {
		changed, err := s.reconcileOne(ctx, p)
		if err != nil {
			s.log.Error("reconciling execution", slog.String("process", p.ID), slog.Any("error", err))
			continue
		}

		if changed {
			recovered++
		}
	}

	if recovered > 0 {
		s.log.Info("reconciled previous state", slog.Int("executions", recovered))
	}

	return nil
}

func (s *Supervisor) reconcileOne(ctx context.Context, p *core.Process) (bool, error) {
	switch p.State {
	case core.StateCreated:
		// Accepted but never scheduled: put it in line.
		if err := p.TransitionTo(core.StateQueued, ""); err != nil {
			return false, err
		}

		queuedAt := time.Now().UTC()
		p.QueuedAt = &queuedAt

		return true, s.store.UpdateProcess(ctx, p)
	case core.StateQueued, core.StateRetrying:
		// The scheduler picks these up on its own.
		return false, nil
	case core.StateStarting, core.StateRunning, core.StateStopping:
		return true, s.recoverInterrupted(ctx, p)
	default:
		return false, nil
	}
}

// recoverInterrupted decides what to do with an execution that was running when
// the daemon went away.
func (s *Supervisor) recoverInterrupted(ctx context.Context, p *core.Process) error {
	worker := s.worker(p.Worker)

	if runner.SameProcess(p.PID, p.PIDStartTime) {
		if err := s.handleOrphan(ctx, p); err != nil {
			return err
		}
	}

	// An orphan left alive is never retried: running the work twice is worse
	// than not running it at all.
	if p.State.IsTerminal() {
		return s.store.UpdateProcess(ctx, p)
	}

	finishedAt := time.Now().UTC()
	p.FinishedAt = &finishedAt

	if err := p.TransitionTo(core.StateCrashed, core.ReasonDaemonRestart); err != nil {
		return err
	}

	s.settle(ctx, p, worker, config.RetryOnNonZeroExit, core.ReasonDaemonRestart)

	return s.store.UpdateProcess(ctx, p)
}

// handleOrphan applies orphan_policy to a process that outlived the daemon.
func (s *Supervisor) handleOrphan(ctx context.Context, p *core.Process) error {
	if s.cfg.OrphanPolicy == config.OrphanPolicyLeave {
		s.log.Warn("leaving orphaned process running",
			slog.String("process", p.ID),
			slog.Int("pid", p.PID),
		)

		finishedAt := time.Now().UTC()
		p.FinishedAt = &finishedAt

		if err := p.TransitionTo(core.StateCrashed, core.ReasonDaemonRestart); err != nil {
			return err
		}

		if err := p.TransitionTo(core.StateFailed, core.ReasonOrphaned); err != nil {
			return err
		}

		s.releaseLock(ctx, p)

		return nil
	}

	handle, err := runner.Adopt(p.PID, p.PIDStartTime)
	if err != nil {
		// The process died between the fingerprint check and here, which is the
		// plain crash path and not a failure of reconciliation.
		//nolint:nilerr // see comment above
		return nil
	}

	s.log.Warn("killing orphaned process",
		slog.String("process", p.ID),
		slog.Int("pid", p.PID),
	)

	if err := s.runner.Stop(ctx, handle, killGrace(s.worker(p.Worker))); err != nil {
		return fmt.Errorf("stopping orphan of %s: %w", p.ID, err)
	}

	return nil
}

// Shutdown asks every running execution to stop, waits for the grace period,
// then lets the runner escalate to SIGKILL.
func (s *Supervisor) Shutdown(ctx context.Context, grace time.Duration) error {
	s.mu.Lock()
	s.draining = true

	pending := make([]*execution, 0, len(s.running))
	for _, exec := range s.running {
		pending = append(pending, exec)
	}

	s.mu.Unlock()

	if len(pending) == 0 {
		return nil
	}

	s.log.Info("stopping executions", slog.Int("count", len(pending)))

	for _, exec := range pending {
		if !exec.setIntent(core.ReasonShutdown) {
			continue
		}

		go s.beginStop(ctx, exec, core.ReasonShutdown, grace)
	}

	done := make(chan struct{})

	go func() {
		s.attempts.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-time.After(grace + shutdownSlack):
		return errors.New("timed out waiting for executions to stop")
	}
}
