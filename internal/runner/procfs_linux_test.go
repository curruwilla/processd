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
