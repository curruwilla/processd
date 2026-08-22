package api

import (
	"encoding/json"
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
	"github.com/curruwilla/processd/internal/fleet"
)

// submissions records what a node was asked to run.
type submissions struct {
	mu     sync.Mutex
	bodies []string
	keys   []string
}

func (s *submissions) record(body, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.bodies = append(s.bodies, body)
	s.keys = append(s.keys, key)
}

func (s *submissions) last() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.bodies) == 0 {
		return "", ""
	}

	return s.bodies[len(s.bodies)-1], s.keys[len(s.keys)-1]
}

// fakeNodeServer stands in for another processd node.
func fakeNodeServer(t *testing.T, items []map[string]any) *httptest.Server {
	t.Helper()

	server, _ := fakeNodeServerRecording(t, items)

	return server
}

func fakeNodeServerRecording(t *testing.T, items []map[string]any) (*httptest.Server, *submissions) {
	t.Helper()

	received := &submissions{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method == http.MethodPost && r.URL.Path == "/v1/processes" {
			body, _ := io.ReadAll(r.Body)

			received.record(string(body), r.Header.Get("Idempotency-Key"))

			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "proc_remote", "status": "STARTING"})

			return
		}

		switch r.URL.Path {
		case "/v1/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "version": "v1.2.3"})
		case "/v1/stats":
			_ = json.NewEncoder(w).Encode(map[string]any{"slots_used": 1, "slots_max": 4})
		case "/v1/processes":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "next_cursor": "page2"})
		case "/v1/workers":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"name": "remote-worker"}})
		case "/v1/processes/proc_remote/signal":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	t.Cleanup(server.Close)

	return server, received
}

// newHub builds a server that aggregates the given nodes.
func newHub(t *testing.T, nodes map[string]string) http.Handler {
	t.Helper()

	declared := make([]config.FleetNode, 0, len(nodes))

	for name, url := range nodes {
		path := filepath.Join(t.TempDir(), name+".token")
		if err := os.WriteFile(path, []byte("node-token"), 0o600); err != nil {
			t.Fatalf("writing token file: %v", err)
		}

		declared = append(declared, config.FleetNode{Name: name, URL: url, TokenFile: path})
	}

	cfg := config.Fleet{
		Nodes:        declared,
		PollInterval: config.Duration(time.Hour),
		Timeout:      config.Duration(2 * time.Second),
	}

	aggregate, err := fleet.New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("fleet.New() returned %v", err)
	}

	return newLiveServerWith(t, func(opts *Options) { opts.Fleet = aggregate })
}

func authed() map[string]string {
	return map[string]string{"Authorization": "Bearer " + testToken}
}

// A read-only token reads anything it is allowed to see and changes nothing.
func TestReadOnlyToken(t *testing.T) {
	t.Parallel()

	handler := newLiveServerWith(t, func(opts *Options) {
		opts.Config.Auth.Tokens = []config.Token{
			{Name: "test", Hash: HashToken(testToken), ReadOnly: true},
		}
	})

	if got := do(t, handler, http.MethodGet, "/v1/processes", "", authed()).Code; got != http.StatusOK {
		t.Errorf("GET /v1/processes = %d, want 200 for a read-only token", got)
	}

	body := `{"worker":"hello","params":{"id":"1"}}`

	resp := do(t, handler, http.MethodPost, "/v1/processes", body, authed())
	if resp.Code != http.StatusForbidden {
		t.Fatalf("POST /v1/processes = %d, want 403 for a read-only token", resp.Code)
	}

	if !containsCode(t, resp.Body.Bytes(), "read_only_token") {
		t.Errorf("error body does not name the refusal: %s", resp.Body.String())
	}

	if got := do(t, handler, http.MethodPost, "/v1/reload", "", authed()).Code; got != http.StatusForbidden {
		t.Errorf("POST /v1/reload = %d, want 403 for a read-only token", got)
	}

	if got := do(t, handler, http.MethodDelete, "/v1/processes/proc_x", "", authed()).Code; got != http.StatusForbidden {
		t.Errorf("DELETE = %d, want 403 for a read-only token", got)
	}
}

// An ordinary node does not answer the fleet routes at all: the absence is the
// statement that there is no fleet.
func TestFleetRoutes_absentWithoutAFleet(t *testing.T) {
	t.Parallel()

	handler := newLiveServer(t)

	if got := do(t, handler, http.MethodGet, "/v1/fleet/nodes", "", authed()).Code; got != http.StatusNotFound {
		t.Fatalf("GET /v1/fleet/nodes = %d, want 404 on a node that aggregates nothing", got)
	}
}

func TestFleetNodes(t *testing.T) {
	t.Parallel()

	node := fakeNodeServer(t, nil)
	handler := newHub(t, map[string]string{"app-01": node.URL})

	resp := do(t, handler, http.MethodGet, "/v1/fleet/nodes", "", authed())
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
	}

	var statuses []map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &statuses); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if len(statuses) != 1 || statuses[0]["name"] != "app-01" {
		t.Fatalf("nodes = %v", statuses)
	}
}

func TestFleetProxy(t *testing.T) {
	t.Parallel()

	node := fakeNodeServer(t, nil)
	handler := newHub(t, map[string]string{"app-01": node.URL})

	resp := do(t, handler, http.MethodGet, "/v1/fleet/nodes/app-01/workers", "", authed())
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
	}

	if body := resp.Body.String(); !strings.Contains(body, "remote-worker") {
		t.Fatalf("body = %s, want the node's own answer", body)
	}

	if got := do(t, handler, http.MethodGet, "/v1/fleet/nodes/ghost/workers", "", authed()).Code; got != http.StatusNotFound {
		t.Errorf("unknown node = %d, want 404", got)
	}
}

// A merged page tags every row with its origin, because an ID on its own cannot
// be looked up again.
func TestFleetProcesses_merged(t *testing.T) {
	t.Parallel()

	older := map[string]any{"id": "proc_old", "created_at": "2026-08-19T10:00:00Z"}
	newer := map[string]any{"id": "proc_new", "created_at": "2026-08-20T10:00:00Z"}

	first := fakeNodeServer(t, []map[string]any{older})
	second := fakeNodeServer(t, []map[string]any{newer})

	handler := newHub(t, map[string]string{"app-01": first.URL, "app-02": second.URL})

	resp := do(t, handler, http.MethodGet, "/v1/processes?node=*", "", authed())
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
	}

	var page struct {
		Items      []map[string]any `json:"items"`
		NextCursor string           `json:"next_cursor"`
	}

	if err := json.Unmarshal(resp.Body.Bytes(), &page); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if len(page.Items) != 2 {
		t.Fatalf("items = %v, want one from each node", page.Items)
	}

	// Newest first, across nodes.
	if page.Items[0]["id"] != "proc_new" {
		t.Errorf("first item = %v, want the newest across nodes", page.Items[0])
	}

	for _, item := range page.Items {
		if item["node"] == nil {
			t.Errorf("item %v carries no origin", item)
		}
	}

	// A merged page has no cursor to give: there is no ordering across nodes to
	// page through.
	if page.NextCursor != "" {
		t.Errorf("next_cursor = %q, want none for a merged page", page.NextCursor)
	}
}

// One node is asked directly, and keeps its own cursor.
func TestFleetProcesses_singleNodeKeepsItsCursor(t *testing.T) {
	t.Parallel()

	item := map[string]any{"id": "proc_1", "created_at": "2026-08-19T10:00:00Z"}
	node := fakeNodeServer(t, []map[string]any{item})

	handler := newHub(t, map[string]string{"app-01": node.URL})

	resp := do(t, handler, http.MethodGet, "/v1/processes?node=app-01", "", authed())
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}

	var page struct {
		Items      []map[string]any `json:"items"`
		NextCursor string           `json:"next_cursor"`
	}

	if err := json.Unmarshal(resp.Body.Bytes(), &page); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if page.NextCursor != "page2" {
		t.Errorf("next_cursor = %q, want the node's own cursor", page.NextCursor)
	}

	if got := do(t, handler, http.MethodGet, "/v1/processes?node=ghost", "", authed()).Code; got != http.StatusNotFound {
		t.Errorf("unknown node = %d, want 404", got)
	}
}

// One unreachable node degrades the page rather than failing it, and says so.
func TestFleetProcesses_degradesOnAnUnreachableNode(t *testing.T) {
	t.Parallel()

	item := map[string]any{"id": "proc_1", "created_at": "2026-08-19T10:00:00Z"}
	live := fakeNodeServer(t, []map[string]any{item})

	handler := newHub(t, map[string]string{"app-01": live.URL, "app-02": "http://127.0.0.1:1"})

	resp := do(t, handler, http.MethodGet, "/v1/processes?node=*", "", authed())
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want the page to survive one dead node: %s", resp.Code, resp.Body.String())
	}

	var page struct {
		Items       []map[string]any `json:"items"`
		Unreachable []string         `json:"unreachable"`
	}

	if err := json.Unmarshal(resp.Body.Bytes(), &page); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if len(page.Items) != 1 {
		t.Errorf("items = %v, want the reachable node's row", page.Items)
	}

	if len(page.Unreachable) != 1 || page.Unreachable[0] != "app-02" {
		t.Errorf("unreachable = %v, want app-02 named", page.Unreachable)
	}
}

func containsCode(t *testing.T, body []byte, code string) bool {
	t.Helper()

	var decoded struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decoding error body: %v", err)
	}

	return decoded.Error.Code == code
}

// A dispatch is forwarded to the node the client named, and the routing field
// never reaches the node, which has no idea what a fleet is.
func TestDispatchToNode(t *testing.T) {
	t.Parallel()

	node, received := fakeNodeServerRecording(t, nil)
	handler := newHub(t, map[string]string{"app-01": node.URL})

	headers := authed()
	headers["Idempotency-Key"] = "client-key-1"

	body := `{"node":"app-01","worker":"hello","params":{"id":"1"}}`

	resp := do(t, handler, http.MethodPost, "/v1/processes", body, headers)
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.Code, resp.Body.String())
	}

	if got := resp.Header().Get("X-Processd-Node"); got != "app-01" {
		t.Errorf("X-Processd-Node = %q, want the node that ran it", got)
	}

	forwarded, key := received.last()

	if strings.Contains(forwarded, `"node"`) {
		t.Errorf("the routing field reached the node: %s", forwarded)
	}

	if !strings.Contains(forwarded, `"worker":"hello"`) {
		t.Errorf("forwarded body lost the request: %s", forwarded)
	}

	// The client owns the idempotency key: the hub answers "unknown" on a
	// timeout, so the client is the one that retries.
	if key != "client-key-1" {
		t.Errorf("Idempotency-Key = %q, want the client's own key", key)
	}

	// The hub keeps no authoritative copy of what it forwarded. Its own listing
	// stays empty, which is what keeps it a router instead of a scheduler.
	listing := do(t, handler, http.MethodGet, "/v1/processes", "", authed())

	var page struct {
		Items []map[string]any `json:"items"`
	}

	if err := json.Unmarshal(listing.Body.Bytes(), &page); err != nil {
		t.Fatalf("decoding the hub listing: %v", err)
	}

	if len(page.Items) != 0 {
		t.Errorf("the hub recorded %v, want it to keep nothing", page.Items)
	}
}

func TestDispatchToNode_unknownNode(t *testing.T) {
	t.Parallel()

	node := fakeNodeServer(t, nil)
	handler := newHub(t, map[string]string{"app-01": node.URL})

	body := `{"node":"ghost","worker":"hello","params":{"id":"1"}}`

	resp := do(t, handler, http.MethodPost, "/v1/processes", body, authed())
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.Code)
	}
}

// A node that did not answer has not said no. Reporting that as a failure is
// how a caller is talked into running the same work twice.
func TestDispatchToNode_silenceIsNotAFailure(t *testing.T) {
	t.Parallel()

	handler := newHub(t, map[string]string{"app-01": "http://127.0.0.1:1"})

	body := `{"node":"app-01","worker":"hello","params":{"id":"1"}}`

	resp := do(t, handler, http.MethodPost, "/v1/processes", body, authed())
	if resp.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 for an outcome nobody knows", resp.Code)
	}

	if !containsCode(t, resp.Body.Bytes(), "dispatch_unknown") {
		t.Fatalf("body does not report an unknown outcome: %s", resp.Body.String())
	}

	// The message has to tell the caller what to do, because the safe action is
	// the counter-intuitive one.
	if !strings.Contains(resp.Body.String(), "Idempotency-Key") {
		t.Errorf("the refusal does not say how to retry safely: %s", resp.Body.String())
	}
}

// A write is proxied only to the node the client named, and the node decides
// whether the hub's token may make it.
func TestFleetProxy_forwardsAWrite(t *testing.T) {
	t.Parallel()

	node := fakeNodeServer(t, nil)
	handler := newHub(t, map[string]string{"app-01": node.URL})

	resp := do(t, handler, http.MethodPost,
		"/v1/fleet/nodes/app-01/processes/proc_remote/signal", `{"signal":"SIGTERM"}`, authed())

	if resp.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want the node's own answer: %s", resp.Code, resp.Body.String())
	}
}

// A read-only token on the hub cannot dispatch, whatever the node would allow.
func TestDispatchToNode_readOnlyHubToken(t *testing.T) {
	t.Parallel()

	node := fakeNodeServer(t, nil)

	declared := []config.FleetNode{}
	path := filepath.Join(t.TempDir(), "app-01.token")

	if err := os.WriteFile(path, []byte("node-token"), 0o600); err != nil {
		t.Fatalf("writing token file: %v", err)
	}

	declared = append(declared, config.FleetNode{Name: "app-01", URL: node.URL, TokenFile: path})

	aggregate, err := fleet.New(config.Fleet{
		Nodes:        declared,
		PollInterval: config.Duration(time.Hour),
		Timeout:      config.Duration(2 * time.Second),
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("fleet.New() returned %v", err)
	}

	handler := newLiveServerWith(t, func(opts *Options) {
		opts.Fleet = aggregate
		opts.Config.Auth.Tokens = []config.Token{
			{Name: "test", Hash: HashToken(testToken), ReadOnly: true},
		}
	})

	body := `{"node":"app-01","worker":"hello","params":{"id":"1"}}`

	if got := do(t, handler, http.MethodPost, "/v1/processes", body, authed()).Code; got != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: a read-only token must not dispatch", got)
	}
}
