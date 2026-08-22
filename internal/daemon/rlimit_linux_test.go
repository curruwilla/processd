package daemon

import (
	"log/slog"
	"syscall"
	"testing"
)

func currentFileLimit(t *testing.T) syscall.Rlimit {
	t.Helper()

	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatalf("reading the file limit: %v", err)
	}

	return limit
}

func TestEnsureFileLimit(t *testing.T) {
	t.Parallel()

	before := currentFileLimit(t)
	log := slog.New(slog.DiscardHandler)

	tests := []struct {
		name         string
		maxProcesses int
	}{
		{name: "no concurrency configured", maxProcesses: 0},
		{name: "negative concurrency", maxProcesses: -1},
		{name: "a limit the process already has", maxProcesses: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ensureFileLimit(tt.maxProcesses, log)

			after := currentFileLimit(t)

			// Whatever it decides, it must never leave the process able to open
			// fewer files than before.
			if after.Cur < before.Cur {
				t.Errorf("soft limit dropped from %d to %d", before.Cur, after.Cur)
			}

			if after.Max != before.Max {
				t.Errorf("hard limit changed from %d to %d", before.Max, after.Max)
			}
		})
	}
}

// The diagnostic exists to tell an operator how small the limit is, so it must
// not print a number in the quintillions when the limit is smaller than the
// daemon's own reserve — which is exactly when it fires.
func TestSupportedProcesses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		limit uint64
		want  int
	}{
		{name: "below the reserve", limit: 64, want: 0},
		{name: "exactly the reserve", limit: fdReserve, want: 0},
		{name: "one process above the reserve", limit: fdReserve + fdsPerProcess, want: 1},
		{name: "a roomy limit", limit: fdReserve + 100*fdsPerProcess, want: 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := supportedProcesses(tt.limit); got != tt.want {
				t.Errorf("supportedProcesses(%d) = %d, want %d", tt.limit, got, tt.want)
			}
		})
	}
}
