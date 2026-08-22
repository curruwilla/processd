package fleet

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/curruwilla/processd/internal/config"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// fakeNode is a stand-in processd node.
type fakeNode struct {
	mu      sync.Mutex
	tokens  []string
	paths   []string
	version string
	down    bool
}

func (f *fakeNode) server(t *testing.T) *httptest.Server {
	t.Helper()

	f.version = "v9.9.9"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.tokens = append(f.tokens, r.Header.Get("Authorization"))
		f.paths = append(f.paths, r.URL.Path)
		down := f.down
		version := f.version
		f.mu.Unlock()

		if down {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/v1/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "version": version})
		case "/v1/stats":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"slots_used": 2, "slots_max": 10, "running": 2,
				// A counter this hub has never heard of must survive the trip.
				"future_counter": 7,
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"path": r.URL.Path})
		}
	}))

	t.Cleanup(server.Close)

	return server
}

func (f *fakeNode) lastToken() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.tokens) == 0 {
		return ""
	}

	return f.tokens[len(f.tokens)-1]
}

func (f *fakeNode) lastPath() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.paths) == 0 {
		return ""
	}

	return f.paths[len(f.paths)-1]
}

func (f *fakeNode) setDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.down = down
}

func tokenFile(t *testing.T, value string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "node.token")
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		t.Fatalf("writing token file: %v", err)
	}

	return path
}

func newFleet(t *testing.T, name, url, token string) (*Fleet, string) {
	t.Helper()

	path := tokenFile(t, token)

	cfg := config.Fleet{
		Nodes:        []config.FleetNode{{Name: name, URL: url, TokenFile: path}},
		PollInterval: config.Duration(time.Hour),
		Timeout:      config.Duration(2 * time.Second),
	}

	f, err := New(cfg, quietLogger())
	if err != nil {
		t.Fatalf("New() returned %v", err)
	}

	if f == nil {
		t.Fatal("New() returned no fleet for a configured one")
	}

	return f, path
}

// An ordinary node aggregates nothing, and the absence has to be explicit so
// the fleet routes never reach the mux.
func TestNew_withoutNodes(t *testing.T) {
	t.Parallel()

	f, err := New(config.Fleet{}, quietLogger())
	if err != nil {
		t.Fatalf("New() returned %v", err)
	}

	if f != nil {
		t.Fatal("New() built a fleet for a daemon that aggregates nothing")
	}
}

// A token that cannot be read now will not start working later.
func TestNew_missingTokenFile(t *testing.T) {
	t.Parallel()

	cfg := config.Fleet{
		Nodes:        []config.FleetNode{{Name: "app-01", URL: "http://127.0.0.1:1", TokenFile: "/nonexistent/token"}},
		PollInterval: config.Duration(time.Second),
		Timeout:      config.Duration(time.Second),
	}

	if _, err := New(cfg, quietLogger()); err == nil {
		t.Fatal("New() accepted a node whose token cannot be read")
	}
}

func TestFleet_pollRecordsTheNode(t *testing.T) {
	t.Parallel()

	node := &fakeNode{}
	server := node.server(t)

	f, _ := newFleet(t, "app-01", server.URL, "node-token")
	f.pollAll(t.Context())

	statuses := f.Statuses()
	if len(statuses) != 1 {
		t.Fatalf("Statuses() returned %d entries, want 1", len(statuses))
	}

	status := statuses[0]

	if !status.Reachable {
		t.Fatalf("node is not reachable: %s", status.Error)
	}

	if status.Version != "v9.9.9" {
		t.Errorf("version = %q", status.Version)
	}

	if status.LastSeen == nil {
		t.Error("last_seen is nil after a successful poll")
	}

	// Tolerant decoding: a counter from a newer node has to survive.
	if status.Stats["future_counter"] != float64(7) {
		t.Errorf("stats = %v, want the unknown counter kept", status.Stats)
	}

	if got := node.lastToken(); got != "Bearer node-token" {
		t.Errorf("authorization = %q, want the hub's node token", got)
	}
}

// An unreachable node keeps what was last known about it, and says why it is
// unreachable. The worst a stale panel can do is be stale.
func TestFleet_unreachableNodeKeepsItsLastSighting(t *testing.T) {
	t.Parallel()

	node := &fakeNode{}
	server := node.server(t)

	f, _ := newFleet(t, "app-01", server.URL, "node-token")
	f.pollAll(t.Context())

	firstSeen := f.Statuses()[0].LastSeen
	if firstSeen == nil {
		t.Fatal("the first poll recorded no sighting")
	}

	node.setDown(true)
	f.pollAll(t.Context())

	status := f.Statuses()[0]

	if status.Reachable {
		t.Fatal("node still reports as reachable")
	}

	if status.Error == "" {
		t.Error("an unreachable node reports no reason")
	}

	if status.LastSeen == nil || !status.LastSeen.Equal(*firstSeen) {
		t.Errorf("last_seen = %v, want the previous sighting %v", status.LastSeen, firstSeen)
	}

	if status.Version != "v9.9.9" {
		t.Errorf("version = %q, want the last known one", status.Version)
	}
}

// A node that stops answering is one fact, and the poll repeats every few
// seconds. Logging it per poll buries everything else: a node down for a day at
// ten-second intervals is eight thousand copies of the same line.
func TestFleet_logsReachabilityChangesOnly(t *testing.T) {
	t.Parallel()

	node := &fakeNode{}
	server := node.server(t)

	var lines countingHandler

	f, _ := newFleet(t, "app-01", server.URL, "node-token")
	f.log = slog.New(&lines)

	f.pollAll(t.Context())

	node.setDown(true)
	f.pollAll(t.Context())

	first := lines.count()
	if first == 0 {
		t.Fatal("the node going down was not logged at all")
	}

	// Three more polls with nothing new to say.
	for range 3 {
		f.pollAll(t.Context())
	}

	if got := lines.count(); got != first {
		t.Errorf("a node that stayed down logged %d more lines, want none", got-first)
	}

	node.setDown(false)
	f.pollAll(t.Context())

	if lines.count() <= first {
		t.Error("the node coming back was not logged, want the recovery reported")
	}
}

// countingHandler counts the records a logger was asked to emit.
type countingHandler struct {
	mu      sync.Mutex
	records int
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *countingHandler) Handle(context.Context, slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.records++

	return nil
}

func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

func (h *countingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.records
}

// Rotating a node's token takes effect on the next poll, not at the next
// restart of the hub.
func TestFleet_reReadsTheTokenOnEveryPoll(t *testing.T) {
	t.Parallel()

	node := &fakeNode{}
	server := node.server(t)

	f, path := newFleet(t, "app-01", server.URL, "old-token")
	f.pollAll(t.Context())

	if got := node.lastToken(); got != "Bearer old-token" {
		t.Fatalf("authorization = %q", got)
	}

	if err := os.WriteFile(path, []byte("new-token\n"), 0o600); err != nil {
		t.Fatalf("rotating the token: %v", err)
	}

	f.pollAll(t.Context())

	if got := node.lastToken(); got != "Bearer new-token" {
		t.Fatalf("authorization = %q, want the rotated token", got)
	}
}

// The proxy authenticates as the hub, never as the caller.
func TestFleet_proxyReplacesTheCallerCredential(t *testing.T) {
	t.Parallel()

	node := &fakeNode{}
	server := node.server(t)

	f, _ := newFleet(t, "app-01", server.URL, "node-token")

	proxy, ok := f.Proxy("app-01")
	if !ok {
		t.Fatal("Proxy() reported no proxy for a configured node")
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/workers", nil)
	req.Header.Set("Authorization", "Bearer the-clients-own-token")

	proxy.ServeHTTP(httptest.NewRecorder(), req)

	if got := node.lastToken(); got != "Bearer node-token" {
		t.Fatalf("authorization = %q, want the hub's token", got)
	}

	if got := node.lastPath(); got != "/v1/workers" {
		t.Fatalf("path = %q", got)
	}
}

func TestFleet_proxyReportsAnUnreachableNode(t *testing.T) {
	t.Parallel()

	// A port nothing listens on.
	f, _ := newFleet(t, "app-01", "http://127.0.0.1:1", "node-token")

	proxy, _ := f.Proxy("app-01")
	recorder := httptest.NewRecorder()

	proxy.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/workers", nil))

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}
}

func TestFleet_unknownNode(t *testing.T) {
	t.Parallel()

	node := &fakeNode{}
	f, _ := newFleet(t, "app-01", node.server(t).URL, "node-token")

	if f.Has("ghost") {
		t.Error("Has() accepted a node that is not configured")
	}

	if _, ok := f.Proxy("ghost"); ok {
		t.Error("Proxy() built a proxy for a node that is not configured")
	}

	resp, err := f.Get(t.Context(), "ghost", "/v1/health", nil)
	if err == nil {
		_ = resp.Body.Close()

		t.Error("Get() called a node that is not configured")
	}
}

func TestTargetPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		tail string
		want string
		ok   bool
	}{
		{name: "plain", tail: "processes", want: "/v1/processes", ok: true},
		{name: "leading slash", tail: "/processes", want: "/v1/processes", ok: true},
		{name: "nested", tail: "processes/proc_1/logs", want: "/v1/processes/proc_1/logs", ok: true},
		{name: "climbing out is refused", tail: "../secret", ok: false},
		{name: "climbing out the long way is refused", tail: "processes/../../secret", ok: false},
		{name: "root", tail: "", want: "/v1", ok: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := TargetPath(tt.tail)

			if ok != tt.ok {
				t.Fatalf("TargetPath(%q) ok = %v, want %v", tt.tail, ok, tt.ok)
			}

			if ok && got != tt.want {
				t.Fatalf("TargetPath(%q) = %q, want %q", tt.tail, got, tt.want)
			}
		})
	}
}
