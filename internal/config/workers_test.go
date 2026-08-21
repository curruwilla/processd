package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/curruwilla/processd/internal/core"
)

func TestWorker_Validate(t *testing.T) {
	t.Parallel()

	valid := func() *Worker {
		w := &Worker{
			Name:    "invoice",
			Command: "/usr/bin/php",
			Args:    []string{"--id={{id}}"},
			Params:  map[string]Param{"id": {Required: true, Pattern: "^[0-9]+$"}},
		}
		applyWorkerDefaults(w)

		return w
	}

	tests := []struct {
		name    string
		mutate  func(*Worker)
		wantErr string
	}{
		{
			name:   "valid worker",
			mutate: func(*Worker) {},
		},
		{
			name:    "command must be absolute",
			mutate:  func(w *Worker) { w.Command = "php" },
			wantErr: "absolute path",
		},
		{
			name:    "placeholder must be declared",
			mutate:  func(w *Worker) { w.Args = []string{"--id={{unknown}}"} },
			wantErr: "not declared in params",
		},
		{
			name:    "lock placeholder must be declared",
			mutate:  func(w *Worker) { w.Lock = "invoice:{{missing}}" },
			wantErr: "not declared in params",
		},
		{
			name:    "pattern must compile",
			mutate:  func(w *Worker) { w.Params["id"] = Param{Pattern: "([0-9]"} },
			wantErr: "pattern",
		},
		{
			name:    "unknown type is refused",
			mutate:  func(w *Worker) { w.Type = "daemon" },
			wantErr: "not supported",
		},
		{
			name: "retry jitter must be a ratio",
			mutate: func(w *Worker) {
				w.Retry.Enabled = Bool(true)
				w.Retry.MaxAttempts = 2
				w.Retry.Backoff.Type = BackoffFixed
				w.Retry.Backoff.Jitter = 3
			},
			wantErr: "jitter",
		},
		{
			name:    "a task may not retry on a plain exit",
			mutate:  func(w *Worker) { w.Retry.RetryOn = []RetryTrigger{RetryOnExit} },
			wantErr: "only valid for a service",
		},
		{
			name:    "a task may not retry forever",
			mutate:  func(w *Worker) { w.Retry.MaxAttempts = AttemptsUnlimited },
			wantErr: "only valid for a service",
		},
		{
			name:    "rotation depth may not be negative",
			mutate:  func(w *Worker) { w.Logs.Rotate.MaxFiles = -1 },
			wantErr: "must not be negative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			worker := valid()
			tt.mutate(worker)

			err := worker.Validate()

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() returned %v, want nil", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("Validate() returned nil, want an error containing %q", tt.wantErr)
			}

			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadWorkers(t *testing.T) {
	t.Parallel()

	t.Run("loads and applies defaults", func(t *testing.T) {
		t.Parallel()

		dir := writeWorkerFile(t, "invoice.yaml", `
version: 1
workers:
  - name: invoice
    command: /usr/bin/php
    args: ["--id={{id}}"]
    params:
      id: {required: true, pattern: "^[0-9]+$"}
`)

		registry, err := LoadWorkers(dir)
		if err != nil {
			t.Fatalf("LoadWorkers() returned %v, want nil", err)
		}

		worker, err := registry.Get("invoice")
		if err != nil {
			t.Fatalf("Get() returned %v, want the worker", err)
		}

		if !worker.IsEnabled() {
			t.Error("worker is disabled, want enabled by default")
		}

		if worker.Cwd != "/" {
			t.Errorf("cwd = %q, want the default %q", worker.Cwd, "/")
		}

		if worker.LockConflict != LockConflictQueue {
			t.Errorf("lock_conflict = %q, want %q", worker.LockConflict, LockConflictQueue)
		}
	})

	t.Run("rejects unknown fields", func(t *testing.T) {
		t.Parallel()

		dir := writeWorkerFile(t, "typo.yaml", `
version: 1
workers:
  - name: invoice
    command: /usr/bin/php
    allow_root: true
`)

		if _, err := LoadWorkers(dir); err == nil {
			t.Fatal("LoadWorkers() returned nil, want an error for the unknown field")
		}
	})

	t.Run("rejects duplicate worker names", func(t *testing.T) {
		t.Parallel()

		dir := writeWorkerFile(t, "dup.yaml", `
version: 1
workers:
  - name: invoice
    command: /usr/bin/php
  - name: invoice
    command: /usr/bin/php
`)

		if _, err := LoadWorkers(dir); err == nil {
			t.Fatal("LoadWorkers() returned nil, want an error for the duplicate name")
		}
	})

	t.Run("missing directory yields an empty registry", func(t *testing.T) {
		t.Parallel()

		registry, err := LoadWorkers(filepath.Join(t.TempDir(), "absent"))
		if err != nil {
			t.Fatalf("LoadWorkers() returned %v, want nil", err)
		}

		if registry.Len() != 0 {
			t.Errorf("registry holds %d workers, want 0", registry.Len())
		}
	})
}

func writeWorkerFile(t *testing.T, name, content string) string {
	t.Helper()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}

	return dir
}

// TestWorker_ValidateService covers the keys whose meaning flips between the
// two execution types, where the wrong one has to be refused rather than
// quietly ignored (docs/SPEC.md §4).
func TestWorker_ValidateService(t *testing.T) {
	t.Parallel()

	valid := func() *Worker {
		w := &Worker{
			Name:    "api",
			Type:    core.TypeService,
			Command: "/usr/bin/api",
			Logs:    WorkerLogs{Rotate: LogRotate{MaxFiles: 3}},
		}
		applyWorkerDefaults(w)

		return w
	}

	tests := []struct {
		name    string
		mutate  func(*Worker)
		wantErr string
	}{
		{
			name:   "valid service",
			mutate: func(*Worker) {},
		},
		{
			name:    "retries may not be turned off",
			mutate:  func(w *Worker) { w.Retry.Enabled = Bool(false) },
			wantErr: "must not be false for a service",
		},
		{
			name:    "a service has no timeout",
			mutate:  func(w *Worker) { w.Timeout = Duration(time.Minute) },
			wantErr: "no deadline",
		},
		{
			name:    "no exit code counts as success",
			mutate:  func(w *Worker) { w.Retry.SuccessExitCodes = []int{0} },
			wantErr: "every exit is abnormal",
		},
		{
			name:    "logs must rotate",
			mutate:  func(w *Worker) { w.Logs.Rotate.MaxFiles = 0 },
			wantErr: "logs.rotate.max_files",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			worker := valid()
			tt.mutate(worker)

			err := worker.Validate()

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() returned %v, want nil", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("Validate() returned nil, want an error mentioning %q", tt.wantErr)
			}

			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// TestApplyWorkerDefaults_Service pins the defaults a service gets for free.
// They are what makes a service definition short: nothing in this list has a
// second sensible answer.
func TestApplyWorkerDefaults_Service(t *testing.T) {
	t.Parallel()

	worker := &Worker{
		Name:    "api",
		Type:    core.TypeService,
		Command: "/usr/bin/api",
		Logs:    WorkerLogs{Rotate: LogRotate{MaxFiles: 3}},
	}
	applyWorkerDefaults(worker)

	if !worker.Retry.IsEnabled() {
		t.Error("retry.enabled = false, want a service to restart by default")
	}

	if !worker.Retry.MaxAttempts.Unlimited() {
		t.Errorf("retry.max_attempts = %v, want unlimited", worker.Retry.MaxAttempts)
	}

	if !slices.Contains(worker.Retry.RetryOn, RetryOnExit) {
		t.Errorf("retry.retry_on = %v, want it to include %q", worker.Retry.RetryOn, RetryOnExit)
	}

	if len(worker.Retry.SuccessExitCodes) != 0 {
		t.Errorf("retry.success_exit_codes = %v, want none for a service", worker.Retry.SuccessExitCodes)
	}

	if err := worker.Validate(); err != nil {
		t.Errorf("Validate() returned %v, want the defaults to be valid", err)
	}
}

// A task keeps the defaults it had before services existed.
func TestApplyWorkerDefaults_TaskUnchanged(t *testing.T) {
	t.Parallel()

	worker := &Worker{Name: "invoice", Command: "/bin/echo", Retry: Retry{Enabled: Bool(true)}}
	applyWorkerDefaults(worker)

	if worker.Retry.MaxAttempts != 1 {
		t.Errorf("retry.max_attempts = %v, want 1", worker.Retry.MaxAttempts)
	}

	if slices.Contains(worker.Retry.RetryOn, RetryOnExit) {
		t.Errorf("retry.retry_on = %v, want no %q for a task", worker.Retry.RetryOn, RetryOnExit)
	}

	if !slices.Equal(worker.Retry.SuccessExitCodes, []int{0}) {
		t.Errorf("retry.success_exit_codes = %v, want [0]", worker.Retry.SuccessExitCodes)
	}
}
