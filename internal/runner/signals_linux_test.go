package runner

import (
	"slices"
	"syscall"
	"testing"
)

func TestParseSignal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		signal  string
		want    syscall.Signal
		wantErr bool
	}{
		{name: "sigterm", signal: "SIGTERM", want: syscall.SIGTERM},
		{name: "sigusr1", signal: "SIGUSR1", want: syscall.SIGUSR1},
		{name: "unknown name", signal: "SIGDANGER", wantErr: true},
		{name: "numeric form is refused", signal: "15", wantErr: true},
		{name: "lowercase is refused", signal: "sigterm", wantErr: true},
		{name: "empty", signal: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseSignal(tt.signal)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseSignal(%q) returned nil, want an error", tt.signal)
				}

				return
			}

			if err != nil {
				t.Fatalf("ParseSignal(%q) returned %v, want nil", tt.signal, err)
			}

			if got != tt.want {
				t.Errorf("ParseSignal(%q) = %v, want %v", tt.signal, got, tt.want)
			}
		})
	}
}

func TestSignalNames(t *testing.T) {
	t.Parallel()

	names := SignalNames()

	if !slices.IsSorted(names) {
		t.Errorf("SignalNames() = %q, want it sorted", names)
	}

	if !slices.Contains(names, "SIGTERM") {
		t.Errorf("SignalNames() = %q, want SIGTERM to be listed", names)
	}
}
