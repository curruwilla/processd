package daemon

import (
	"log/slog"
	"syscall"
)

const (
	// fdsPerProcess is what one supervised execution costs in descriptors: two
	// capped log files plus both ends of the stdout and stderr pipes. Measured
	// at five with 64 concurrent executions.
	fdsPerProcess = 5

	// fdReserve covers the daemon itself: the listener, accepted connections,
	// the database and its write-ahead log.
	fdReserve = 256
)

// ensureFileLimit checks that the process can open enough descriptors for the
// configured concurrency, and says so plainly when it cannot.
//
// The Go runtime already raises the soft limit to the hard limit at startup, so
// the raise here is a formality; the hard limit set by systemd or the container
// runtime is the real ceiling. What matters is the diagnostic: without it,
// max_processes silently stops meaning anything above roughly a fifth of the
// limit, and executions fail while creating pipes instead of queueing — which
// reads like a broken worker rather than a limit on the node.
func ensureFileLimit(maxProcesses int, log *slog.Logger) {
	if maxProcesses <= 0 {
		return
	}

	needed := uint64(maxProcesses)*fdsPerProcess + fdReserve

	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		log.Warn("reading the file descriptor limit", slog.Any("error", err))
		return
	}

	if limit.Cur >= needed {
		return
	}

	target := min(needed, limit.Max)

	raised := limit
	raised.Cur = target

	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &raised); err != nil {
		log.Warn("raising the file descriptor limit",
			slog.Uint64("current", limit.Cur),
			slog.Uint64("needed", needed),
			slog.Any("error", err),
		)

		return
	}

	log.Info("raised the file descriptor limit",
		slog.Uint64("from", limit.Cur),
		slog.Uint64("to", target),
	)

	if target < needed {
		log.Warn("file descriptor limit is below what max_processes needs",
			slog.Int("max_processes", maxProcesses),
			slog.Uint64("limit", target),
			slog.Uint64("needed", needed),
			slog.Int("supported_processes", supportedProcesses(target)),
		)
	}
}

// supportedProcesses is how many executions a descriptor limit actually allows.
//
// The subtraction is guarded because the limit can be below the daemon's own
// reserve — a container with a hard limit of 64, say. Unsigned arithmetic would
// wrap there and print a number in the quintillions, in the one log line whose
// whole job is to tell the operator how small the limit is.
func supportedProcesses(limit uint64) int {
	if limit <= fdReserve {
		return 0
	}

	//nolint:gosec // bounded above by the descriptor limit
	return int((limit - fdReserve) / fdsPerProcess)
}
