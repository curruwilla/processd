// Package notify tells somebody when an execution ends badly.
//
// A failed task is otherwise silent unless a human is watching the console or a
// Prometheus rule is already written, which is the hole every wrapper script was
// written to fill (docs/SPEC.md §22.2). This is the daemon opening an outbound
// connection of its own for the first time, so every path here is bounded and
// no failure of it can reach the execution that triggered it.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/curruwilla/processd/internal/config"
	"github.com/curruwilla/processd/internal/core"
	"github.com/curruwilla/processd/internal/logstore"
)

const (
	// queueDepth bounds the pending deliveries. A node whose workers are all
	// failing must not grow a notification backlog on top of the incident.
	queueDepth = 256

	// deliveryWorkers is how many notifications are in flight at once. The bound
	// matters more than the number: a slow endpoint must not stall the rest.
	deliveryWorkers = 4

	// retryDelay is the wait between webhook attempts. It is deliberately flat
	// and short: a notification that arrives minutes late has already lost most
	// of its value, and the daemon is not a delivery guarantee.
	retryDelay = 2 * time.Second

	// maxResponseBytes bounds what is read back from a webhook. The body is only
	// ever used to explain a failure in a log line.
	maxResponseBytes = 2 << 10
)

// Submitter admits an execution. It is the scheduler, narrowed to what a
// notification worker needs.
type Submitter interface {
	Submit(ctx context.Context, p *core.Process) error
}

// LogTailer reads back the output of a finished attempt.
type LogTailer interface {
	Lines(processID string, attempt int, stream logstore.Stream, at time.Time, tail int) ([]string, error)
}

// Options groups the notifier dependencies.
type Options struct {
	// Fallback is the daemon-wide policy, used by every worker that declares
	// none of its own.
	Fallback config.Notify
	Workers  func() *config.Registry
	Submit   Submitter
	Logs     LogTailer
	Node     string
	Logger   *slog.Logger

	// Client is the HTTP client for webhooks. A nil client gets a default one
	// with no timeout of its own: the per-webhook timeout owns that.
	Client *http.Client
}

// job is one queued delivery. The execution is captured by value, because the
// supervisor keeps mutating its own copy after handing this over.
type job struct {
	event   config.NotifyEvent
	policy  config.Notify
	process *core.Process
}

// Notifier delivers outcome notifications on its own goroutines.
type Notifier struct {
	fallback config.Notify
	workers  func() *config.Registry
	submit   Submitter
	logs     LogTailer
	client   *http.Client
	node     string
	log      *slog.Logger

	jobs chan job

	// mu guards closed against the send in Notify. Sending on a closed channel
	// panics, and the supervisor keeps settling executions while the daemon
	// shuts down, so the two have to be ordered rather than raced.
	mu     sync.RWMutex
	closed bool

	// drained is closed once every delivery goroutine has exited, and abort is
	// closed to give up on whatever is still in flight.
	drained   chan struct{}
	abort     chan struct{}
	abortOnce sync.Once

	// dropped counts the notifications the queue had no room for. It is reported
	// once at shutdown rather than per drop, so an incident does not also
	// produce a log flood.
	dropped atomic.Int64
}

// New wires a notifier. It does not start delivering until Run is called.
func New(opts Options) *Notifier {
	client := opts.Client
	if client == nil {
		client = &http.Client{}
	}

	return &Notifier{
		fallback: opts.Fallback,
		workers:  opts.Workers,
		submit:   opts.Submit,
		logs:     opts.Logs,
		client:   client,
		node:     opts.Node,
		log:      opts.Logger,
		jobs:     make(chan job, queueDepth),
		drained:  make(chan struct{}),
		abort:    make(chan struct{}),
	}
}

// Notify queues a notification for the outcome, if the worker asked for one.
//
// It never blocks and never fails: it is called from the supervisor, on the
// path that records what an execution did, and nothing about telling somebody
// may delay or change that.
func (n *Notifier) Notify(event config.NotifyEvent, p *core.Process, worker *config.Worker) {
	policy := n.policyFor(worker)
	if !policy.Notifies(event) {
		return
	}

	n.mu.RLock()
	defer n.mu.RUnlock()

	if n.closed {
		return
	}

	select {
	case n.jobs <- job{event: event, policy: policy, process: p.Clone()}:
	default:
		n.dropped.Add(1)
	}
}

// policyFor returns the worker's own policy, or the daemon-wide fallback.
func (n *Notifier) policyFor(worker *config.Worker) config.Notify {
	if worker != nil && worker.Notify.IsSet() {
		return worker.Notify
	}

	return n.fallback
}

// Run delivers queued notifications until Close drains the queue.
//
// It deliberately does not stop on ctx: the daemon shutting down is exactly
// when the last few outcomes are worth reporting, so the queue is closed by
// Close, after the supervisor has settled everything it was running.
func (n *Notifier) Run(ctx context.Context) error {
	deliveries, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()

	// A delivery is bounded by its own timeout, and Close cuts the rest short
	// when a shutdown cannot wait for it.
	go func() {
		select {
		case <-n.abort:
		case <-n.drained:
		}

		cancel()
	}()

	var wg sync.WaitGroup

	for range deliveryWorkers {
		wg.Go(func() {
			for pending := range n.jobs {
				n.deliver(deliveries, pending)
			}
		})
	}

	wg.Wait()
	close(n.drained)

	if dropped := n.dropped.Load(); dropped > 0 {
		n.log.Warn("notifications dropped because the queue was full", slog.Int64("count", dropped))
	}

	return nil
}

// Close stops accepting notifications and gives the queued ones budget to go
// out, then abandons whatever is still in flight.
//
// Delivery is best effort by design: a node must not stay up waiting on an
// endpoint to acknowledge that something else went wrong.
func (n *Notifier) Close(budget time.Duration) {
	n.mu.Lock()

	if !n.closed {
		n.closed = true
		close(n.jobs)
	}

	n.mu.Unlock()

	select {
	case <-n.drained:
	case <-time.After(budget):
		n.log.Warn("abandoning pending notifications", slog.Duration("budget", budget))
		n.abortOnce.Do(func() { close(n.abort) })
		<-n.drained
	}
}

// deliver runs every configured delivery for one outcome. One failing does not
// skip the other.
func (n *Notifier) deliver(ctx context.Context, pending job) {
	if pending.policy.Webhook != nil {
		n.post(ctx, pending)
	}

	if pending.policy.Exec != nil {
		n.run(ctx, pending)
	}
}

// payload is what a webhook receives.
//
// It carries identity, outcome and timing, and nothing else. There is no
// environment, no command line and no argument list: the daemon environment
// holds secrets by design, and a notification is the one place they would leave
// the node. Metadata is included because a client put it there deliberately.
type payload struct {
	Event   config.NotifyEvent `json:"event"`
	Node    string             `json:"node"`
	SentAt  time.Time          `json:"sent_at"`
	Process payloadProcess     `json:"process"`

	// LogTail is present only when the webhook opted in.
	LogTail []string `json:"log_tail,omitempty"`
}

type payloadProcess struct {
	ID         string            `json:"id"`
	Worker     string            `json:"worker"`
	Type       core.Type         `json:"type"`
	State      core.State        `json:"state"`
	Reason     core.Reason       `json:"reason,omitempty"`
	Attempt    int               `json:"attempt"`
	Restarts   int               `json:"restarts"`
	ExitCode   *int              `json:"exit_code"`
	Signal     string            `json:"signal,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
	StartedAt  *time.Time        `json:"started_at,omitempty"`
	FinishedAt *time.Time        `json:"finished_at,omitempty"`
	DurationMS int64             `json:"duration_ms"`
}

// post delivers one webhook, retrying a failure up to the configured bound.
func (n *Notifier) post(ctx context.Context, pending job) {
	hook := pending.policy.Webhook

	body, err := json.Marshal(n.payloadOf(pending))
	if err != nil {
		n.log.Error("encoding notification", slog.String("process", pending.process.ID), slog.Any("error", err))
		return
	}

	attempts := hook.Retry + 1

	for attempt := 1; attempt <= attempts; attempt++ {
		err = n.postOnce(ctx, hook, body)
		if err == nil {
			return
		}

		if attempt < attempts {
			select {
			case <-ctx.Done():
			case <-time.After(retryDelay):
			}
		}
	}

	// Failing to notify is logged, never fatal: the execution it describes has
	// already happened and is already recorded.
	n.log.Error("delivering notification webhook",
		slog.String("process", pending.process.ID),
		slog.String("event", string(pending.event)),
		slog.String("url", hook.URL),
		slog.Int("attempts", attempts),
		slog.Any("error", err),
	)
}

func (n *Notifier) postOnce(ctx context.Context, hook *config.NotifyWebhook, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, hook.Timeout.Duration())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, hook.Method, hook.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building notification request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	for name, value := range hook.Headers {
		req.Header.Set(name, value)
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("posting notification: %w", err)
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= http.StatusBadRequest {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))

		return fmt.Errorf("notification endpoint answered %s: %s", resp.Status, bytes.TrimSpace(detail))
	}

	// The body is drained so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))

	return nil
}

// payloadOf builds the body for one outcome.
func (n *Notifier) payloadOf(pending job) payload {
	p := pending.process

	built := payload{
		Event:  pending.event,
		Node:   n.node,
		SentAt: time.Now().UTC(),
		Process: payloadProcess{
			ID:         p.ID,
			Worker:     p.Worker,
			Type:       p.Type,
			State:      p.State,
			Reason:     p.Reason,
			Attempt:    p.Attempt,
			Restarts:   p.Restarts,
			ExitCode:   p.ExitCode,
			Signal:     p.Signal,
			Metadata:   p.Metadata,
			CreatedAt:  p.CreatedAt,
			StartedAt:  p.StartedAt,
			FinishedAt: p.FinishedAt,
			DurationMS: p.Duration().Milliseconds(),
		},
	}

	if tail := pending.policy.Webhook.IncludeLogTail; tail > 0 && n.logs != nil {
		lines, err := n.logs.Lines(p.ID, p.Attempt, logstore.StreamBoth, p.CreatedAt, tail)
		if err != nil {
			n.log.Warn("reading the log tail for a notification",
				slog.String("process", p.ID), slog.Any("error", err))
		} else {
			built.LogTail = lines
		}
	}

	return built
}

// run fires the worker configured as a notification target.
//
// The outcome reaches it as params, so a notification obeys the same rule as
// every other execution: nothing reaches a process that the worker did not
// declare.
func (n *Notifier) run(ctx context.Context, pending job) {
	name := pending.policy.Exec.Worker

	worker, err := n.workers().Get(name)
	if err != nil {
		n.log.Error("resolving the notification worker", slog.String("worker", name), slog.Any("error", err))
		return
	}

	if !worker.IsEnabled() {
		n.log.Warn("notification worker is disabled", slog.String("worker", name))
		return
	}

	process, err := worker.Instantiate(n.paramsFor(worker, pending))
	if err != nil {
		n.log.Error("building the notification execution",
			slog.String("worker", name), slog.Any("error", err))

		return
	}

	process.Metadata = map[string]string{
		triggerKey:      triggerNotify,
		notifyEventKey:  string(pending.event),
		notifySourceKey: pending.process.ID,
	}

	if err := n.submit.Submit(ctx, process); err != nil {
		n.log.Error("submitting the notification execution",
			slog.String("worker", name),
			slog.String("source", pending.process.ID),
			slog.Any("error", err),
		)
	}
}

// Metadata keys that mark an execution the notifier created.
const (
	triggerKey      = "processd.trigger"
	triggerNotify   = "notify"
	notifyEventKey  = "processd.notify_event"
	notifySourceKey = "processd.notify_source"
)

// paramsFor fills in the values the notification worker declared, and only
// those: passing a param a worker did not declare is refused by Resolve, which
// is the behaviour that makes this safe rather than a special case.
func (n *Notifier) paramsFor(worker *config.Worker, pending job) map[string]string {
	p := pending.process

	available := map[string]string{
		"event":      string(pending.event),
		"process_id": p.ID,
		"worker":     p.Worker,
		"state":      string(p.State),
		"reason":     string(p.Reason),
		"attempt":    strconv.Itoa(p.Attempt),
		"signal":     p.Signal,
		"node":       n.node,
	}

	if p.ExitCode != nil {
		available["exit_code"] = strconv.Itoa(*p.ExitCode)
	}

	params := make(map[string]string, len(available))

	for name, value := range available {
		if _, declared := worker.Params[name]; declared {
			params[name] = value
		}
	}

	return params
}
