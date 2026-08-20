package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/curruwilla/processd/internal/core"
	"github.com/curruwilla/processd/internal/webui"
)

func TestServer_health_deep(t *testing.T) {
	t.Parallel()

	handler := newLiveServer(t)

	rec := do(t, handler, http.MethodGet, "/v1/health?deep=1", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/health?deep=1 returned %d (%s), want 200", rec.Code, rec.Body.String())
	}

	health := decode[healthResponse](t, rec)
	if health.Status != "ok" || health.Store != "ok" {
		t.Errorf("health = %+v, want an ok status and an ok store", health)
	}

	shallow := decode[healthResponse](t, do(t, handler, http.MethodGet, "/v1/health", "", nil))
	if shallow.Store != "" {
		t.Errorf("shallow health reported the store as %q, want it untouched", shallow.Store)
	}
}

func TestServer_stats(t *testing.T) {
	t.Parallel()

	handler := newLiveServer(t)

	created := decode[createProcessResponse](t,
		do(t, handler, http.MethodPost, "/v1/processes", `{"worker":"hello","params":{"id":"9"}}`, nil))
	awaitState(t, handler, created.ID)

	stats := decode[statsResponse](t, do(t, handler, http.MethodGet, "/v1/stats", "", nil))

	if stats.SlotsMax != 2 {
		t.Errorf("slots_max = %d, want 2", stats.SlotsMax)
	}

	if stats.Workers == 0 {
		t.Errorf("workers = %d, want the loaded definitions", stats.Workers)
	}

	// Terminal executions are history: the endpoint reports what is still
	// active, so a completed run leaves no state behind.
	if len(stats.States) != 0 {
		t.Errorf("states = %v, want only non-terminal executions", stats.States)
	}
}

func TestServer_metrics(t *testing.T) {
	t.Parallel()

	handler := newLiveServer(t)

	created := decode[createProcessResponse](t,
		do(t, handler, http.MethodPost, "/v1/processes", `{"worker":"hello","params":{"id":"11"}}`, nil))
	awaitState(t, handler, created.ID)

	rec := do(t, handler, http.MethodGet, "/v1/metrics", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/metrics returned %d, want 200", rec.Code)
	}

	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type = %q, want the Prometheus text format", got)
	}

	body := rec.Body.String()

	want := []string{
		"processd_daemon_up 1",
		"processd_slots_max 2",
		"processd_queue_depth 0",
		`processd_processes{state="COMPLETED"} 1`,
		`processd_process_attempts_total{worker="hello"} 1`,
		`processd_processes_total{worker="hello",status="COMPLETED"} 1`,
		`processd_process_duration_seconds_count{worker="hello"} 1`,
		"processd_processes_running",
		"processd_running_rss_bytes",
	}

	for _, line := range want {
		if !strings.Contains(body, line) {
			t.Errorf("exposition is missing %q, got:\n%s", line, body)
		}
	}
}

func TestServer_usageOfRunningExecution(t *testing.T) {
	t.Parallel()

	handler := newLiveServer(t)

	created := decode[createProcessResponse](t,
		do(t, handler, http.MethodPost, "/v1/processes", `{"worker":"sleeper"}`, nil))

	t.Cleanup(func() {
		do(t, handler, http.MethodDelete, "/v1/processes/"+created.ID+"?grace=0s", "", nil)
	})

	process := awaitRunning(t, handler, created.ID)
	if process.Usage == nil {
		t.Fatal("a running execution carries no usage sample")
	}

	if process.Usage.RSSBytes <= 0 {
		t.Errorf("rss_bytes = %d, want the memory of a live process", process.Usage.RSSBytes)
	}

	if process.Usage.Threads < 1 {
		t.Errorf("threads = %d, want at least one", process.Usage.Threads)
	}
}

func TestServer_consoleIsPublicButTheAPIIsNot(t *testing.T) {
	t.Parallel()

	console, err := webui.Handler()
	if err != nil {
		t.Fatalf("webui.Handler() returned %v, want nil", err)
	}

	handler := newLiveServerWith(t, func(opts *Options) { opts.UI = console })

	page := withoutToken(t, handler, "/ui/")
	if page.Code != http.StatusOK {
		t.Errorf("GET /ui/ returned %d, want the console without a token", page.Code)
	}

	root := withoutToken(t, handler, "/")
	if root.Code != http.StatusFound {
		t.Errorf("GET / returned %d, want a redirect to the console", root.Code)
	}

	api := withoutToken(t, handler, "/v1/stats")
	if api.Code != http.StatusUnauthorized {
		t.Errorf("GET /v1/stats without a token returned %d, want 401", api.Code)
	}
}

func TestServer_consoleIsOffByDefault(t *testing.T) {
	t.Parallel()

	handler := newLiveServer(t)

	if rec := withoutToken(t, handler, "/ui/"); rec.Code == http.StatusOK {
		t.Errorf("GET /ui/ returned %d with no console wired, want a refusal", rec.Code)
	}
}

// awaitRunning polls until the execution is actually running.
func awaitRunning(t *testing.T, handler http.Handler, id string) processResponse {
	t.Helper()

	for range 200 {
		process := decode[processResponse](t, do(t, handler, http.MethodGet, "/v1/processes/"+id, "", nil))
		if process.Status == core.StateRunning {
			return process
		}

		if process.Status.IsTerminal() {
			t.Fatalf("execution reached %s before it could be sampled", process.Status)
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatal("execution never reached RUNNING")

	return processResponse{}
}

// withoutToken issues an unauthenticated request, which is how a browser first
// reaches the console.
func withoutToken(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))

	return rec
}
