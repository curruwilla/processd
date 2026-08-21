package supervisor

import (
	"context"
	"log/slog"
	"time"

	"github.com/curruwilla/processd/internal/core"
)

// Stop terminates an execution gracefully, escalating to SIGKILL after grace.
// A user-requested stop never triggers a retry.
func (s *Supervisor) Stop(ctx context.Context, id string, grace time.Duration) error {
	exec := s.lookup(id)
	if exec != nil {
		if !exec.setIntent(core.ReasonUserRequest) {
			// Already stopping for another reason; the first one wins.
			return nil
		}

		if grace <= 0 {
			grace = exec.grace
		}

		s.beginStop(ctx, exec, core.ReasonUserRequest, grace)

		return nil
	}

	return s.cancelPending(ctx, id)
}

// cancelPending cancels an execution that has not started yet. Queued work is
// cancellable: it holds no process, only a place in line.
func (s *Supervisor) cancelPending(ctx context.Context, id string) error {
	p, err := s.store.GetProcess(ctx, id)
	if err != nil {
		return err
	}

	switch p.State {
	case core.StateCreated, core.StateQueued, core.StateRetrying:
	default:
		return errNotRunning(id)
	}

	if err := p.TransitionTo(core.StateCanceled, core.ReasonUserRequest); err != nil {
		return err
	}

	s.releaseLock(ctx, p)

	if err := s.store.UpdateProcess(ctx, p); err != nil {
		return err
	}

	// A queued execution holds no slot, but a service stopped during its backoff
	// does: the slot belongs to the execution, not to one of its attempts. The
	// release is idempotent, so both cases take the same path.
	s.onSettle(p)

	return nil
}

// Signal forwards a signal from the allowlist to a running execution. It always
// reaches the whole process group.
func (s *Supervisor) Signal(ctx context.Context, id, signal string) error {
	exec := s.lookup(id)
	if exec == nil {
		if _, err := s.store.GetProcess(ctx, id); err != nil {
			return err
		}

		return errNotRunning(id)
	}

	return s.runner.Signal(exec.handle, signal)
}

// beginStop moves the execution to STOPPING and asks the process group to end.
func (s *Supervisor) beginStop(ctx context.Context, exec *execution, reason core.Reason, grace time.Duration) {
	exec.mu.Lock()

	p := exec.process
	if p.State == core.StateRunning {
		if err := p.TransitionTo(core.StateStopping, reason); err != nil {
			s.log.Error("invalid state transition", slog.String("process", p.ID), slog.Any("error", err))
		}
	}

	// The persist reads the execution, so it stays inside the lock; only the
	// stop itself, which blocks for the whole grace period, happens outside.
	err := s.store.UpdateProcess(ctx, p)

	exec.mu.Unlock()

	if err != nil {
		s.log.Error("persisting stop request", slog.String("process", p.ID), slog.Any("error", err))
	}

	if err := s.runner.Stop(ctx, exec.handle, grace); err != nil {
		s.log.Error("stopping execution", slog.String("process", p.ID), slog.Any("error", err))
	}
}

func (s *Supervisor) lookup(id string) *execution {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.running[id]
}
