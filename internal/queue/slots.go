// Package queue admits executions and decides when they may start.
package queue

import "sync"

// Slots tracks concurrency against two independent ceilings: the node-wide
// limit and the per-worker limit. An execution may only start when both have
// room; a worker at its limit keeps waiting instead of being rejected
// (docs/SPEC.md §14.1).
//
// A slot is held by an execution, not by one of its attempts: a service keeps
// its slot across the gap between a crash and its restart, otherwise a task
// could take it mid-backoff and the service would never come back. Holders are
// therefore tracked by execution id, which also makes acquire and release
// idempotent — the start path releases what it reserved on every failure, and
// the supervisor reports the outcome of the same attempt independently.
type Slots struct {
	mu sync.Mutex

	globalMax  int
	globalUsed int

	workerMax  map[string]int
	workerUsed map[string]int

	// holders maps an execution id to the worker whose slot it holds.
	holders map[string]string
}

// NewSlots returns a slot table with the node-wide limit applied.
func NewSlots(globalMax int) *Slots {
	return &Slots{
		globalMax:  globalMax,
		workerMax:  map[string]int{},
		workerUsed: map[string]int{},
		holders:    map[string]string{},
	}
}

// SetWorkerLimit sets the ceiling for one worker. A limit of zero means the
// worker is only bound by the global limit.
func (s *Slots) SetWorkerLimit(worker string, limit int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.workerMax[worker] = limit
}

// TryAcquire reserves a slot for the execution, reporting whether it succeeded.
// An execution that already holds its slot keeps it and succeeds without
// consuming a second one.
func (s *Slots) TryAcquire(processID, worker string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, held := s.holders[processID]; held {
		return true
	}

	if s.globalUsed >= s.globalMax {
		return false
	}

	if limit := s.workerMax[worker]; limit > 0 && s.workerUsed[worker] >= limit {
		return false
	}

	s.globalUsed++
	s.workerUsed[worker]++
	s.holders[processID] = worker

	return true
}

// Release returns the slot the execution holds. Releasing a slot that is not
// held is a no-op.
func (s *Slots) Release(processID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	worker, held := s.holders[processID]
	if !held {
		return
	}

	delete(s.holders, processID)

	if s.globalUsed > 0 {
		s.globalUsed--
	}

	if s.workerUsed[worker] > 0 {
		s.workerUsed[worker]--
	}
}

// Holds reports whether the execution currently occupies a slot.
func (s *Slots) Holds(processID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, held := s.holders[processID]

	return held
}

// Usage reports the used and maximum node-wide slots.
func (s *Slots) Usage() (used, limit int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.globalUsed, s.globalMax
}
