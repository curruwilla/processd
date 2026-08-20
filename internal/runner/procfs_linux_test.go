package runner

import (
	"os"
	"testing"
)

func TestProcessStartTime(t *testing.T) {
	t.Parallel()

	startTime, err := processStartTime(os.Getpid())
	if err != nil {
		t.Fatalf("processStartTime() returned %v, want nil", err)
	}

	if startTime == 0 {
		t.Error("start time = 0, want the value from /proc")
	}

	if _, err := processStartTime(-1); err == nil {
		t.Error("processStartTime(-1) returned nil, want an error")
	}
}

func TestSameProcess(t *testing.T) {
	t.Parallel()

	pid := os.Getpid()

	startTime, err := processStartTime(pid)
	if err != nil {
		t.Fatalf("processStartTime() returned %v, want nil", err)
	}

	tests := []struct {
		name      string
		pid       int
		startTime uint64
		want      bool
	}{
		{name: "matching fingerprint", pid: pid, startTime: startTime, want: true},
		{name: "recycled pid has another start time", pid: pid, startTime: startTime + 1, want: false},
		{name: "unknown start time is never trusted", pid: pid, startTime: 0, want: false},
		{name: "invalid pid", pid: -1, startTime: startTime, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := SameProcess(tt.pid, tt.startTime); got != tt.want {
				t.Errorf("SameProcess(%d, %d) = %t, want %t", tt.pid, tt.startTime, got, tt.want)
			}
		})
	}
}

func TestProcessUsage(t *testing.T) {
	t.Parallel()

	pid := os.Getpid()

	startTime, err := processStartTime(pid)
	if err != nil {
		t.Fatalf("processStartTime() returned %v, want nil", err)
	}

	usage, err := ProcessUsage(pid, startTime)
	if err != nil {
		t.Fatalf("ProcessUsage() returned %v, want nil", err)
	}

	if usage.RSSBytes <= 0 {
		t.Errorf("RSSBytes = %d, want a running process to hold memory", usage.RSSBytes)
	}

	if usage.Threads < 1 {
		t.Errorf("Threads = %d, want at least one", usage.Threads)
	}

	if usage.CPUSeconds < 0 {
		t.Errorf("CPUSeconds = %v, want a non-negative sample", usage.CPUSeconds)
	}
}

func TestProcessUsage_RejectsRecycledPID(t *testing.T) {
	t.Parallel()

	// A start time that does not belong to the PID is exactly what a recycled
	// PID looks like, and the sample must be refused rather than attributed.
	if _, err := ProcessUsage(os.Getpid(), 1); err == nil {
		t.Error("ProcessUsage() returned nil, want an error for a mismatched fingerprint")
	}
}
