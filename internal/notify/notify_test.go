package notify

import (
	"context"
	"encoding/json"
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
)

type fakeSubmitter struct {
	mu        sync.Mutex
	submitted []*core.Process
}

func (f *fakeSubmitter) Submit(_ context.Context, p *core.Process) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.submitted = append(f.submitted, p)

	return nil
}

func (f *fakeSubmitter) all() []*core.Process {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]*core.Process(nil), f.submitted...)
}

type fakeLogs struct {
	lines []string
	err   error
}

func (f fakeLogs) Lines(string, int, logstore.Stream, time.Time, int) ([]string, error) {
	return f.lines, f.err
}

// recorder collects the bodies a webhook endpoint received.
type recorder struct {
	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header
	status  []int
}

func (r *recorder) handler(statuses ...int) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		body := make([]byte, req.ContentLength)
		_, _ = req.Body.Read(body)

		r.mu.Lock()
		call := len(r.bodies)
		r.bodies = append(r.bodies, body)
		r.headers = append(r.headers, req.Header.Clone())
		r.mu.Unlock()

		status := http.StatusOK
		if call < len(statuses) {
			status = statuses[call]
		}

		r.status = append(r.status, status)
		w.WriteHeader(status)
	}
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.bodies)
}

func (r *recorder) decode(t *testing.T, index int) payload {
	t.Helper()

	r.mu.Lock()
	defer r.mu.Unlock()

	var decoded payload
	if err := json.Unmarshal(r.bodies[index], &decoded); err != nil {
		t.Fatalf("decoding notification body %q: %v", r.bodies[index], err)
	}

	return decoded
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func registryFrom(t *testing.T, yaml string) *config.Registry {
	t.Helper()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "workers.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatalf("writing workers file: %v", err)
	}

	registry, err := config.LoadWorkers(dir)
	if err != nil {
		t.Fatalf("loading workers: %v", err)
	}

	return registry
}

// start runs the notifier and returns a function that drains and stops it.
func start(t *testing.T, n *Notifier) func() {
	t.Helper()

	done := make(chan struct{})

	go func() {
		defer close(done)

		_ = n.Run(t.Context())
	}()

	return func() {
		n.Close(5 * time.Second)
		<-done
	}
}

func failedProcess() *core.Process {
	exit := 3
	started := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	finished := started.Add(2 * time.Second)

	return &core.Process{
		ID:         "proc_test",
		Worker:     "invoice",
		Type:       core.TypeTask,
		State:      core.StateFailed,
		Reason:     core.ReasonMaxAttempts,
		Attempt:    3,
		ExitCode:   &exit,
		Command:    "/usr/bin/secret-tool",
		Args:       []string{"--token=hunter2"},
		Env:        map[string]string{"DB_PASSWORD": "hunter2"},
		Metadata:   map[string]string{"invoice": "42"},
		CreatedAt:  started,
		StartedAt:  &started,
		FinishedAt: &finished,
	}
}

func webhookWorker(t *testing.T, url, events string) *config.Worker {
	t.Helper()

	registry := registryFrom(t, `
version: 1
workers:
  - name: invoice
    command: /bin/echo
    notify:
      on: [`+events+`]
      webhook:
        url: `+url+`
        retry: 0
`)

	worker, err := registry.Get("invoice")
	if err != nil {
		t.Fatalf("Get() returned %v", err)
	}

	return worker
}

func TestNotifier_PostsTheOutcome(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	server := httptest.NewServer(rec.handler())

	defer server.Close()

	worker := webhookWorker(t, server.URL, "retries_exhausted")

	notifier := New(Options{Node: "node-01", Logger: quietLogger(), Client: server.Client()})
	stop := start(t, notifier)

	notifier.Notify(config.NotifyOnRetriesExhausted, failedProcess(), worker)
	stop()

	if rec.count() != 1 {
		t.Fatalf("endpoint received %d calls, want 1", rec.count())
	}

	got := rec.decode(t, 0)

	if got.Event != config.NotifyOnRetriesExhausted {
		t.Errorf("event = %q", got.Event)
	}

	if got.Node != "node-01" {
		t.Errorf("node = %q, want node-01", got.Node)
	}

	if got.Process.ID != "proc_test" || got.Process.State != core.StateFailed {
		t.Errorf("process = %+v", got.Process)
	}

	if got.Process.ExitCode == nil || *got.Process.ExitCode != 3 {
		t.Errorf("exit_code = %v, want 3", got.Process.ExitCode)
	}

	if got.Process.Metadata["invoice"] != "42" {
		t.Errorf("metadata = %v, want the client's own keys", got.Process.Metadata)
	}

	if got.Process.DurationMS != 2000 {
		t.Errorf("duration_ms = %d, want 2000", got.Process.DurationMS)
	}
}

// The payload is the one place a secret could leave the node, so the body must
// not carry the environment, the command or its arguments.
func TestNotifier_PayloadCarriesNoSecrets(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	server := httptest.NewServer(rec.handler())

	defer server.Close()

	worker := webhookWorker(t, server.URL, "retries_exhausted")

	notifier := New(Options{Logger: quietLogger(), Client: server.Client()})
	stop := start(t, notifier)

	notifier.Notify(config.NotifyOnRetriesExhausted, failedProcess(), worker)
	stop()

	rec.mu.Lock()
	body := string(rec.bodies[0])
	rec.mu.Unlock()

	for _, forbidden := range []string{"hunter2", "DB_PASSWORD", "secret-tool", "--token"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("notification body leaked %q: %s", forbidden, body)
		}
	}
}

func TestNotifier_IgnoresAnEventTheWorkerDidNotSubscribeTo(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	server := httptest.NewServer(rec.handler())

	defer server.Close()

	worker := webhookWorker(t, server.URL, "crashed")

	notifier := New(Options{Logger: quietLogger(), Client: server.Client()})
	stop := start(t, notifier)

	notifier.Notify(config.NotifyOnRetriesExhausted, failedProcess(), worker)
	stop()

	if rec.count() != 0 {
		t.Fatalf("endpoint received %d calls for an unsubscribed event", rec.count())
	}
}

func TestNotifier_RetriesAFailedDelivery(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	server := httptest.NewServer(rec.handler(http.StatusInternalServerError))

	defer server.Close()

	registry := registryFrom(t, `
version: 1
workers:
  - name: invoice
    command: /bin/echo
    notify:
      on: [failed]
      webhook:
        url: `+server.URL+`
        retry: 1
        timeout: 2s
`)

	worker, _ := registry.Get("invoice")

	notifier := New(Options{Logger: quietLogger(), Client: server.Client()})
	stop := start(t, notifier)

	notifier.Notify(config.NotifyOnFailed, failedProcess(), worker)
	stop()

	// The first call answered 500, the second answered 200.
	if rec.count() != 2 {
		t.Fatalf("endpoint received %d calls, want the failure to be retried once", rec.count())
	}
}

func TestNotifier_LogTailIsOptIn(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	server := httptest.NewServer(rec.handler())

	defer server.Close()

	logs := fakeLogs{lines: []string{"boom", "stack trace"}}

	withoutTail := webhookWorker(t, server.URL, "failed")

	notifier := New(Options{Logger: quietLogger(), Client: server.Client(), Logs: logs})
	stop := start(t, notifier)

	notifier.Notify(config.NotifyOnFailed, failedProcess(), withoutTail)
	stop()

	if tail := rec.decode(t, 0).LogTail; len(tail) != 0 {
		t.Fatalf("log_tail = %v, want nothing without include_log_tail", tail)
	}

	registry := registryFrom(t, `
version: 1
workers:
  - name: invoice
    command: /bin/echo
    notify:
      on: [failed]
      webhook:
        url: `+server.URL+`
        retry: 0
        include_log_tail: 20
`)

	withTail, _ := registry.Get("invoice")

	notifier = New(Options{Logger: quietLogger(), Client: server.Client(), Logs: logs})
	stop = start(t, notifier)

	notifier.Notify(config.NotifyOnFailed, failedProcess(), withTail)
	stop()

	if tail := rec.decode(t, 1).LogTail; len(tail) != 2 || tail[0] != "boom" {
		t.Fatalf("log_tail = %v, want the two captured lines", tail)
	}
}

func TestNotifier_FallsBackToTheDaemonPolicy(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	server := httptest.NewServer(rec.handler())

	defer server.Close()

	registry := registryFrom(t, `
version: 1
workers:
  - name: invoice
    command: /bin/echo
`)

	worker, _ := registry.Get("invoice")

	fallback := config.Notify{
		On: []config.NotifyEvent{config.NotifyOnFailed},
		Webhook: &config.NotifyWebhook{
			URL: server.URL, Method: http.MethodPost, Timeout: config.Duration(2 * time.Second),
		},
	}

	notifier := New(Options{Fallback: fallback, Logger: quietLogger(), Client: server.Client()})
	stop := start(t, notifier)

	notifier.Notify(config.NotifyOnFailed, failedProcess(), worker)
	stop()

	if rec.count() != 1 {
		t.Fatalf("endpoint received %d calls, want the daemon policy to apply", rec.count())
	}
}

// A worker that declares its own policy replaces the daemon one outright.
func TestNotifier_WorkerPolicyReplacesTheFallback(t *testing.T) {
	t.Parallel()

	fallbackRec := &recorder{}
	fallbackServer := httptest.NewServer(fallbackRec.handler())

	defer fallbackServer.Close()

	workerRec := &recorder{}
	workerServer := httptest.NewServer(workerRec.handler())

	defer workerServer.Close()

	worker := webhookWorker(t, workerServer.URL, "failed")

	fallback := config.Notify{
		On: []config.NotifyEvent{config.NotifyOnFailed},
		Webhook: &config.NotifyWebhook{
			URL: fallbackServer.URL, Method: http.MethodPost, Timeout: config.Duration(2 * time.Second),
		},
	}

	notifier := New(Options{Fallback: fallback, Logger: quietLogger(), Client: workerServer.Client()})
	stop := start(t, notifier)

	notifier.Notify(config.NotifyOnFailed, failedProcess(), worker)
	stop()

	if workerRec.count() != 1 {
		t.Errorf("worker endpoint received %d calls, want 1", workerRec.count())
	}

	if fallbackRec.count() != 0 {
		t.Errorf("daemon endpoint received %d calls, want the worker policy to replace it", fallbackRec.count())
	}
}

// The exec target receives the outcome as params, and only the ones it declared.
func TestNotifier_ExecPassesOnlyDeclaredParams(t *testing.T) {
	t.Parallel()

	registry := registryFrom(t, `
version: 1
workers:
  - name: invoice
    command: /bin/echo
    notify:
      on: [failed]
      exec:
        worker: alert
  - name: alert
    command: /usr/local/bin/alert
    args: ["--worker={{worker}}", "--state={{state}}"]
    params:
      worker: {}
      state: {}
`)

	worker, _ := registry.Get("invoice")
	submitter := &fakeSubmitter{}

	notifier := New(Options{
		Workers: func() *config.Registry { return registry },
		Submit:  submitter,
		Node:    "node-01",
		Logger:  quietLogger(),
	})

	stop := start(t, notifier)

	notifier.Notify(config.NotifyOnFailed, failedProcess(), worker)
	stop()

	submitted := submitter.all()
	if len(submitted) != 1 {
		t.Fatalf("submitted %d executions, want 1", len(submitted))
	}

	alert := submitted[0]

	if alert.Worker != "alert" {
		t.Fatalf("worker = %q", alert.Worker)
	}

	want := []string{"--worker=invoice", "--state=FAILED"}
	if len(alert.Args) != len(want) || alert.Args[0] != want[0] || alert.Args[1] != want[1] {
		t.Fatalf("args = %v, want %v", alert.Args, want)
	}

	if alert.Metadata[notifySourceKey] != "proc_test" {
		t.Errorf("metadata = %v, want the source execution recorded", alert.Metadata)
	}

	if alert.Metadata[triggerKey] != triggerNotify {
		t.Errorf("metadata = %v, want the execution marked as a notification", alert.Metadata)
	}
}

// Sending on a closed queue would panic, and the supervisor keeps settling
// executions while the daemon shuts down.
// The daemon-wide fallback applies to every worker that declares no policy of
// its own — including the worker it points at. Without a guard, a notification
// worker that cannot run reports its own failure, which runs it again.
func TestNotifier_DoesNotNotifyAboutANotification(t *testing.T) {
	t.Parallel()

	registry := registryFrom(t, `
version: 1
workers:
  - name: invoice
    command: /bin/echo
  - name: alert
    command: /bin/echo
`)

	submitter := &fakeSubmitter{}

	n := New(Options{
		Fallback: config.Notify{
			On:   []config.NotifyEvent{config.NotifyOnFailed},
			Exec: &config.NotifyExec{Worker: "alert"},
		},
		Workers: func() *config.Registry { return registry },
		Submit:  submitter,
		Node:    "node-1",
		Logger:  quietLogger(),
	})

	stop := start(t, n)

	alert, err := registry.Get("alert")
	if err != nil {
		t.Fatalf("Get() returned %v, want the worker", err)
	}

	// The notification worker itself fails, carrying the marker the notifier
	// puts on everything it creates.
	failed := failedProcess()
	failed.Worker = "alert"
	failed.Metadata = map[string]string{
		triggerKey:      triggerNotify,
		notifyEventKey:  string(config.NotifyOnFailed),
		notifySourceKey: "proc_original",
	}

	n.Notify(config.NotifyOnFailed, failed, alert)

	// An ordinary failure still reports, so the guard is about the marker and
	// not about the worker.
	n.Notify(config.NotifyOnFailed, failedProcess(), nil)

	stop()

	submitted := submitter.all()
	if len(submitted) != 1 {
		t.Fatalf("the notifier ran %d executions, want 1: %v", len(submitted), submitted)
	}

	if source := submitted[0].Metadata[notifySourceKey]; source != "proc_test" {
		t.Errorf("the notification reports %q, want the ordinary failure", source)
	}
}

func TestNotifier_NotifyAfterCloseIsSafe(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	server := httptest.NewServer(rec.handler())

	defer server.Close()

	worker := webhookWorker(t, server.URL, "failed")

	notifier := New(Options{Logger: quietLogger(), Client: server.Client()})
	stop := start(t, notifier)
	stop()

	notifier.Notify(config.NotifyOnFailed, failedProcess(), worker)

	if rec.count() != 0 {
		t.Fatalf("endpoint received %d calls after Close", rec.count())
	}
}

func TestNotifier_HeadersAreSent(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	server := httptest.NewServer(rec.handler())

	defer server.Close()

	registry := registryFrom(t, `
version: 1
workers:
  - name: invoice
    command: /bin/echo
    notify:
      on: [failed]
      webhook:
        url: `+server.URL+`
        retry: 0
        headers:
          X-Processd-Channel: incidents
`)

	worker, _ := registry.Get("invoice")

	notifier := New(Options{Logger: quietLogger(), Client: server.Client()})
	stop := start(t, notifier)

	notifier.Notify(config.NotifyOnFailed, failedProcess(), worker)
	stop()

	rec.mu.Lock()
	header := rec.headers[0]
	rec.mu.Unlock()

	if got := header.Get("X-Processd-Channel"); got != "incidents" {
		t.Errorf("X-Processd-Channel = %q, want incidents", got)
	}

	if got := header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}
