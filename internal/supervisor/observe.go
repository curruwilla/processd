package supervisor

import (
	"github.com/curruwilla/processd/internal/runner"
)

// Sample describes one attempt under supervision.
//
// It is a copy: the supervised execution is mutated by its own goroutine, so
// observers never receive a pointer into it.
type Sample struct {
	ProcessID    string
	Worker       string
	Attempt      int
	PID          int
	PIDStartTime uint64
}

// Snapshot lists the attempts currently under supervision, in no particular
// order.
func (s *Supervisor) Snapshot() []Sample {
	s.mu.Lock()
	executions := make([]*execution, 0, len(s.running))

	for _, exec := range s.running {
		executions = append(executions, exec)
	}

	s.mu.Unlock()

	samples := make([]Sample, 0, len(executions))

	for _, exec := range executions {
		exec.mu.Lock()

		samples = append(samples, Sample{
			ProcessID:    exec.process.ID,
			Worker:       exec.process.Worker,
			Attempt:      exec.process.Attempt,
			PID:          exec.process.PID,
			PIDStartTime: exec.process.PIDStartTime,
		})

		exec.mu.Unlock()
	}

	return samples
}

// Usage samples the resources one execution is using. It reports false when the
// execution is not running here, and when the process died between the lookup
// and the sample — a sample of a process that is gone is not an error, it is
// simply unavailable.
func (s *Supervisor) Usage(id string) (runner.Usage, bool) {
	exec := s.lookup(id)
	if exec == nil {
		return runner.Usage{}, false
	}

	usage, err := s.runner.Usage(exec.handle)
	if err != nil {
		return runner.Usage{}, false
	}

	return usage, true
}

// RunningAttempt reports which attempt of an execution is running right now.
// Log streaming uses it to tell "this attempt is still writing" from "this
// attempt is over", which are the same file with different endings.
func (s *Supervisor) RunningAttempt(id string) (int, bool) {
	exec := s.lookup(id)
	if exec == nil {
		return 0, false
	}

	exec.mu.Lock()
	defer exec.mu.Unlock()

	return exec.process.Attempt, true
}
