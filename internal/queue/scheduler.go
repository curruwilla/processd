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

// Starter runs one execution to completion, including retries.
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

	// TODO(spec §14): check queue depth, persist as CREATED, then try to
	// acquire slot and lock; transition to STARTING or QUEUED accordingly.
	return fmt.Errorf("submitting %s: %w", p.ID, errNotImplemented)
}

// Run drives the dispatch loop until the context is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.dispatch(ctx)
		case <-s.wake:
			s.dispatch(ctx)
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

// dispatch starts every queued execution that has become eligible.
//
// It scans for the first *eligible* item rather than only the head: an item
// blocked by a lock or by its worker limit must not stall the whole queue.
func (s *Scheduler) dispatch(ctx context.Context) {
	if s.draining.Load() {
		return
	}

	// TODO(spec §14.2): load queued executions, expire the ones past
	// queue.item_ttl, and start those whose slot and lock are both available.
	_ = ctx
}

// Drain stops admitting and dispatching work. Queued executions stay queued and
// are picked up again after a restart.
func (s *Scheduler) Drain() { s.draining.Store(true) }

var errNotImplemented = errors.New("not implemented")
