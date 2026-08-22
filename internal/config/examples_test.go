package config

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/curruwilla/processd/internal/core"
)

// The shipped examples are the first thing an operator copies, so a rule they
// no longer satisfy has to fail here rather than on someone's first reload.
func TestLoadWorkers_examples(t *testing.T) {
	t.Parallel()

	registry, err := LoadWorkers(filepath.Join("..", "..", "examples", "workers.d"))
	if err != nil {
		t.Fatalf("LoadWorkers() returned %v, want nil", err)
	}

	task, err := registry.Get("invoice-process")
	if err != nil {
		t.Fatalf("Get() returned %v, want nil", err)
	}

	if task.Type != core.TypeTask {
		t.Errorf("invoice-process type = %q, want %q", task.Type, core.TypeTask)
	}

	service, err := registry.Get("api")
	if err != nil {
		t.Fatalf("Get() returned %v, want nil", err)
	}

	if service.Type != core.TypeService {
		t.Errorf("api type = %q, want %q", service.Type, core.TypeService)
	}

	if !service.Retry.IsEnabled() || !service.Retry.MaxAttempts.Unlimited() {
		t.Errorf("api retry = %v/%v, want enabled and unlimited",
			service.Retry.IsEnabled(), service.Retry.MaxAttempts)
	}

	if service.Logs.Rotate.MaxFiles < 1 {
		t.Errorf("api logs.rotate.max_files = %d, want rotation configured", service.Logs.Rotate.MaxFiles)
	}
}

// The scheduled example has to compile its expression and resolve with no
// params at all, which is exactly what a firing gives it.
func TestLoadWorkers_scheduledExample(t *testing.T) {
	t.Parallel()

	registry, err := LoadWorkers(filepath.Join("..", "..", "examples", "workers.d"))
	if err != nil {
		t.Fatalf("LoadWorkers() returned %v, want nil", err)
	}

	worker, err := registry.Get("nightly-report")
	if err != nil {
		t.Fatalf("Get() returned %v, want nil", err)
	}

	if !worker.Schedule.IsSet() || worker.Schedule.Compiled() == nil {
		t.Fatal("nightly-report has no compiled schedule")
	}

	if got := worker.Schedule.Compiled().Location().String(); got != "America/Sao_Paulo" {
		t.Errorf("schedule zone = %q, want America/Sao_Paulo", got)
	}

	if _, err := worker.Instantiate(nil); err != nil {
		t.Fatalf("Instantiate(nil) returned %v: a scheduled worker must run with no params", err)
	}
}

// The notification example has to resolve against its target, which lives in
// another file — the check LoadWorkers can only do once the directory is in.
func TestLoadWorkers_notifyExample(t *testing.T) {
	t.Parallel()

	registry, err := LoadWorkers(filepath.Join("..", "..", "examples", "workers.d"))
	if err != nil {
		t.Fatalf("LoadWorkers() returned %v, want nil", err)
	}

	worker, err := registry.Get("nightly-report")
	if err != nil {
		t.Fatalf("Get() returned %v, want nil", err)
	}

	if !worker.Notify.IsSet() {
		t.Fatal("nightly-report declares no notification")
	}

	if !worker.Notify.Notifies(NotifyOnRetriesExhausted) {
		t.Errorf("notify.on = %v, want retries_exhausted", worker.Notify.On)
	}

	// The bounds are defaults, not requirements, so the example must still end
	// up with them.
	if worker.Notify.Webhook == nil || worker.Notify.Webhook.Timeout <= 0 {
		t.Fatalf("notify.webhook = %+v, want a bounded timeout", worker.Notify.Webhook)
	}

	target, err := registry.Get(worker.Notify.Exec.Worker)
	if err != nil {
		t.Fatalf("the notification target does not resolve: %v", err)
	}

	// Every param the target declares has to be one a notification carries.
	for name, param := range target.Params {
		if param.Required && !slices.Contains(NotifyParams, name) {
			t.Errorf("notification target requires param %q, which no notification carries", name)
		}
	}
}

// The shipped daemon configuration is the first thing an operator copies, so it
// has to load and validate exactly as written.
func TestLoad_exampleConfig(t *testing.T) {
	t.Parallel()

	cfg, err := Load(filepath.Join("..", "..", "examples", "processd.yaml"))
	if err != nil {
		t.Fatalf("Load() returned %v, want nil", err)
	}

	if cfg.Listen == "" || cfg.DataDir == "" || cfg.WorkersDir == "" {
		t.Fatalf("the example left required paths empty: %+v", cfg)
	}

	if len(cfg.Auth.Tokens) == 0 {
		t.Error("the example declares no token, and a node without one refuses every call")
	}
}
