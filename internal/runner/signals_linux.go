package runner

import (
	"fmt"
	"sort"
	"syscall"
)

// allowedSignals is the allowlist accepted by the API (docs/SPEC.md §6.7).
// Anything outside it is rejected before reaching the kernel.
var allowedSignals = map[string]syscall.Signal{
	"SIGTERM": syscall.SIGTERM,
	"SIGINT":  syscall.SIGINT,
	"SIGQUIT": syscall.SIGQUIT,
	"SIGHUP":  syscall.SIGHUP,
	"SIGUSR1": syscall.SIGUSR1,
	"SIGUSR2": syscall.SIGUSR2,
	"SIGKILL": syscall.SIGKILL,
	"SIGSTOP": syscall.SIGSTOP,
	"SIGCONT": syscall.SIGCONT,
}

// ParseSignal resolves a signal name from the allowlist.
func ParseSignal(name string) (syscall.Signal, error) {
	sig, ok := allowedSignals[name]
	if !ok {
		return 0, fmt.Errorf("signal %q is not allowed", name)
	}

	return sig, nil
}

// SignalNames returns the accepted signal names, sorted.
func SignalNames() []string {
	names := make([]string, 0, len(allowedSignals))
	for name := range allowedSignals {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

// signalName maps a signal back to its name for reporting.
func signalName(sig syscall.Signal) string {
	for name, candidate := range allowedSignals {
		if candidate == sig {
			return name
		}
	}

	return sig.String()
}
