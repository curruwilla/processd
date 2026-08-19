package runner

import (
	"bytes"
	"context"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExecRunner_Start(t *testing.T) {
	t.Parallel()

	t.Run("runs a command and captures stdout", func(t *testing.T) {
		t.Parallel()

		var stdout bytes.Buffer

		handle, err := NewExecRunner().Start(t.Context(), Spec{
			Command: "/bin/echo",
			Args:    []string{"hello"},
			Cwd:     "/",
			Stdout:  &stdout,
		})
		if err != nil {
			t.Fatalf("Start() returned %v, want nil", err)
		}

		result, err := NewExecRunner().Wait(t.Context(), handle)
		if err != nil {
			t.Fatalf("Wait() returned %v, want nil", err)
		}

		if result.ExitCode != 0 {
			t.Errorf("exit code = %d, want 0", result.ExitCode)
		}

		if strings.TrimSpace(stdout.String()) != "hello" {
			t.Errorf("stdout = %q, want %q", stdout.String(), "hello")
		}

		if handle.PID <= 0 || handle.PIDStartTime == 0 {
			t.Errorf("handle = (pid %d, start %d), want both to be recorded", handle.PID, handle.PIDStartTime)
		}
	})

	t.Run("reports a non-zero exit code", func(t *testing.T) {
		t.Parallel()

		handle, err := NewExecRunner().Start(t.Context(), Spec{Command: "/bin/false", Cwd: "/"})
		if err != nil {
			t.Fatalf("Start() returned %v, want nil", err)
		}

		result, err := NewExecRunner().Wait(t.Context(), handle)
		if err != nil {
			t.Fatalf("Wait() returned %v, want nil", err)
		}

		if result.ExitCode == 0 {
			t.Error("exit code = 0, want a failure code")
		}
	})

	t.Run("rejects a relative command", func(t *testing.T) {
		t.Parallel()

		_, err := NewExecRunner().Start(t.Context(), Spec{Command: "echo", Cwd: "/"})
		if err == nil {
			t.Fatal("Start() returned nil, want an error for the relative path")
		}
	})

	t.Run("gives the process its own group", func(t *testing.T) {
		t.Parallel()

		handle, err := NewExecRunner().Start(t.Context(), Spec{Command: "/bin/sleep", Args: []string{"5"}, Cwd: "/"})
		if err != nil {
			t.Fatalf("Start() returned %v, want nil", err)
		}

		defer func() {
			_, _ = NewExecRunner().Wait(context.WithoutCancel(t.Context()), handle)
		}()

		pgid, err := processGroup(handle.PID)
		if err != nil {
			t.Fatalf("processGroup() returned %v, want nil", err)
		}

		if pgid != handle.PID {
			t.Errorf("pgid = %d, want it to equal the pid %d", pgid, handle.PID)
		}

		if pgid == syscall.Getpgrp() {
			t.Error("child shares the daemon process group, want a dedicated one")
		}

		if err := NewExecRunner().Stop(t.Context(), handle, time.Second); err != nil {
			t.Fatalf("Stop() returned %v, want nil", err)
		}
	})
}

func TestExecRunner_Stop(t *testing.T) {
	t.Parallel()

	runner := NewExecRunner()

	handle, err := runner.Start(t.Context(), Spec{Command: "/bin/sleep", Args: []string{"30"}, Cwd: "/"})
	if err != nil {
		t.Fatalf("Start() returned %v, want nil", err)
	}

	results := make(chan Result, 1)

	go func() {
		result, waitErr := runner.Wait(context.WithoutCancel(t.Context()), handle)
		if waitErr != nil {
			t.Errorf("Wait() returned %v, want nil", waitErr)
		}

		results <- result
	}()

	started := time.Now()

	if err := runner.Stop(t.Context(), handle, 10*time.Second); err != nil {
		t.Fatalf("Stop() returned %v, want nil", err)
	}

	select {
	case result := <-results:
		if result.Signal != "SIGTERM" {
			t.Errorf("signal = %q, want SIGTERM", result.Signal)
		}

		if result.ExitCode != 143 {
			t.Errorf("exit code = %d, want 143", result.ExitCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit after Stop")
	}

	// Stop must return as soon as the process is reaped, not after the grace
	// period has elapsed.
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("Stop() took %s, want it to return on exit", elapsed)
	}
}

func TestBuildEnv(t *testing.T) {
	// No t.Parallel: t.Setenv mutates process-wide state.
	t.Setenv("PROCESSD_TEST_SECRET", "do-not-leak")
	t.Setenv("PROCESSD_TEST_PATH", "/usr/bin")

	env := buildEnv(Spec{
		Env:            map[string]string{"APP_ENV": "production"},
		EnvPassthrough: []string{"PROCESSD_TEST_PATH", "PROCESSD_TEST_ABSENT"},
	})

	joined := strings.Join(env, "\n")

	if !strings.Contains(joined, "APP_ENV=production") {
		t.Errorf("env = %q, want the explicit value", env)
	}

	if !strings.Contains(joined, "PROCESSD_TEST_PATH=/usr/bin") {
		t.Errorf("env = %q, want the passthrough value", env)
	}

	// The child inherits nothing by default: the daemon environment holds API
	// tokens and database paths.
	if strings.Contains(joined, "do-not-leak") {
		t.Errorf("env = %q, want no daemon variables to leak", env)
	}

	if strings.Contains(joined, "PROCESSD_TEST_ABSENT") {
		t.Errorf("env = %q, want unset passthrough names to be skipped", env)
	}
}
