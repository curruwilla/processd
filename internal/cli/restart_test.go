package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The worker a restart in these tests is built around.
const (
	testWorker = "api"
	testType   = "service"
)

// fakeDaemon answers the endpoints a restart calls, recording what it received.
type fakeDaemon struct {
	mu sync.Mutex

	// statuses is handed out one per GET of the execution, the last one
	// repeating, so a test can make the execution settle after N polls.
	statuses []string
	polls    int

	worker   string
	enabled  bool
	metadata map[string]string

	// createFailures is the number of times the create is answered with
	// no_capacity before it succeeds.
	createFailures int

	stopped  int
	created  []map[string]any
	graceGot string
}

func (d *fakeDaemon) status() string {
	d.mu.Lock()
	defer d.mu.Unlock()

	current := d.statuses[min(d.polls, len(d.statuses)-1)]
	d.polls++

	return current
}

func (d *fakeDaemon) server(t *testing.T) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/processes/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(t, w, http.StatusOK, map[string]any{
			"id":       r.PathValue("id"),
			"worker":   d.worker,
			"type":     testType,
			"status":   d.status(),
			"metadata": d.metadata,
		})
	})

	mux.HandleFunc("DELETE /v1/processes/{id}", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.stopped++
		d.graceGot = r.URL.Query().Get("grace")
		d.mu.Unlock()

		w.WriteHeader(http.StatusAccepted)
	})

	mux.HandleFunc("GET /v1/workers", func(w http.ResponseWriter, _ *http.Request) {
		workers := []map[string]any{}
		if d.worker != "" {
			workers = append(workers, map[string]any{"name": d.worker, "enabled": d.enabled})
		}

		writeTestJSON(t, w, http.StatusOK, workers)
	})

	mux.HandleFunc("POST /v1/processes", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding create request: %v", err)
		}

		d.mu.Lock()
		d.created = append(d.created, body)
		remaining := d.createFailures
		d.createFailures--
		d.mu.Unlock()

		if remaining > 0 {
			writeTestJSON(t, w, http.StatusServiceUnavailable, map[string]any{
				"error": map[string]any{"code": "no_capacity", "message": "no free slot"},
			})

			return
		}

		writeTestJSON(t, w, http.StatusCreated, map[string]any{
			"id": "proc_new", "status": "STARTING", "pid": nil,
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return srv
}

func writeTestJSON(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("writing response: %v", err)
	}
}

func testClient(srv *httptest.Server) *client {
	return &client{baseURL: srv.URL, http: srv.Client()}
}

func TestRestartExecution(t *testing.T) {
	t.Parallel()

	t.Run("stops the execution and recreates it from its worker", func(t *testing.T) {
		t.Parallel()

		daemon := &fakeDaemon{
			statuses: []string{"RUNNING", "STOPPING", "CANCELED"},
			worker:   testWorker,
			enabled:  true,
			metadata: map[string]string{"requested_by": "ops"},
		}

		out := &bytes.Buffer{}

		err := restartExecution(t.Context(), testClient(daemon.server(t)), out, restartOptions{
			id:     "proc_old",
			grace:  "5s",
			params: map[string]string{},
			wait:   time.Minute,
		})
		if err != nil {
			t.Fatalf("restartExecution returned %v, want nil", err)
		}

		if daemon.stopped != 1 {
			t.Errorf("stopped %d executions, want 1", daemon.stopped)
		}

		if daemon.graceGot != "5s" {
			t.Errorf("grace forwarded as %q, want %q", daemon.graceGot, "5s")
		}

		if len(daemon.created) != 1 {
			t.Fatalf("created %d executions, want 1", len(daemon.created))
		}

		created := daemon.created[0]
		if created["worker"] != testWorker || created["type"] != testType {
			t.Errorf("created %v, want worker %q of type %q", created, testWorker, testType)
		}

		if metadata, ok := created["metadata"].(map[string]any); !ok || metadata["requested_by"] != "ops" {
			t.Errorf("created metadata is %v, want the metadata of the previous execution", created["metadata"])
		}

		if want := "proc_old stopped\nproc_new STARTING -\n"; out.String() != want {
			t.Errorf("printed %q, want %q", out.String(), want)
		}
	})

	t.Run("waits for the previous execution to settle", func(t *testing.T) {
		t.Parallel()

		daemon := &fakeDaemon{
			statuses: []string{"RUNNING", "RUNNING", "CANCELED"},
			worker:   testWorker,
			enabled:  true,
		}

		err := restartExecution(t.Context(), testClient(daemon.server(t)), &bytes.Buffer{}, restartOptions{
			id: "proc_old", wait: time.Minute,
		})
		if err != nil {
			t.Fatalf("restartExecution returned %v, want nil", err)
		}

		// One read before the stop, then one per poll until it settled.
		if daemon.polls != 3 {
			t.Errorf("read the execution %d times, want 3", daemon.polls)
		}
	})

	t.Run("gives up when the execution does not stop", func(t *testing.T) {
		t.Parallel()

		daemon := &fakeDaemon{statuses: []string{"RUNNING"}, worker: testWorker, enabled: true}

		err := restartExecution(t.Context(), testClient(daemon.server(t)), &bytes.Buffer{}, restartOptions{
			id: "proc_old", wait: 0,
		})
		if err == nil {
			t.Fatal("restartExecution returned nil, want a failure")
		}

		if !strings.Contains(err.Error(), "was not restarted") {
			t.Errorf("restartExecution returned %v, want it to report that nothing was restarted", err)
		}

		if len(daemon.created) != 0 {
			t.Errorf("created %d executions, want none", len(daemon.created))
		}
	})

	t.Run("retries the create while the node has no free slot", func(t *testing.T) {
		t.Parallel()

		daemon := &fakeDaemon{
			statuses:       []string{"CANCELED"},
			worker:         testWorker,
			enabled:        true,
			createFailures: 2,
		}

		err := restartExecution(t.Context(), testClient(daemon.server(t)), &bytes.Buffer{}, restartOptions{
			id: "proc_old", wait: time.Minute,
		})
		if err != nil {
			t.Fatalf("restartExecution returned %v, want nil", err)
		}

		if len(daemon.created) != 3 {
			t.Errorf("attempted %d creates, want 3", len(daemon.created))
		}
	})

	t.Run("recreates an execution that already ended without stopping it", func(t *testing.T) {
		t.Parallel()

		daemon := &fakeDaemon{statuses: []string{"FAILED"}, worker: testWorker, enabled: true}

		out := &bytes.Buffer{}

		err := restartExecution(t.Context(), testClient(daemon.server(t)), out, restartOptions{
			id: "proc_old", wait: time.Minute,
		})
		if err != nil {
			t.Fatalf("restartExecution returned %v, want nil", err)
		}

		if daemon.stopped != 0 {
			t.Errorf("stopped %d executions, want none: it had already ended", daemon.stopped)
		}

		if want := "proc_new STARTING -\n"; out.String() != want {
			t.Errorf("printed %q, want %q", out.String(), want)
		}
	})

	t.Run("refuses before stopping when the worker is gone", func(t *testing.T) {
		t.Parallel()

		daemon := &fakeDaemon{statuses: []string{"RUNNING"}, worker: testWorker, enabled: false}

		err := restartExecution(t.Context(), testClient(daemon.server(t)), &bytes.Buffer{}, restartOptions{
			id: "proc_old", wait: time.Minute,
		})
		if err == nil {
			t.Fatal("restartExecution returned nil, want a failure")
		}

		if daemon.stopped != 0 {
			t.Errorf("stopped %d executions, want none: a disabled worker cannot be started again", daemon.stopped)
		}
	})

	t.Run("refuses an execution that has no worker", func(t *testing.T) {
		t.Parallel()

		daemon := &fakeDaemon{statuses: []string{"RUNNING"}, enabled: true}

		err := restartExecution(t.Context(), testClient(daemon.server(t)), &bytes.Buffer{}, restartOptions{
			id: "proc_old", wait: time.Minute,
		})
		if err == nil {
			t.Fatal("restartExecution returned nil, want a usage error")
		}

		if daemon.stopped != 0 {
			t.Errorf("stopped %d executions, want none", daemon.stopped)
		}
	})
}

func TestHasCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		code string
		want bool
	}{
		{name: "matching code", err: &apiError{code: "no_capacity"}, code: "no_capacity", want: true},
		{name: "other code", err: &apiError{code: "not_running"}, code: "no_capacity"},
		{name: "not an api failure", err: context.Canceled, code: "no_capacity"},
		{name: "no error", code: "no_capacity"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := hasCode(tt.err, tt.code); got != tt.want {
				t.Errorf("hasCode(%v, %q) = %t, want %t", tt.err, tt.code, got, tt.want)
			}
		})
	}
}
