// Package runner starts and supervises OS processes.
//
// Processd is Linux-first: the implementation relies on POSIX process groups,
// signals and /proc, so the concrete runner only builds on Linux.
package runner

import (
	"context"
	"io"
	"os/exec"
	"sync"
	"time"
)

// Spec is everything needed to start one attempt.
type Spec struct {
	Command string
	Args    []string
	Cwd     string

	// Env is the complete environment of the child, except for the daemon
	// variables named in EnvPassthrough. The child never inherits the daemon
	// environment wholesale: that would leak API tokens and database paths.
	Env            map[string]string
	EnvPassthrough []string

	User  string
	Group string

	Stdout io.Writer
	Stderr io.Writer

	// AllowRoot permits running as root when no User is set. Off by default:
	// the daemon fails closed rather than silently running work as root.
	AllowRoot bool
}

// Handle identifies a started process. The pair (PID, PIDStartTime) is what
// makes a PID safe to signal after a daemon restart — PIDs are recycled, the
// start time is not (docs/SPEC.md §8).
type Handle struct {
	PID          int
	PIDStartTime uint64

	pgid int
	cmd  *exec.Cmd

	// done is closed once Wait has reaped the process. Stop waits on it instead
	// of polling: a reaped-but-unwaited child stays a signalable zombie, so
	// liveness polling alone would always burn the full grace period.
	done     chan struct{}
	doneOnce sync.Once
}

// markDone reports that the process has been reaped.
func (h *Handle) markDone() {
	if h.done == nil {
		return
	}

	h.doneOnce.Do(func() { close(h.done) })
}

// Usage is a point-in-time resource sample of a running process, read from
// /proc. It is only meaningful while the process is alive.
type Usage struct {
	// CPUSeconds is the CPU time the process has consumed, user plus system.
	CPUSeconds float64
	// RSSBytes is the resident set size: the memory actually held in RAM.
	RSSBytes int64
	Threads  int
}

// Result is how one attempt ended.
type Result struct {
	ExitCode int
	Signal   string
}

// Runner starts processes and controls their lifetime.
type Runner interface {
	// Start forks and execs the spec, returning as soon as the process is
	// running.
	Start(ctx context.Context, spec Spec) (*Handle, error)
	// Wait blocks until the process exits and reports how it ended.
	Wait(ctx context.Context, h *Handle) (Result, error)
	// Signal delivers sig to the whole process group.
	Signal(h *Handle, name string) error
	// Stop asks the process group to terminate, escalating to SIGKILL after
	// grace has elapsed.
	Stop(ctx context.Context, h *Handle, grace time.Duration) error
	// Usage samples the resources the process is using right now. It fails once
	// the process is gone, which is not an error worth reporting: a sample of a
	// dead process has no meaning.
	Usage(h *Handle) (Usage, error)
}
