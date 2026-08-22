package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/curruwilla/processd/internal/config"
	"github.com/curruwilla/processd/internal/core"
	"github.com/curruwilla/processd/internal/store"
)

const (
	// dispatchInterval is the fallback tick: most dispatches are triggered by
	// Notify, this only catches retries whose backoff elapsed with nothing else
	// happening on the node.
	dispatchInterval = time.Second

	// coalesceWindow batches the wake-ups that arrive together.
	//
	// Every finishing attempt wakes the loop, and every pass queries the pending
	// set. Under load that turns one completion into one full scan; waiting a
	// moment first collapses a burst of completions into a single pass, at the
	// cost of starting queued work a few milliseconds later.
	coalesceWindow = 25 * time.Millisecond
)

// Starter runs one attempt of an execution and supervises it.
type Starter interface {
	Start(ctx context.Context, p *core.Process) error
}

// Scheduler admits executions, holds them in the queue and dispatches them as
// slots and locks free up.
type Scheduler struct {
	cfg     config.Config
	store   store.Store
	slots   *Slots
	starter Starter
	log     *slog.Logger

	registry atomic.Pointer[config.Registry]
	draining atomic.Bool

	// wake nudges the dispatch loop when something may have become eligible.
	wake chan struct{}
}

// New wires a scheduler. Dependencies are passed explicitly: the daemon builds
// the object graph by hand in one place.
func New(
	cfg config.Config,
	st store.Store,
	registry *config.Registry,
	starter Starter,
	log *slog.Logger,
) *Scheduler {
	s := &Scheduler{
		cfg:     cfg,
		store:   st,
		slots:   NewSlots(cfg.MaxProcesses),
		starter: starter,
		log:     log,
		wake:    make(chan struct{}, 1),
	}

	s.SetRegistry(registry)

	return s
}

// SetRegistry swaps the worker registry after a reload. Running executions keep
// the definition they were created with.
func (s *Scheduler) SetRegistry(registry *config.Registry) {
	s.registry.Store(registry)

	for _, worker := range registry.All() {
		s.slots.SetWorkerLimit(worker.Name, worker.MaxProcesses)
	}
}

// Registry returns the worker registry currently in effect.
func (s *Scheduler) Registry() *config.Registry { return s.registry.Load() }

// Slots exposes concurrency accounting for the stats endpoint.
func (s *Scheduler) Slots() *Slots { return s.slots }

// Submit persists a new execution and either starts it immediately or queues
// it. It reports core.ErrQueueFull when the queue is at its bound and
// core.ErrShuttingDown while the daemon is draining.
func (s *Scheduler) Submit(ctx context.Context, p *core.Process) error {
	if s.draining.Load() {
		return core.ErrShuttingDown
	}

	// A service never waits in line (docs/SPEC.md §4), so how deep the queue is
	// says nothing about whether it may start.
	if p.Type != core.TypeService {
		depth, err := s.queueDepth(ctx)
		if err != nil {
			return err
		}

		if depth >= s.cfg.Queue.MaxDepth {
			return fmt.Errorf("queue holds %d executions: %w", depth, core.ErrQueueFull)
		}
	}

	if err := s.store.CreateProcess(ctx, p); err != nil {
		return err
	}

	return s.admit(ctx, p)
}

// admit tries to start an execution right away, queueing it when a slot or its
// lock is unavailable.
func (s *Scheduler) admit(ctx context.Context, p *core.Process) error {
	worker := s.worker(p.Worker)

	// A worker configured to reject on lock conflicts must answer immediately
	// rather than sit in the queue, so the lock is claimed before the slot.
	claimed := false

	if p.Lock != "" && lockConflict(worker) == config.LockConflictReject {
		if err := s.store.AcquireLock(ctx, p.Lock, p.ID); err != nil {
			if errors.Is(err, core.ErrLockHeld) {
				return s.rejectLocked(ctx, p, err)
			}

			return err
		}

		claimed = true
	}

	if !s.slots.TryAcquire(p.ID, p.Worker) {
		// A lock belongs to a running attempt, never to a place in line. Keeping
		// the one claimed a moment ago would make every later submission answer
		// 409 against an execution that is not running, and for as long as the
		// node stays full. The dispatch pass claims it again when a slot frees
		// up, and by then it means what it says.
		if claimed {
			s.releaseLock(ctx, p)
		}

		return s.park(ctx, p, fmt.Errorf("%d of %d slots in use: %w", used(s.slots), s.cfg.MaxProcesses, core.ErrNoCapacity))
	}

	if err := s.store.AcquireLock(ctx, p.Lock, p.ID); err != nil {
		s.slots.Release(p.ID)

		if errors.Is(err, core.ErrLockHeld) {
			return s.park(ctx, p, fmt.Errorf("%q: %w", p.Lock, core.ErrNoCapacity))
		}

		return err
	}

	return s.launch(ctx, p)
}

// park holds a task that could not start yet, and refuses a service that could
// not start now.
//
// A service takes its slot at admission or not at all: parking it would leave
// something that is supposed to be running listed as merely waiting, with no
// bound on how long. A task is work to be done eventually, so it queues.
func (s *Scheduler) park(ctx context.Context, p *core.Process, refusal error) error {
	if p.Type == core.TypeService {
		return s.reject(ctx, p, refusal)
	}

	return s.enqueue(ctx, p)
}

// reject records an execution that was refused at admission and reports why.
// The row is kept: an execution that was refused is part of the history.
func (s *Scheduler) reject(ctx context.Context, p *core.Process, cause error) error {
	if err := p.TransitionTo(core.StateCanceled, core.ReasonNoCapacity); err != nil {
		return err
	}

	if err := s.store.UpdateProcess(ctx, p); err != nil {
		return err
	}

	return cause
}

func used(slots *Slots) int {
	inUse, _ := slots.Usage()

	return inUse
}

// rejectLocked records the execution that lost a lock race and reports 409. The
// row is kept: an execution that was refused is part of the history.
func (s *Scheduler) rejectLocked(ctx context.Context, p *core.Process, cause error) error {
	if err := p.TransitionTo(core.StateCanceled, core.ReasonLockConflict); err != nil {
		return err
	}

	if err := s.store.UpdateProcess(ctx, p); err != nil {
		return err
	}

	return cause
}

// enqueue parks an execution until a slot and its lock are both free.
func (s *Scheduler) enqueue(ctx context.Context, p *core.Process) error {
	if err := p.TransitionTo(core.StateQueued, ""); err != nil {
		return err
	}

	queuedAt := time.Now().UTC()
	p.QueuedAt = &queuedAt

	return s.store.UpdateProcess(ctx, p)
}

// launch starts the next attempt of an execution that already holds its slot
// and lock.
func (s *Scheduler) launch(ctx context.Context, p *core.Process) error {
	p.Attempt++
	p.ClearAttempt()
	p.RetryAt = nil

	if err := p.TransitionTo(core.StateStarting, ""); err != nil {
		s.releaseAll(ctx, p)
		return err
	}

	if err := s.store.UpdateProcess(ctx, p); err != nil {
		s.releaseAll(ctx, p)
		return err
	}

	if err := s.starter.Start(ctx, p); err != nil {
		s.releaseAll(ctx, p)
		return err
	}

	return nil
}

// releaseAll gives back what launch had reserved, so a failure to start never
// leaks a slot or a lock.
func (s *Scheduler) releaseAll(ctx context.Context, p *core.Process) {
	s.slots.Release(p.ID)
	s.releaseLock(ctx, p)
}

// releaseLock frees the lock an execution holds, reporting a failure rather
// than returning it: the caller is already on its way somewhere else.
func (s *Scheduler) releaseLock(ctx context.Context, p *core.Process) {
	if err := s.store.ReleaseLock(ctx, p.Lock, p.ID); err != nil {
		s.log.Error("releasing lock", slog.String("process", p.ID), slog.Any("error", err))
	}
}

// Run drives the dispatch loop until the context is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	ticker := time.NewTicker(dispatchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.dispatch(ctx)
		case <-s.wake:
			s.coalesce(ctx)
			s.dispatch(ctx)
		}
	}
}

// coalesce absorbs the wake-ups that arrive within the batching window, so a
// burst of completions costs one dispatch pass instead of one per completion.
func (s *Scheduler) coalesce(ctx context.Context) {
	timer := time.NewTimer(coalesceWindow)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-timer.C:
			return
		}
	}
}

// Notify asks the dispatch loop to look for newly eligible work.
func (s *Scheduler) Notify() {
	select {
	case s.wake <- struct{}{}:
	default: // a pass is already pending
	}
}

// OnExecutionSettled frees the slot an execution held and looks for more work.
//
// A service on its way to a restart keeps its slot: it was admitted only
// because the node had room for it, and letting a task take that room during
// the backoff would leave the service unable to come back. Every other outcome,
// including a service stopped mid-backoff, gives the slot back.
func (s *Scheduler) OnExecutionSettled(p *core.Process) {
	if p.Type == core.TypeService && p.State == core.StateRetrying {
		s.Notify()
		return
	}

	s.slots.Release(p.ID)
	s.Notify()
}

// dispatch starts every pending execution that has become eligible.
//
// It scans for eligible items rather than only the head of the queue: an item
// blocked by its lock or by its worker limit must not stall everything behind
// it.
func (s *Scheduler) dispatch(ctx context.Context) {
	if s.draining.Load() {
		return
	}

	pending, err := s.store.PendingProcesses(ctx, time.Now().UTC())
	if err != nil {
		s.log.Error("reading pending executions", slog.Any("error", err))
		return
	}

	for _, p := range pending {
		// Expiry runs even when the node is full: an execution that waited past
		// its TTL must be failed whether or not a slot ever frees up.
		if s.expire(ctx, p) {
			continue
		}

		if !s.slots.TryAcquire(p.ID, p.Worker) {
			continue
		}

		if err := s.store.AcquireLock(ctx, p.Lock, p.ID); err != nil {
			s.slots.Release(p.ID)

			if !errors.Is(err, core.ErrLockHeld) {
				s.log.Error("acquiring lock", slog.String("process", p.ID), slog.Any("error", err))
			}

			continue
		}

		if err := s.launch(ctx, p); err != nil {
			s.log.Error("starting execution", slog.String("process", p.ID), slog.Any("error", err))
		}
	}
}

// expire fails an execution that waited in the queue longer than item_ttl.
func (s *Scheduler) expire(ctx context.Context, p *core.Process) bool {
	ttl := s.cfg.Queue.ItemTTL.Duration()
	if ttl <= 0 || p.State != core.StateQueued {
		return false
	}

	// A service only passes through the queue on its way back from a daemon
	// restart it was told to survive; failing it for having waited would undo
	// exactly what retry.on_shutdown asked for.
	if p.Type == core.TypeService {
		return false
	}

	waiting := p.CreatedAt
	if p.QueuedAt != nil {
		waiting = *p.QueuedAt
	}

	if time.Since(waiting) <= ttl {
		return false
	}

	if err := p.TransitionTo(core.StateFailed, core.ReasonQueueTimeout); err != nil {
		s.log.Error("expiring queued execution", slog.String("process", p.ID), slog.Any("error", err))
		return false
	}

	s.releaseLock(ctx, p)

	if err := s.store.UpdateProcess(ctx, p); err != nil {
		s.log.Error("expiring queued execution", slog.String("process", p.ID), slog.Any("error", err))
	}

	s.log.Warn("execution expired in queue", slog.String("process", p.ID), slog.Duration("ttl", ttl))

	return true
}

// queueDepth counts the executions waiting for a slot.
func (s *Scheduler) queueDepth(ctx context.Context) (int, error) {
	return s.store.PendingCount(ctx)
}

// Drain stops admitting and dispatching work. Queued executions stay queued and
// are picked up again after a restart.
func (s *Scheduler) Drain() { s.draining.Store(true) }

func (s *Scheduler) worker(name string) *config.Worker {
	if name == "" {
		return nil
	}

	worker, err := s.Registry().Get(name)
	if err != nil {
		return nil
	}

	return worker
}

func lockConflict(worker *config.Worker) config.LockConflict {
	if worker == nil || worker.LockConflict == "" {
		return config.LockConflictQueue
	}

	return worker.LockConflict
}
