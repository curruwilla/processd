package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/curruwilla/processd/internal/config"
	"github.com/curruwilla/processd/internal/core"
	"github.com/curruwilla/processd/internal/logstore"
	"github.com/curruwilla/processd/internal/metrics"
	"github.com/curruwilla/processd/internal/queue"
	"github.com/curruwilla/processd/internal/runner"
	"github.com/curruwilla/processd/internal/store/sqlite"
	"github.com/curruwilla/processd/internal/supervisor"
)

const testToken = "test-secret"

const liveWorkers = `
version: 1
workers:
  - name: hello
    command: /bin/echo
    args: ["hello", "{{id}}"]
    params:
      id: {required: true, pattern: "^[0-9]+$"}
    cwd: /tmp
  - name: sleeper
    command: /bin/sleep
    args: ["30"]
    cwd: /tmp
    kill_grace: 1s
  - name: chatty
    command: /bin/sh
    args: ["-c", "echo one; sleep 0.4; echo two >&2"]
    cwd: /tmp
    kill_grace: 1s
  - name: api
    type: service
    command: /bin/sleep
    args: ["30"]
    cwd: /tmp
    kill_grace: 1s
    overridable: [timeout]
    retry:
      backoff: {type: fixed, initial: 1s, max: 1s, jitter: 0}
    logs:
      rotate:
        max_files: 2
`

// newLiveServer wires the real object graph: only the network is faked, so the
// handlers are exercised against actual persistence and process execution.
func newLiveServer(t *testing.T) http.Handler {
	t.Helper()

	return newLiveServerWith(t, nil)
}

// newLiveServerWith builds the same graph and lets a test adjust the options,
// for the parts of the surface that are wired in rather than always present.
func newLiveServerWith(t *testing.T, tune func(*Options)) http.Handler {
	t.Helper()

	workersDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workersDir, "w.yaml"), []byte(liveWorkers), 0o600); err != nil {
		t.Fatalf("writing workers: %v", err)
	}

	registry, err := config.LoadWorkers(workersDir)
	if err != nil {
		t.Fatalf("LoadWorkers() returned %v, want nil", err)
	}

	db, err := sqlite.Open(filepath.Join(t.TempDir(), "processd.db"))
	if err != nil {
		t.Fatalf("Open() returned %v, want nil", err)
	}

	t.Cleanup(func() {
		_ = db.Close()
	})

	cfg := config.Default()
	cfg.MaxProcesses = 2
	cfg.AllowRootProcesses = true
	cfg.Auth.Tokens = []config.Token{{Name: "test", Hash: HashToken(testToken)}}

	log := slog.New(slog.DiscardHandler)
	logs := logstore.New(t.TempDir(), 1<<20)
	observed := metrics.NewRegistry()
	sup := supervisor.New(cfg, db, runner.NewExecRunner(), logs, log)
	scheduler := queue.New(cfg, db, registry, sup, log)

	sup.SetMetrics(observed)
	sup.SetWorkers(scheduler.Registry)
	sup.SetOnSettle(scheduler.OnExecutionSettled)

	t.Cleanup(func() {
		_ = sup.Shutdown(t.Context(), time.Second)
	})

	opts := Options{
		Config:     cfg,
		Store:      db,
		Scheduler:  scheduler,
		Supervisor: sup,
		Logs:       logs,
		Metrics:    observed,
		Logger:     log,
		Reload:     func(context.Context) error { return nil },
	}

	if tune != nil {
		tune(&opts)
	}

	return New(opts).Handler()
}

func do(t *testing.T, handler http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	req := httptest.NewRequestWithContext(t.Context(), method, path, reader)
	req.Header.Set("Authorization", "Bearer "+testToken)

	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	for name, value := range headers {
		req.Header.Set(name, value)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()

	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding response %q: %v", rec.Body.String(), err)
	}

	return out
}

// awaitState polls until the execution reaches a terminal state.
func awaitState(t *testing.T, handler http.Handler, id string) processResponse {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		rec := do(t, handler, http.MethodGet, "/v1/processes/"+id, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET process returned %d, want 200", rec.Code)
		}

		process := decode[processResponse](t, rec)
		if process.Status.IsTerminal() {
			return process
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatal("execution did not reach a terminal state")

	return processResponse{}
}

func TestServer_createProcess_runsToCompletion(t *testing.T) {
	t.Parallel()

	handler := newLiveServer(t)

	rec := do(t, handler, http.MethodPost, "/v1/processes", `{"worker":"hello","params":{"id":"42"}}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST returned %d (%s), want 201", rec.Code, rec.Body.String())
	}

	created := decode[createProcessResponse](t, rec)
	if created.ID == "" {
		t.Fatal("response carries no execution id")
	}

	finished := awaitState(t, handler, created.ID)
	if finished.Status != core.StateCompleted {
		t.Fatalf("execution ended as %s, want %s", finished.Status, core.StateCompleted)
	}

	if finished.ExitCode == nil || *finished.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", finished.ExitCode)
	}

	logs := decode[logsResponse](t, do(t, handler, http.MethodGet, "/v1/processes/"+created.ID+"/logs", "", nil))
	if len(logs.Lines) != 1 || !strings.Contains(logs.Lines[0], "hello 42") {
		t.Errorf("logs = %q, want the captured output", logs.Lines)
	}
}

func TestServer_createProcess_idempotency(t *testing.T) {
	t.Parallel()

	handler := newLiveServer(t)
	body := `{"worker":"hello","params":{"id":"7"}}`
	headers := map[string]string{idempotencyHeader: "key-1"}

	first := do(t, handler, http.MethodPost, "/v1/processes", body, headers)
	if first.Code != http.StatusCreated {
		t.Fatalf("first POST returned %d (%s), want 201", first.Code, first.Body.String())
	}

	created := decode[createProcessResponse](t, first)

	replay := do(t, handler, http.MethodPost, "/v1/processes", body, headers)
	if replay.Code != http.StatusOK {
		t.Fatalf("replay returned %d, want 200", replay.Code)
	}

	if replay.Header().Get("Idempotent-Replay") != "true" {
		t.Error("replay is not marked, want the Idempotent-Replay header")
	}

	if decode[createProcessResponse](t, replay).ID != created.ID {
		t.Error("replay produced a different execution, want the original one")
	}

	reuse := do(t, handler, http.MethodPost, "/v1/processes", `{"worker":"hello","params":{"id":"8"}}`, headers)
	if reuse.Code != http.StatusConflict {
		t.Errorf("reusing the key with another payload returned %d, want 409", reuse.Code)
	}
}

func TestServer_listProcesses(t *testing.T) {
	t.Parallel()

	handler := newLiveServer(t)

	for i := range 3 {
		body := `{"worker":"hello","params":{"id":"` + string(rune('1'+i)) + `"}}`
		if rec := do(t, handler, http.MethodPost, "/v1/processes", body, nil); rec.Code != http.StatusCreated {
			t.Fatalf("POST returned %d (%s), want 201", rec.Code, rec.Body.String())
		}
	}

	page := decode[listResponse](t, do(t, handler, http.MethodGet, "/v1/processes?limit=2", "", nil))
	if len(page.Items) != 2 || page.NextCursor == "" {
		t.Fatalf("page held %d items with cursor %q, want 2 items and a cursor", len(page.Items), page.NextCursor)
	}

	next := decode[listResponse](t, do(t, handler, http.MethodGet, "/v1/processes?limit=2&cursor="+page.NextCursor, "", nil))
	if len(next.Items) != 1 {
		t.Errorf("second page held %d items, want 1", len(next.Items))
	}

	if next.Items[0].ID == page.Items[0].ID {
		t.Error("second page repeats the first")
	}
}

func TestServer_deleteProcess(t *testing.T) {
	t.Parallel()

	handler := newLiveServer(t)

	rec := do(t, handler, http.MethodPost, "/v1/processes", `{"worker":"sleeper"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST returned %d (%s), want 201", rec.Code, rec.Body.String())
	}

	created := decode[createProcessResponse](t, rec)

	deleted := do(t, handler, http.MethodDelete, "/v1/processes/"+created.ID+"?grace=1s", "", nil)
	if deleted.Code != http.StatusAccepted {
		t.Fatalf("DELETE returned %d (%s), want 202", deleted.Code, deleted.Body.String())
	}

	finished := awaitState(t, handler, created.ID)
	if finished.Status != core.StateCanceled || finished.Reason != core.ReasonUserRequest {
		t.Errorf("execution is %s/%s, want CANCELED/user_request", finished.Status, finished.Reason)
	}
}

func TestServer_processLogs_validation(t *testing.T) {
	t.Parallel()

	handler := newLiveServer(t)

	rec := do(t, handler, http.MethodPost, "/v1/processes", `{"worker":"hello","params":{"id":"1"}}`, nil)
	created := decode[createProcessResponse](t, rec)

	awaitState(t, handler, created.ID)

	tests := []struct {
		name  string
		query string
		want  int
	}{
		{name: "unknown stream", query: "?stream=syslog", want: http.StatusBadRequest},
		{name: "attempt that never ran", query: "?attempt=9", want: http.StatusBadRequest},
		{name: "negative tail", query: "?tail=-1", want: http.StatusBadRequest},
		{name: "explicit stdout", query: "?stream=stdout", want: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := do(t, handler, http.MethodGet, "/v1/processes/"+created.ID+"/logs"+tt.query, "", nil)
			if got.Code != tt.want {
				t.Errorf("GET logs%s returned %d, want %d", tt.query, got.Code, tt.want)
			}
		})
	}
}

func TestServer_createProcess_service(t *testing.T) {
	t.Parallel()

	t.Run("the worker decides the type", func(t *testing.T) {
		t.Parallel()

		handler := newLiveServer(t)

		rec := do(t, handler, http.MethodPost, "/v1/processes", `{"worker":"api"}`, nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST returned %d (%s), want 201", rec.Code, rec.Body.String())
		}

		created := decode[createProcessResponse](t, rec)
		process := decode[processResponse](t, do(t, handler, http.MethodGet, "/v1/processes/"+created.ID, "", nil))

		if process.Type != core.TypeService {
			t.Errorf("type = %q, want %q", process.Type, core.TypeService)
		}

		// A service restarts for as long as it is meant to run, so there is no
		// ceiling to report.
		if process.MaxAttempts != nil {
			t.Errorf("max_attempts = %v, want null for a service", *process.MaxAttempts)
		}
	})

	t.Run("a task still reports its ceiling", func(t *testing.T) {
		t.Parallel()

		handler := newLiveServer(t)

		rec := do(t, handler, http.MethodPost, "/v1/processes", `{"worker":"hello","params":{"id":"1"}}`, nil)
		created := decode[createProcessResponse](t, rec)
		process := decode[processResponse](t, do(t, handler, http.MethodGet, "/v1/processes/"+created.ID, "", nil))

		if process.MaxAttempts == nil || *process.MaxAttempts != 1 {
			t.Errorf("max_attempts = %v, want 1", process.MaxAttempts)
		}
	})

	t.Run("a request may not run a service as a task", func(t *testing.T) {
		t.Parallel()

		handler := newLiveServer(t)

		rec := do(t, handler, http.MethodPost, "/v1/processes", `{"worker":"api","type":"task"}`, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("POST returned %d (%s), want 400", rec.Code, rec.Body.String())
		}
	})

	t.Run("an unknown type is refused", func(t *testing.T) {
		t.Parallel()

		handler := newLiveServer(t)

		rec := do(t, handler, http.MethodPost, "/v1/processes", `{"worker":"api","type":"daemon"}`, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("POST returned %d (%s), want 400", rec.Code, rec.Body.String())
		}
	})

	t.Run("a service has no timeout to override", func(t *testing.T) {
		t.Parallel()

		handler := newLiveServer(t)

		rec := do(t, handler, http.MethodPost, "/v1/processes", `{"worker":"api","timeout":"30s"}`, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("POST returned %d (%s), want 400", rec.Code, rec.Body.String())
		}

		if body := decode[errorBody](t, rec); body.Error.Code != "timeout_denied" {
			t.Errorf("error code = %q, want %q", body.Error.Code, "timeout_denied")
		}
	})

	// A service takes a slot at admission or not at all, so a full node answers
	// 503 instead of parking it in the queue (docs/SPEC.md §4).
	t.Run("a full node refuses a service", func(t *testing.T) {
		t.Parallel()

		handler := newLiveServer(t)

		for range 2 {
			rec := do(t, handler, http.MethodPost, "/v1/processes", `{"worker":"sleeper"}`, nil)
			if rec.Code != http.StatusCreated {
				t.Fatalf("POST returned %d (%s), want 201", rec.Code, rec.Body.String())
			}
		}

		rec := do(t, handler, http.MethodPost, "/v1/processes", `{"worker":"api"}`, nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("POST returned %d (%s), want 503", rec.Code, rec.Body.String())
		}

		if body := decode[errorBody](t, rec); body.Error.Code != "no_capacity" {
			t.Errorf("error code = %q, want %q", body.Error.Code, "no_capacity")
		}
	})

	// Stopping a service is not the same as a service failing: it is told to
	// stop and not to come back.
	t.Run("delete stops it for good", func(t *testing.T) {
		t.Parallel()

		handler := newLiveServer(t)

		created := decode[createProcessResponse](t,
			do(t, handler, http.MethodPost, "/v1/processes", `{"worker":"api"}`, nil))

		if rec := do(t, handler, http.MethodDelete, "/v1/processes/"+created.ID, "", nil); rec.Code != http.StatusAccepted {
			t.Fatalf("DELETE returned %d (%s), want 202", rec.Code, rec.Body.String())
		}

		stopped := awaitState(t, handler, created.ID)
		if stopped.Status != core.StateCanceled || stopped.Reason != core.ReasonUserRequest {
			t.Errorf("stopped service is %s/%s, want CANCELED/user_request", stopped.Status, stopped.Reason)
		}
	})
}

// A raw command is already the sharpest edge in the API; supervising one
// forever with no worker definition bounding it is not an edge worth adding.
func TestServer_createProcess_rawServiceDenied(t *testing.T) {
	t.Parallel()

	handler := newLiveServerWith(t, func(opts *Options) {
		opts.Config.ExecutionMode = config.ExecutionModeRaw
		opts.Config.AllowedCommands = []string{"/bin/sleep"}
	})

	rec := do(t, handler, http.MethodPost, "/v1/processes",
		`{"command":"/bin/sleep","args":["30"],"type":"service"}`, nil)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST returned %d (%s), want 403", rec.Code, rec.Body.String())
	}
}
