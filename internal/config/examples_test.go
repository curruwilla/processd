package config

import (
	"path/filepath"
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
