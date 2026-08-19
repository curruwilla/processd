// Package queue admits executions and decides when they may start.
package queue

import "sync"

// Slots tracks concurrency against two independent ceilings: the node-wide
// limit and the per-worker limit. An execution may only start when both have
// room; a worker at its limit keeps waiting instead of being rejected
// (docs/SPEC.md §14.1).
type Slots struct {
	mu sync.Mutex

	globalMax  int
	globalUsed int

	workerMax  map[string]int
	workerUsed map[string]int
}

// NewSlots returns a slot table with the node-wide limit applied.
func NewSlots(globalMax int) *Slots {
	return &Slots{
		globalMax:  globalMax,
		workerMax:  map[string]int{},
		workerUsed: map[string]int{},
	}
}

// SetWorkerLimit sets the ceiling for one worker. A limit of zero means the
// worker is only bound by the global limit.
func (s *Slots) SetWorkerLimit(worker string, limit int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.workerMax[worker] = limit
}

// TryAcquire reserves a slot for the worker, reporting whether it succeeded.
func (s *Slots) TryAcquire(worker string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.globalUsed >= s.globalMax {
		return false
	}

	if limit := s.workerMax[worker]; limit > 0 && s.workerUsed[worker] >= limit {
		return false
	}

	s.globalUsed++
	s.workerUsed[worker]++

	return true
}

// Release returns a slot previously acquired for the worker.
func (s *Slots) Release(worker string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.globalUsed > 0 {
		s.globalUsed--
	}

	if s.workerUsed[worker] > 0 {
		s.workerUsed[worker]--
	}
}

// Usage reports the used and maximum node-wide slots.
func (s *Slots) Usage() (used, limit int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.globalUsed, s.globalMax
}
