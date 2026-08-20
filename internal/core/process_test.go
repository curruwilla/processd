package core

import (
	"testing"
	"time"
)

func TestProcess_Duration(t *testing.T) {
	t.Parallel()

	started := time.Now()
	finished := started.Add(1500 * time.Millisecond)

	tests := []struct {
		name string
		p    *Process
		want time.Duration
	}{
		{name: "unfinished", p: &Process{StartedAt: &started}, want: 0},
		{name: "never started", p: &Process{FinishedAt: &finished}, want: 0},
		{name: "finished", p: &Process{StartedAt: &started, FinishedAt: &finished}, want: 1500 * time.Millisecond},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.p.Duration(); got != tt.want {
				t.Errorf("Duration() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestProcess_Clone(t *testing.T) {
	t.Parallel()

	started := time.Now()
	exitCode := 3

	original := &Process{
		ID:         "proc_1",
		Args:       []string{"--id=1"},
		Env:        map[string]string{"APP_ENV": "production"},
		Metadata:   map[string]string{"origin": "cron"},
		ExitCode:   &exitCode,
		StartedAt:  &started,
		FinishedAt: &started,
		RetryAt:    &started,
		QueuedAt:   &started,
	}

	clone := original.Clone()

	clone.ID = "proc_2"
	clone.Args[0] = "--id=2"
	clone.Env["APP_ENV"] = "staging"
	clone.Metadata["origin"] = "api"
	*clone.ExitCode = 9
	*clone.StartedAt = started.Add(time.Hour)

	if original.ID != "proc_1" {
		t.Errorf("id = %q, want the original to be untouched", original.ID)
	}

	if original.Args[0] != "--id=1" {
		t.Errorf("args = %q, want the original slice to be untouched", original.Args)
	}

	if original.Env["APP_ENV"] != "production" {
		t.Errorf("env = %v, want the original map to be untouched", original.Env)
	}

	if original.Metadata["origin"] != "cron" {
		t.Errorf("metadata = %v, want the original map to be untouched", original.Metadata)
	}

	if *original.ExitCode != 3 {
		t.Errorf("exit code = %d, want the original pointer to be untouched", *original.ExitCode)
	}

	if !original.StartedAt.Equal(started) {
		t.Error("started_at changed, want the original timestamp to be untouched")
	}
}

func TestProcess_ClearAttempt(t *testing.T) {
	t.Parallel()

	now := time.Now()
	exitCode := 1

	p := &Process{
		Attempt:      2,
		PID:          42,
		PIDStartTime: 7,
		ExitCode:     &exitCode,
		Signal:       "SIGTERM",
		LogTruncated: true,
		StartedAt:    &now,
		FinishedAt:   &now,
	}

	p.ClearAttempt()

	if p.PID != 0 || p.PIDStartTime != 0 || p.ExitCode != nil || p.Signal != "" || p.LogTruncated {
		t.Errorf("runtime facts survived the reset: %+v", p)
	}

	if p.StartedAt != nil || p.FinishedAt != nil {
		t.Error("timestamps survived the reset, want them cleared")
	}

	// The attempt counter itself belongs to the execution, not to one try.
	if p.Attempt != 2 {
		t.Errorf("attempt = %d, want it preserved as 2", p.Attempt)
	}
}
