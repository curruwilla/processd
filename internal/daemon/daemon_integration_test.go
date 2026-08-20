//go:build integration

// Package daemon's integration test drives the assembled daemon over HTTP: real
// config files, real SQLite, real processes. Run it with:
//
//	go test -tags=integration ./internal/daemon/
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/curruwilla/processd/internal/api"
	"github.com/curruwilla/processd/internal/config"
	"github.com/curruwilla/processd/internal/core"
)

const token = "integration-secret"

const workersFile = `
version: 1
workers:
  - name: hello
    command: /bin/echo
    args: ["hello", "{{id}}"]
    params:
      id: {required: true, pattern: "^[0-9]+$"}
    cwd: /tmp
    lock: "hello:{{id}}"
  - name: flaky
    command: /bin/false
    cwd: /tmp
    retry:
      enabled: true
      max_attempts: 2
      backoff: {type: fixed, initial: 50ms, max: 50ms, jitter: 0}
  - name: sleeper
    command: /bin/sleep
    args: ["30"]
    cwd: /tmp
    kill_grace: 1s
`

type testDaemon struct {
	t       *testing.T
	baseURL string
	cfg     config.Config
	stop    context.CancelFunc
	done    chan error
}

func freePort(t *testing.T) int {
	t.Helper()

	var config net.ListenConfig

	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T, want *net.TCPAddr", listener.Addr())
	}

	if err := listener.Close(); err != nil {
		t.Fatalf("releasing the port: %v", err)
	}

	return addr.Port
}

// newConfig writes the on-disk layout a daemon needs and returns its config.
func newConfig(t *testing.T) config.Config {
	t.Helper()

	root := t.TempDir()
	workersDir := filepath.Join(root, "workers.d")

	if err := os.MkdirAll(workersDir, 0o750); err != nil {
		t.Fatalf("creating workers dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(workersDir, "w.yaml"), []byte(workersFile), 0o600); err != nil {
		t.Fatalf("writing workers: %v", err)
	}

	cfg := config.Default()
	cfg.Listen = fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cfg.DataDir = filepath.Join(root, "data")
	cfg.LogDir = filepath.Join(root, "logs")
	cfg.WorkersDir = workersDir
	cfg.MaxProcesses = 2
	cfg.ShutdownGrace = config.Duration(2 * time.Second)
	cfg.AllowRootProcesses = true
	cfg.Auth.Tokens = []config.Token{{Name: "integration", Hash: api.HashToken(token)}}

	return cfg
}

// start boots a daemon with the given config and waits for it to serve.
func start(t *testing.T, cfg config.Config) *testDaemon {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	d, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		cancel()
		t.Fatalf("New() returned %v, want nil", err)
	}

	daemon := &testDaemon{
		t:       t,
		baseURL: "http://" + cfg.Listen,
		cfg:     cfg,
		stop:    cancel,
		done:    make(chan error, 1),
	}

	go func() {
		runErr := d.Run(ctx)
		_ = d.Close()

		daemon.done <- runErr
	}()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, daemon.baseURL+"/v1/health", nil)
		if err != nil {
			cancel()
			t.Fatalf("building health request: %v", err)
		}

		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			return daemon
		}

		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	t.Fatal("daemon did not start serving")

	return nil
}

// shutdown stops the daemon and asserts it exited cleanly.
func (d *testDaemon) shutdown() {
	d.t.Helper()

	d.stop()

	select {
	case err := <-d.done:
		if err != nil {
			d.t.Errorf("Run() returned %v, want nil", err)
		}
	case <-time.After(15 * time.Second):
		d.t.Fatal("daemon did not shut down")
	}
}

func (d *testDaemon) request(method, path, body string) *http.Response {
	d.t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	req, err := http.NewRequestWithContext(d.t.Context(), method, d.baseURL+path, reader)
	if err != nil {
		d.t.Fatalf("building request: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)

	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		d.t.Fatalf("calling %s %s: %v", method, path, err)
	}

	return resp
}

func decodeBody[T any](t *testing.T, resp *http.Response, wantStatus int) T {
	t.Helper()

	defer func() {
		_ = resp.Body.Close()
	}()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response: %v", err)
	}

	if resp.StatusCode != wantStatus {
		t.Fatalf("status = %d (%s), want %d", resp.StatusCode, raw, wantStatus)
	}

	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding %q: %v", raw, err)
	}

	return out
}

type processView struct {
	ID       string     `json:"id"`
	Status   core.State `json:"status"`
	Reason   string     `json:"reason"`
	PID      *int       `json:"pid"`
	Attempt  int        `json:"attempt"`
	ExitCode *int       `json:"exit_code"`
}

func (d *testDaemon) submit(body string) processView {
	d.t.Helper()

	resp := d.request(http.MethodPost, "/v1/processes", body)
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		d.t.Fatalf("submit returned %d (%s), want 201 or 202", resp.StatusCode, raw)
	}

	var created processView
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		d.t.Fatalf("decoding submission: %v", err)
	}

	return created
}

func (d *testDaemon) get(id string) processView {
	d.t.Helper()

	//nolint:bodyclose // decodeBody closes the response body
	return decodeBody[processView](d.t, d.request(http.MethodGet, "/v1/processes/"+id, ""), http.StatusOK)
}

func (d *testDaemon) awaitTerminal(id string) processView {
	d.t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		process := d.get(id)
		if process.Status.IsTerminal() {
			return process
		}

		time.Sleep(25 * time.Millisecond)
	}

	d.t.Fatalf("execution %s never reached a terminal state", id)

	return processView{}
}

func TestDaemon_runsExecutionEndToEnd(t *testing.T) {
	t.Parallel()

	daemon := start(t, newConfig(t))
	defer daemon.shutdown()

	created := daemon.submit(`{"worker":"hello","params":{"id":"42"}}`)

	finished := daemon.awaitTerminal(created.ID)
	if finished.Status != core.StateCompleted {
		t.Fatalf("execution ended as %s, want %s", finished.Status, core.StateCompleted)
	}

	if finished.ExitCode == nil || *finished.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", finished.ExitCode)
	}

	//nolint:bodyclose // decodeBody closes the response body
	logs := decodeBody[struct {
		Lines []string `json:"lines"`
	}](t, daemon.request(http.MethodGet, "/v1/processes/"+created.ID+"/logs", ""), http.StatusOK)

	if len(logs.Lines) != 1 || !strings.Contains(logs.Lines[0], "hello 42") {
		t.Errorf("logs = %q, want the captured output", logs.Lines)
	}
}

func TestDaemon_retriesUntilItFails(t *testing.T) {
	t.Parallel()

	daemon := start(t, newConfig(t))
	defer daemon.shutdown()

	created := daemon.submit(`{"worker":"flaky"}`)

	finished := daemon.awaitTerminal(created.ID)
	if finished.Status != core.StateFailed {
		t.Fatalf("execution ended as %s, want %s", finished.Status, core.StateFailed)
	}

	if finished.Attempt != 2 {
		t.Errorf("attempt = %d, want the configured 2", finished.Attempt)
	}

	if finished.Reason != "max_attempts" {
		t.Errorf("reason = %q, want max_attempts", finished.Reason)
	}
}

func TestDaemon_queuesBeyondTheNodeLimit(t *testing.T) {
	t.Parallel()

	daemon := start(t, newConfig(t))
	defer daemon.shutdown()

	ids := []string{}
	for range 3 {
		ids = append(ids, daemon.submit(`{"worker":"sleeper"}`).ID)
	}

	states := map[core.State]int{}
	for _, id := range ids {
		states[daemon.get(id).Status]++
	}

	if states[core.StateQueued] != 1 {
		t.Errorf("states = %v, want exactly one queued execution", states)
	}

	// Stopping a running execution must let the queued one through.
	for _, id := range ids {
		if process := daemon.get(id); process.Status != core.StateQueued {
			resp := daemon.request(http.MethodDelete, "/v1/processes/"+id+"?grace=1s", "")
			_ = resp.Body.Close()

			break
		}
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		queued := 0

		for _, id := range ids {
			if daemon.get(id).Status == core.StateQueued {
				queued++
			}
		}

		if queued == 0 {
			return
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Error("queued execution never started after a slot freed up")
}

func TestDaemon_recoversAfterRestart(t *testing.T) {
	t.Parallel()

	cfg := newConfig(t)
	daemon := start(t, cfg)

	created := daemon.submit(`{"worker":"sleeper"}`)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && daemon.get(created.ID).PID == nil {
		time.Sleep(25 * time.Millisecond)
	}

	running := daemon.get(created.ID)
	if running.PID == nil {
		t.Fatal("execution never reported a pid")
	}

	// A clean shutdown cancels it; the restart must then reconcile the state
	// left behind rather than leaving it RUNNING forever.
	daemon.shutdown()

	restarted := start(t, cfg)
	defer restarted.shutdown()

	recovered := restarted.get(created.ID)
	if !recovered.Status.IsTerminal() {
		t.Errorf("execution is %s after the restart, want a terminal state", recovered.Status)
	}

	if _, err := os.Stat(fmt.Sprintf("/proc/%d", *running.PID)); err == nil {
		t.Errorf("pid %d is still alive after shutdown", *running.PID)
	}
}

func TestDaemon_streamsAttemptLogs(t *testing.T) {
	t.Parallel()

	daemon := start(t, newConfig(t))
	defer daemon.shutdown()

	created := daemon.submit(`{"worker":"hello","params":{"id":"77"}}`)
	daemon.awaitTerminal(created.ID)

	resp := daemon.request(http.MethodGet, "/v1/processes/"+created.ID+"/logs/stream?tail=0", "")

	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream returned %d, want 200", resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the stream: %v", err)
	}

	body := string(raw)

	if !strings.Contains(body, `"text":"hello 77"`) {
		t.Errorf("stream carried no output line:\n%s", body)
	}

	if !strings.Contains(body, "event: end") || !strings.Contains(body, `"status":"COMPLETED"`) {
		t.Errorf("stream did not report how the attempt ended:\n%s", body)
	}
}

// TestDaemon_streamEndsWithTheAttempt follows an execution that is still
// running, over a real connection: nothing is buffered until the end, and the
// stream closes on its own once the attempt is stopped.
func TestDaemon_streamEndsWithTheAttempt(t *testing.T) {
	t.Parallel()

	daemon := start(t, newConfig(t))
	defer daemon.shutdown()

	created := daemon.submit(`{"worker":"sleeper"}`)

	resp := daemon.request(http.MethodGet, "/v1/processes/"+created.ID+"/logs/stream", "")

	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream returned %d, want 200", resp.StatusCode)
	}

	stopped := daemon.request(http.MethodDelete, "/v1/processes/"+created.ID+"?grace=0s", "")
	_ = stopped.Body.Close()

	done := make(chan string, 1)

	go func() {
		raw, _ := io.ReadAll(resp.Body)
		done <- string(raw)
	}()

	select {
	case body := <-done:
		if !strings.Contains(body, "event: end") {
			t.Errorf("stream closed without an end event:\n%s", body)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("stream did not end with the attempt")
	}
}

func TestDaemon_servesTheConsole(t *testing.T) {
	t.Parallel()

	daemon := start(t, newConfig(t))
	defer daemon.shutdown()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, daemon.baseURL+"/ui/", nil)
	if err != nil {
		t.Fatalf("building console request: %v", err)
	}

	// No Authorization header: the console itself is public, the API it calls
	// is not.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("calling the console: %v", err)
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/ returned %d, want 200", resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the console: %v", err)
	}

	if !strings.Contains(string(raw), "<title>processd</title>") {
		t.Errorf("GET /ui/ did not serve the console page")
	}
}
