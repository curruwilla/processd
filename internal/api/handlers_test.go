package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/curruwilla/processd/internal/config"
	"github.com/curruwilla/processd/internal/core"
	"github.com/curruwilla/processd/internal/logstore"
	"github.com/curruwilla/processd/internal/metrics"
	"github.com/curruwilla/processd/internal/queue"
	"github.com/curruwilla/processd/internal/runner"
	"github.com/curruwilla/processd/internal/store"
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
  - name: blinker
    type: service
    command: /bin/true
    cwd: /tmp
    retry:
      backoff: {type: fixed, initial: 5s, max: 5s, jitter: 0}
    logs:
      rotate:
        max_files: 2
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

	handler, _ := newLiveNode(t, nil, tune)

	return handler
}

// newLiveNode builds the same graph and hands back the store with it, for the
// tests that check what the handlers wrote rather than what they answered. The
// configuration hook runs before the graph is built, since the components read
// it once and keep their own copy.
func newLiveNode(t *testing.T, tuneConfig func(*config.Config), tune func(*Options)) (http.Handler, store.Store) {
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

	if tuneConfig != nil {
		tuneConfig(&cfg)
	}

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

	return New(opts).Handler(), db
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

// A client that timed out and retried is what the key exists for, and its first
// request is often still in flight when the second one arrives. Both find no
// record, so the key has to be a claim taken before the work rather than a note
// written after it.
func TestServer_createProcess_idempotencyUnderConcurrency(t *testing.T) {
	t.Parallel()

	handler := newLiveServer(t)
	body := `{"worker":"hello","params":{"id":"7"}}`
	headers := map[string]string{idempotencyHeader: "key-racing"}

	const copies = 8

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		ids       = map[string]bool{}
		conflicts int
	)

	for range copies {
		wg.Go(func() {
			response := do(t, handler, http.MethodPost, "/v1/processes", body, headers)

			mu.Lock()
			defer mu.Unlock()

			switch response.Code {
			case http.StatusCreated, http.StatusAccepted, http.StatusOK:
				ids[decode[createProcessResponse](t, response).ID] = true
			case http.StatusConflict:
				// The winner had the key but had not recorded its execution yet.
				conflicts++
			default:
				t.Errorf("POST returned %d (%s)", response.Code, response.Body.String())
			}
		})
	}

	wg.Wait()

	if len(ids) != 1 {
		t.Errorf("the same key produced %d executions, want 1: %v", len(ids), ids)
	}

	listed := decode[listResponse](t, do(t, handler, http.MethodGet, "/v1/processes", "", nil))
	if len(listed.Items) != 1 {
		t.Errorf("the node holds %d executions, want 1", len(listed.Items))
	}
}

// A request refused before it ran must leave its key free: a queue that was
// full a moment ago is a reason to repeat the request, not to refuse it for as
// long as the history is kept.
func TestServer_createProcess_idempotencyReleasedWhenNothingRan(t *testing.T) {
	t.Parallel()

	handler, db := newLiveNode(t, func(cfg *config.Config) {
		cfg.MaxProcesses = 1
		cfg.Queue.MaxDepth = 1
	}, nil)

	// One execution takes the only slot, a second fills the only queue place.
	for _, id := range []string{"1", "2"} {
		seeded := do(t, handler, http.MethodPost, "/v1/processes", `{"worker":"sleeper"}`, nil)
		if seeded.Code != http.StatusCreated && seeded.Code != http.StatusAccepted {
			t.Fatalf("seeding %s returned %d (%s)", id, seeded.Code, seeded.Body.String())
		}
	}

	headers := map[string]string{idempotencyHeader: "key-refused"}

	refused := do(t, handler, http.MethodPost, "/v1/processes", `{"worker":"hello","params":{"id":"7"}}`, headers)
	if refused.Code != http.StatusTooManyRequests {
		t.Fatalf("POST into a full queue returned %d (%s), want 429", refused.Code, refused.Body.String())
	}

	if _, err := db.FindIdempotency(t.Context(), "key-refused"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("FindIdempotency() returned %v, want the claim released with core.ErrNotFound", err)
	}
}

// A key outlives nothing: once the execution it replays has been purged, the
// request is new again. Answering "your own key is unknown" would refuse a
// request the client is entitled to repeat.
func TestServer_createProcess_idempotencyAfterThePurge(t *testing.T) {
	t.Parallel()

	handler, db := newLiveNode(t, nil, nil)

	body := `{"worker":"hello","params":{"id":"7"}}`
	headers := map[string]string{idempotencyHeader: "key-purged"}

	first := do(t, handler, http.MethodPost, "/v1/processes", body, headers)
	if first.Code != http.StatusCreated {
		t.Fatalf("first POST returned %d (%s), want 201", first.Code, first.Body.String())
	}

	created := decode[createProcessResponse](t, first)
	awaitState(t, handler, created.ID)

	if _, err := db.PurgeHistory(t.Context(), time.Now().UTC().Add(time.Hour), 0); err != nil {
		t.Fatalf("PurgeHistory() returned %v, want nil", err)
	}

	// The key went with the execution it pointed at.
	if _, err := db.FindIdempotency(t.Context(), "key-purged"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("FindIdempotency() returned %v, want the key purged with its execution", err)
	}

	again := do(t, handler, http.MethodPost, "/v1/processes", body, headers)
	if again.Code != http.StatusCreated {
		t.Fatalf("repeating the request returned %d (%s), want 201", again.Code, again.Body.String())
	}

	if decode[createProcessResponse](t, again).ID == created.ID {
		t.Error("the repeat replayed a purged execution, want a new one")
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

// Fail closed: a raw command has no worker, so the fields only a worker can
// answer for are refused rather than accepted and dropped. Ignoring them
// silently would run something other than what was asked for.
func TestServer_createProcess_rawRefusesWorkerFields(t *testing.T) {
	t.Parallel()

	handler := newLiveServerWith(t, func(opts *Options) {
		opts.Config.ExecutionMode = config.ExecutionModeRaw
		opts.Config.AllowedCommands = []string{"/bin/echo"}
	})

	tests := []struct {
		name string
		body string
		code string
	}{
		{
			name: "a worker as well as a command",
			body: `{"command":"/bin/echo","worker":"hello"}`,
			code: "command_and_worker",
		},
		{
			name: "params nothing declared",
			body: `{"command":"/bin/echo","params":{"id":"7"}}`,
			code: "params_denied",
		},
		{
			name: "a timeout no worker allowed",
			body: `{"command":"/bin/echo","timeout":"5s"}`,
			code: "timeout_denied",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := do(t, handler, http.MethodPost, "/v1/processes", tt.body, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("POST returned %d (%s), want 400", rec.Code, rec.Body.String())
			}

			if got := decode[errorBody](t, rec).Error.Code; got != tt.code {
				t.Errorf("error code = %q, want %q", got, tt.code)
			}
		})
	}
}

// The console is a client of these fields: without them a service is invisible
// on a dashboard, because a healthy one produces no terminal state at all.
func TestServer_serviceObservability(t *testing.T) {
	t.Parallel()

	t.Run("a running attempt reports uptime instead of nothing", func(t *testing.T) {
		t.Parallel()

		handler := newLiveServer(t)

		created := decode[createProcessResponse](t,
			do(t, handler, http.MethodPost, "/v1/processes", `{"worker":"api"}`, nil))

		process := awaitRunning(t, handler, created.ID)

		if process.DurationMS == nil {
			t.Fatal("duration_ms is null while the service runs, want its uptime")
		}

		if process.FinishedAt != nil {
			t.Error("finished_at is set on a running service, want nil")
		}
	})

	t.Run("restarts are counted per execution", func(t *testing.T) {
		t.Parallel()

		handler := newLiveServer(t)

		created := decode[createProcessResponse](t,
			do(t, handler, http.MethodPost, "/v1/processes", `{"worker":"blinker"}`, nil))

		process := awaitRestart(t, handler, created.ID)

		if process.Restarts < 1 {
			t.Errorf("restarts = %d, want at least 1", process.Restarts)
		}

		// A service in backoff has to say when it comes back: a static RETRYING
		// is not something an operator can act on.
		if process.RetryAt == nil {
			t.Error("retry_at is null while the service waits out its backoff")
		}
	})

	t.Run("stats separate services from the queue", func(t *testing.T) {
		t.Parallel()

		handler := newLiveServer(t)

		created := decode[createProcessResponse](t,
			do(t, handler, http.MethodPost, "/v1/processes", `{"worker":"blinker"}`, nil))

		awaitRestart(t, handler, created.ID)

		stats := decode[statsResponse](t, do(t, handler, http.MethodGet, "/v1/stats", "", nil))

		if stats.Services.Restarting < 1 {
			t.Errorf("services.restarting = %d, want at least 1", stats.Services.Restarting)
		}

		if stats.Services.Restarts < 1 {
			t.Errorf("services.restarts = %d, want at least 1", stats.Services.Restarts)
		}

		// The restarting service holds its slot, so it is not waiting for one.
		if stats.QueueDepth != 0 {
			t.Errorf("queue_depth = %d, want 0: a restarting service is not queued", stats.QueueDepth)
		}
	})

	t.Run("the listing filters by type", func(t *testing.T) {
		t.Parallel()

		handler := newLiveServer(t)

		do(t, handler, http.MethodPost, "/v1/processes", `{"worker":"hello","params":{"id":"1"}}`, nil)
		do(t, handler, http.MethodPost, "/v1/processes", `{"worker":"api"}`, nil)

		services := decode[listResponse](t, do(t, handler, http.MethodGet, "/v1/processes?type=service", "", nil))
		if len(services.Items) != 1 || services.Items[0].Type != core.TypeService {
			t.Errorf("type=service returned %d items, want only the service", len(services.Items))
		}

		tasks := decode[listResponse](t, do(t, handler, http.MethodGet, "/v1/processes?type=task", "", nil))
		if len(tasks.Items) != 1 || tasks.Items[0].Type != core.TypeTask {
			t.Errorf("type=task returned %d items, want only the task", len(tasks.Items))
		}

		rec := do(t, handler, http.MethodGet, "/v1/processes?type=daemon", "", nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("type=daemon returned %d, want 400", rec.Code)
		}
	})
}

// awaitRestart polls until the execution is waiting out a backoff.
func awaitRestart(t *testing.T, handler http.Handler, id string) processResponse {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		rec := do(t, handler, http.MethodGet, "/v1/processes/"+id, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET process returned %d, want 200", rec.Code)
		}

		process := decode[processResponse](t, rec)
		if process.Status == core.StateRetrying {
			return process
		}

		if process.Status.IsTerminal() {
			t.Fatalf("execution reached %s instead of restarting", process.Status)
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatal("execution never reached RETRYING")

	return processResponse{}
}
