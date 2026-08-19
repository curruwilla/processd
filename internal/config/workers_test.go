package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
			name:    "service type is not supported yet",
			mutate:  func(w *Worker) { w.Type = "service" },
			wantErr: "not supported",
		},
		{
			name: "retry jitter must be a ratio",
			mutate: func(w *Worker) {
				w.Retry.Enabled = true
				w.Retry.MaxAttempts = 2
				w.Retry.Backoff.Type = BackoffFixed
				w.Retry.Backoff.Jitter = 3
			},
			wantErr: "jitter",
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
