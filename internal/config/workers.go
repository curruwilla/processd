package config

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"time"

	"github.com/curruwilla/processd/internal/core"
	"github.com/curruwilla/processd/internal/cron"
)

// LockConflict decides what happens when the lock a worker needs is taken.
type LockConflict string

const (
	// LockConflictQueue keeps the execution queued until the lock frees up.
	LockConflictQueue LockConflict = "queue"
	// LockConflictReject answers 409 instead of queueing.
	LockConflictReject LockConflict = "reject"
)

// BackoffType is the growth curve of the retry delay.
type BackoffType string

// Supported backoff curves.
const (
	BackoffExponential BackoffType = "exponential"
	BackoffLinear      BackoffType = "linear"
	BackoffFixed       BackoffType = "fixed"
)

// RetryTrigger is a class of failure that may be retried.
type RetryTrigger string

// Retry triggers, matching the failure classes the supervisor reports.
const (
	RetryOnNonZeroExit RetryTrigger = "nonzero_exit"
	RetryOnSignal      RetryTrigger = "signal"
	RetryOnStartError  RetryTrigger = "start_error"
	RetryOnTimeout     RetryTrigger = "timeout"
	// RetryOnExit is any exit at all, the successful ones included. Only a
	// service may declare it: for a task, exiting is the point.
	RetryOnExit RetryTrigger = "exit"
)

// retryTriggers is every trigger the supervisor knows how to report.
var retryTriggers = []RetryTrigger{
	RetryOnNonZeroExit,
	RetryOnSignal,
	RetryOnStartError,
	RetryOnTimeout,
	RetryOnExit,
}

// Overridable names a field a request is allowed to override.
type Overridable string

// Fields a worker may expose for per-request overriding.
const (
	OverridableEnv     Overridable = "env"
	OverridableTimeout Overridable = "timeout"
	OverridableLock    Overridable = "lock"
)

// Worker is a named execution template (docs/SPEC.md §5).
type Worker struct {
	Name    string    `yaml:"name"`
	Enabled *bool     `yaml:"enabled"`
	Type    core.Type `yaml:"type"`

	Command string           `yaml:"command"`
	Args    []string         `yaml:"args"`
	Params  map[string]Param `yaml:"params"`

	Cwd            string            `yaml:"cwd"`
	User           string            `yaml:"user"`
	Group          string            `yaml:"group"`
	Env            map[string]string `yaml:"env"`
	EnvPassthrough []string          `yaml:"env_passthrough"`

	Timeout   Duration `yaml:"timeout"`
	KillGrace Duration `yaml:"kill_grace"`

	MaxProcesses int           `yaml:"max_processes"`
	Lock         string        `yaml:"lock"`
	LockConflict LockConflict  `yaml:"lock_conflict"`
	Overridable  []Overridable `yaml:"overridable"`

	Retry    Retry      `yaml:"retry"`
	Logs     WorkerLogs `yaml:"logs"`
	Schedule Schedule   `yaml:"schedule"`
	Notify   Notify     `yaml:"notify"`
}

// NotifyEvent is an outcome worth telling somebody about (docs/SPEC.md §22.2).
type NotifyEvent string

// The outcomes a notification can be attached to. They are chosen from the most
// specific match, so one outcome produces at most one notification even when
// the worker lists several events that would all apply.
const (
	// NotifyOnFailed is a definitive failure that is not one of the two below.
	NotifyOnFailed NotifyEvent = "failed"
	// NotifyOnCrashed is an attempt that ended badly and will be retried.
	NotifyOnCrashed NotifyEvent = "crashed"
	// NotifyOnRetriesExhausted is a failure that spent the attempt budget.
	NotifyOnRetriesExhausted NotifyEvent = "retries_exhausted"
	// NotifyOnTimeout is an attempt killed by its own deadline, whether or not
	// another attempt follows.
	NotifyOnTimeout NotifyEvent = "timeout"
)

// notifyEvents is every event a worker may subscribe to.
var notifyEvents = []NotifyEvent{
	NotifyOnFailed,
	NotifyOnCrashed,
	NotifyOnRetriesExhausted,
	NotifyOnTimeout,
}

// Notify says who to tell when an execution ends badly.
//
// Without it a failure is silent unless somebody is watching the console or
// scraping the metrics, which is the hole every wrapper script was written to
// fill (docs/SPEC.md §22.2).
type Notify struct {
	// On lists the outcomes worth reporting. It is required whenever any
	// delivery is configured: a notification with no trigger is a typo.
	On []NotifyEvent `yaml:"on"`

	// Webhook posts the outcome to an HTTP endpoint.
	Webhook *NotifyWebhook `yaml:"webhook"`

	// Exec runs another worker instead of, or as well as, the webhook.
	Exec *NotifyExec `yaml:"exec"`
}

// NotifyWebhook is an outbound HTTP call. It is the only place the daemon opens
// a connection of its own, so every bound on it is mandatory.
type NotifyWebhook struct {
	URL     string            `yaml:"url"`
	Method  string            `yaml:"method"`
	Headers map[string]string `yaml:"headers"`

	// Timeout bounds one attempt, and Retry bounds how many follow it. An
	// unbounded notifier is a way to turn one failing worker into an outage.
	Timeout Duration `yaml:"timeout"`
	Retry   int      `yaml:"retry"`

	// IncludeLogTail attaches the last N lines of the attempt's output. It is
	// off by default and opt-in on purpose: logs carry secrets far more often
	// than anyone intends.
	IncludeLogTail int `yaml:"include_log_tail"`
}

// NotifyParams are the values a notification worker may declare as params. Any
// it declares is filled in; any it does not is never passed, because arguments
// reach a process only through declared params and a notification is not an
// exception to that.
var NotifyParams = []string{
	"event",
	"process_id",
	"worker",
	"state",
	"reason",
	"attempt",
	"exit_code",
	"signal",
	"node",
}

// NotifyExec runs a worker when the outcome matches. The target receives the
// outcome through declared params like any other execution — nothing reaches a
// process undeclared, notifications included.
type NotifyExec struct {
	Worker string `yaml:"worker"`
}

// IsSet reports whether anything is configured.
func (n Notify) IsSet() bool { return len(n.On) > 0 || n.Webhook != nil || n.Exec != nil }

// Notifies reports whether the policy subscribes to the event.
func (n Notify) Notifies(event NotifyEvent) bool { return slices.Contains(n.On, event) }

// Schedule makes a worker fire on its own, without an external cron calling the
// API (docs/SPEC.md §22.1).
//
// A schedule is a property of the worker, not of a request: the daemon that
// owns the process is the one that knows whether it ran, so it is also the one
// that can report that it did not.
type Schedule struct {
	// Cron is a five-field expression, or one of the @-descriptors. Empty means
	// the worker is only ever triggered through the API.
	Cron string `yaml:"cron"`

	// Timezone names the zone the expression is read in. Empty is UTC, never
	// the host zone: the same file must describe the same instants on every
	// node.
	Timezone string `yaml:"timezone"`

	// CatchUp decides what happens to occurrences that passed while the daemon
	// was down. False, the default, records them and moves on: surprise work
	// after a restart is worse than work that did not happen.
	CatchUp bool `yaml:"catch_up"`

	// Jitter spreads firings randomly across a window, so that a fleet of nodes
	// sharing a schedule does not hit the same dependency at the same instant.
	Jitter Duration `yaml:"jitter"`

	compiled *cron.Schedule
}

// IsSet reports whether the worker has a schedule at all.
func (s Schedule) IsSet() bool { return s.Cron != "" }

// Compiled returns the parsed expression, or nil when the worker has no
// schedule. Validate compiles it once at load time, so neither the dispatch
// loop nor a request ever pays for parsing.
func (s Schedule) Compiled() *cron.Schedule { return s.compiled }

// Param declares a substitutable value, and the only shape it may take.
// Arguments reach the process through declared params and nowhere else.
type Param struct {
	Required bool     `yaml:"required"`
	Pattern  string   `yaml:"pattern"`
	Enum     []string `yaml:"enum"`
	Default  string   `yaml:"default"`

	compiled *regexp.Regexp
}

// Retry is the restart policy of a worker (docs/SPEC.md §12).
//
// Enabled is a pointer so that "the operator said nothing" stays distinct from
// "the operator said no": a task defaults to no retries and a service to
// restarting forever, and an explicit `enabled: false` on a service is a
// contradiction the loader refuses rather than resolves.
type Retry struct {
	Enabled          *bool          `yaml:"enabled"`
	MaxAttempts      Attempts       `yaml:"max_attempts"`
	RetryOn          []RetryTrigger `yaml:"retry_on"`
	SuccessExitCodes []int          `yaml:"success_exit_codes"`
	NoRetryExitCodes []int          `yaml:"no_retry_exit_codes"`
	ResetAfter       Duration       `yaml:"reset_after"`
	OnShutdown       bool           `yaml:"on_shutdown"`
	Backoff          Backoff        `yaml:"backoff"`
}

// Backoff describes how the delay between attempts grows.
type Backoff struct {
	Type       BackoffType `yaml:"type"`
	Initial    Duration    `yaml:"initial"`
	Max        Duration    `yaml:"max"`
	Multiplier float64     `yaml:"multiplier"`
	Jitter     float64     `yaml:"jitter"`
}

// WorkerLogs overrides the daemon-wide log limits for one worker.
//
// Only the size cap is per worker. Retention is not: the log GC walks files by
// age, and asking it which worker wrote each one would turn a directory walk
// into one store lookup per file (docs/SPEC.md §10).
type WorkerLogs struct {
	MaxBytesPerStream ByteSize  `yaml:"max_bytes_per_stream"`
	Rotate            LogRotate `yaml:"rotate"`
}

// LogRotate keeps a long-lived stream readable past its size cap by moving the
// full file aside instead of dropping what follows it.
//
// Without rotation a stream goes silent the first time it fills its cap. That
// is the right trade for a task, whose attempt is short by definition, and
// unusable for a service, whose single attempt may run for months — which is
// why a service must configure it (docs/SPEC.md §10).
type LogRotate struct {
	// MaxFiles is how many rotated files are kept behind the live one. Zero
	// disables rotation.
	MaxFiles int `yaml:"max_files"`
}

// IsEnabled reports whether the worker may be executed. Workers are enabled
// unless the file says otherwise.
func (w *Worker) IsEnabled() bool { return w.Enabled == nil || *w.Enabled }

// IsEnabled reports whether the policy retries at all. The loader resolves the
// default for the worker type, so a policy that reached this point has an
// answer.
func (r Retry) IsEnabled() bool { return r.Enabled != nil && *r.Enabled }

// Allows reports whether a request may override the given field.
func (w *Worker) Allows(field Overridable) bool { return slices.Contains(w.Overridable, field) }

// EffectiveAttempts returns how many attempts the policy allows, resolving a
// disabled policy to the single attempt every execution gets.
func (r Retry) EffectiveAttempts() int {
	if !r.IsEnabled() {
		return 1
	}

	if r.MaxAttempts.Unlimited() {
		return AttemptsUnlimited
	}

	return max(r.MaxAttempts.Int(), 1)
}

// declaresPolicy reports whether the file set a retry key that means nothing
// unless retries are on.
//
// success_exit_codes and no_retry_exit_codes are deliberately not on the list:
// they decide what an exit means, which is a question a task answers whether or
// not it ever tries again.
func (r Retry) declaresPolicy() bool {
	return r.MaxAttempts != 0 ||
		len(r.RetryOn) > 0 ||
		r.ResetAfter != 0 ||
		r.OnShutdown ||
		r.Backoff != (Backoff{})
}

// RetriesOn reports whether the trigger is in the worker's retry policy.
func (w *Worker) RetriesOn(trigger RetryTrigger) bool {
	return w.Retry.IsEnabled() && slices.Contains(w.Retry.RetryOn, trigger)
}

// Instantiate turns the template into a concrete execution with params applied.
//
// The definition it produces is frozen at this point: reloading workers.d later
// never mutates an execution that already exists (docs/SPEC.md §5). Every
// trigger goes through here — an API request, a schedule — so that a field
// added to a worker cannot reach one path and be forgotten on the other.
func (w *Worker) Instantiate(params map[string]string) (*core.Process, error) {
	resolved, err := w.Resolve(params)
	if err != nil {
		return nil, err
	}

	return &core.Process{
		ID:          core.NewProcessID(),
		Worker:      w.Name,
		Type:        w.Type,
		State:       core.StateCreated,
		Command:     w.Command,
		Args:        resolved.Args,
		Env:         maps.Clone(w.Env),
		Cwd:         w.Cwd,
		User:        w.User,
		Group:       w.Group,
		Lock:        resolved.Lock,
		Timeout:     w.Timeout.Duration(),
		MaxAttempts: w.Retry.EffectiveAttempts(),
		CreatedAt:   time.Now().UTC(),
	}, nil
}

// workersFile is the on-disk shape of a file in workers.d.
type workersFile struct {
	Version int       `yaml:"version"`
	Workers []*Worker `yaml:"workers"`
}

// Registry holds every worker currently loaded, indexed by name.
type Registry struct {
	workers map[string]*Worker
}

// Get returns the worker by name, or core.ErrWorkerNotFound.
func (r *Registry) Get(name string) (*Worker, error) {
	worker, ok := r.workers[name]
	if !ok {
		return nil, fmt.Errorf("%q: %w", name, core.ErrWorkerNotFound)
	}

	return worker, nil
}

// All returns every worker, sorted by name.
func (r *Registry) All() []*Worker {
	all := make([]*Worker, 0, len(r.workers))
	for _, worker := range r.workers {
		all = append(all, worker)
	}

	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })

	return all
}

// Len returns how many workers are loaded.
func (r *Registry) Len() int { return len(r.workers) }

// LoadWorkers reads every *.yaml file in dir and returns the resulting
// registry. Loading is all-or-nothing: one invalid worker fails the whole
// reload, so a bad edit never silently drops a worker from a live daemon.
func LoadWorkers(dir string) (*Registry, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("scanning workers dir %q: %w", dir, err)
	}

	registry := &Registry{workers: map[string]*Worker{}}
	errs := []error{}

	for _, path := range paths {
		file, err := os.Open(path) //nolint:gosec // paths come from the operator's workers dir
		if err != nil {
			errs = append(errs, fmt.Errorf("opening %q: %w", path, err))
			continue
		}

		var parsed workersFile

		err = decodeStrict(file, &parsed)
		_ = file.Close() // read-only handle

		if err != nil {
			errs = append(errs, fmt.Errorf("parsing %q: %w", path, err))
			continue
		}

		for _, worker := range parsed.Workers {
			if _, exists := registry.workers[worker.Name]; exists {
				errs = append(errs, fmt.Errorf("worker %q declared twice", worker.Name))
				continue
			}

			applyWorkerDefaults(worker)

			if err := worker.Validate(); err != nil {
				errs = append(errs, fmt.Errorf("worker %q: %w", worker.Name, err))
				continue
			}

			registry.workers[worker.Name] = worker
		}
	}

	// A notification target can only be checked once every file is in: it may be
	// declared after the worker that points at it, or in another file entirely.
	errs = append(errs, validateNotifyTargets(registry)...)

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	return registry, nil
}

// validateNotifyTargets checks every notify.exec against the loaded set.
func validateNotifyTargets(registry *Registry) []error {
	errs := []error{}

	for _, worker := range registry.All() {
		if worker.Notify.Exec == nil {
			continue
		}

		name := worker.Notify.Exec.Worker

		target, ok := registry.workers[name]
		if !ok {
			errs = append(errs, fmt.Errorf("worker %q: notify.exec.worker %q is not declared", worker.Name, name))
			continue
		}

		// A notification that can itself notify is a loop with no bound: the
		// worker that reports a failure fails, and reports that.
		if target.Notify.IsSet() {
			errs = append(errs, fmt.Errorf(
				"worker %q: notify.exec.worker %q declares notify of its own, which would notify about notifications",
				worker.Name, name,
			))
		}

		if target.Type == core.TypeService {
			errs = append(errs, fmt.Errorf(
				"worker %q: notify.exec.worker %q is a service, and a notification is a task that ends",
				worker.Name, name,
			))
		}

		// A notification carries exactly the values in NotifyParams, so anything
		// else the target insists on has nobody to answer for it.
		for param, declared := range target.Params {
			if declared.Required && !slices.Contains(NotifyParams, param) {
				errs = append(errs, fmt.Errorf(
					"worker %q: notify.exec.worker %q requires param %q, which a notification does not carry",
					worker.Name, name, param,
				))
			}
		}
	}

	return errs
}

func applyWorkerDefaults(w *Worker) {
	if w.Type == "" {
		w.Type = core.TypeTask
	}

	if w.Cwd == "" {
		w.Cwd = "/"
	}

	if w.LockConflict == "" {
		w.LockConflict = LockConflictQueue
	}

	if w.KillGrace == 0 {
		w.KillGrace = Duration(15 * time.Second)
	}

	// A service restarts by definition, so its policy is filled in whether or
	// not the file mentions retries; a task only gets one when it asks.
	if w.Type == core.TypeService && w.Retry.Enabled == nil {
		enabled := true
		w.Retry.Enabled = &enabled
	}

	if w.Retry.IsEnabled() {
		applyRetryDefaults(&w.Retry, w.Type)
	}

	applyNotifyDefaults(&w.Notify)
}

// applyNotifyDefaults fills in the bounds an outbound call must have. They are
// defaults rather than requirements because the operator should not have to
// restate "do not hang" on every worker.
func applyNotifyDefaults(n *Notify) {
	if n.Webhook == nil {
		return
	}

	if n.Webhook.Method == "" {
		n.Webhook.Method = http.MethodPost
	}

	if n.Webhook.Timeout == 0 {
		n.Webhook.Timeout = Duration(5 * time.Second)
	}
}

func applyRetryDefaults(r *Retry, kind core.Type) {
	service := kind == core.TypeService

	if r.MaxAttempts == 0 {
		// Something meant to run forever has no attempt budget to spend; a task
		// that keeps failing has to stop somewhere.
		r.MaxAttempts = 1
		if service {
			r.MaxAttempts = AttemptsUnlimited
		}
	}

	if len(r.RetryOn) == 0 {
		r.RetryOn = []RetryTrigger{RetryOnNonZeroExit, RetryOnSignal, RetryOnStartError}
		if service {
			// Every way a service can stop running is a reason to start it again,
			// a clean exit included.
			r.RetryOn = append(r.RetryOn, RetryOnExit)
		}
	}

	// No exit code means success for a service, so it never gets the default a
	// task gets, and declaring one is refused outright.
	if len(r.SuccessExitCodes) == 0 && !service {
		r.SuccessExitCodes = []int{0}
	}

	if r.Backoff.Type == "" {
		r.Backoff.Type = BackoffExponential
	}

	if r.Backoff.Initial == 0 {
		r.Backoff.Initial = Duration(5 * time.Second)
	}

	if r.Backoff.Max == 0 {
		r.Backoff.Max = Duration(5 * time.Minute)
	}

	if r.Backoff.Multiplier == 0 {
		r.Backoff.Multiplier = 2
	}
}

// Validate rejects worker definitions that are unsafe or unusable. It also
// compiles every param pattern once, so that request handling never pays for
// regexp compilation and a broken pattern fails at load time.
func (w *Worker) Validate() error {
	errs := []error{}

	if w.Name == "" {
		errs = append(errs, errors.New("name must not be empty"))
	}

	switch w.Type {
	case core.TypeTask, core.TypeService:
	default:
		errs = append(errs, fmt.Errorf("type %q: %w", w.Type, core.ErrUnsupportedType))
	}

	if !filepath.IsAbs(w.Command) {
		errs = append(errs, fmt.Errorf("command %q must be an absolute path", w.Command))
	}

	if !filepath.IsAbs(w.Cwd) {
		errs = append(errs, fmt.Errorf("cwd %q must be an absolute path", w.Cwd))
	}

	if w.MaxProcesses < 0 {
		errs = append(errs, errors.New("max_processes must not be negative"))
	}

	switch w.LockConflict {
	case LockConflictQueue, LockConflictReject:
	default:
		errs = append(errs, fmt.Errorf("lock_conflict %q is unknown", w.LockConflict))
	}

	errs = append(errs, w.validateParams()...)
	errs = append(errs, w.validateRetry()...)
	errs = append(errs, w.validateLogs()...)
	errs = append(errs, w.validateSchedule()...)
	errs = append(errs, w.Notify.validate()...)
	errs = append(errs, w.validateType()...)

	return errors.Join(errs...)
}

func (w *Worker) validateParams() []error {
	errs := []error{}

	for name, param := range w.Params {
		if param.Pattern == "" {
			continue
		}

		compiled, err := regexp.Compile(param.Pattern)
		if err != nil {
			errs = append(errs, fmt.Errorf("param %q pattern: %w", name, err))
			continue
		}

		param.compiled = compiled
		w.Params[name] = param
	}

	// Every placeholder must be declared, otherwise a typo would silently ship
	// a literal "{{id}}" to the process.
	for _, arg := range w.Args {
		for _, name := range placeholderNames(arg) {
			if _, ok := w.Params[name]; !ok {
				errs = append(errs, fmt.Errorf("arg placeholder %q is not declared in params", name))
			}
		}
	}

	for _, name := range placeholderNames(w.Lock) {
		if _, ok := w.Params[name]; !ok {
			errs = append(errs, fmt.Errorf("lock placeholder %q is not declared in params", name))
		}
	}

	return errs
}

func (w *Worker) validateRetry() []error {
	if !w.Retry.IsEnabled() {
		// Fail closed. A policy written next to a switch that is off never runs,
		// and a file that reads "max_attempts: 5" on a worker that makes one
		// attempt is a worse answer than a reload that does not apply.
		if w.Retry.declaresPolicy() {
			return []error{errors.New(
				"retry declares a policy but retry.enabled is not true, so none of it would apply: " +
					"set retry.enabled: true, or remove the policy",
			)}
		}

		return nil
	}

	errs := []error{}

	if w.Retry.MaxAttempts < 1 && !w.Retry.MaxAttempts.Unlimited() {
		errs = append(errs, errors.New(`retry.max_attempts must be at least 1, or "unlimited"`))
	}

	if w.Retry.Backoff.Jitter < 0 || w.Retry.Backoff.Jitter > 1 {
		errs = append(errs, errors.New("retry.backoff.jitter must be between 0 and 1"))
	}

	// A multiplier of zero is the absent key, and applyRetryDefaults fills it in.
	// Anything below that shrinks the curve towards zero, which is a backoff that
	// does not back off.
	if w.Retry.Backoff.Multiplier < 0 {
		errs = append(errs, errors.New("retry.backoff.multiplier must be greater than zero"))
	}

	if w.Retry.Backoff.Initial < 0 {
		errs = append(errs, errors.New("retry.backoff.initial must not be negative"))
	}

	if w.Retry.Backoff.Max < 0 {
		errs = append(errs, errors.New("retry.backoff.max must not be negative"))
	}

	switch w.Retry.Backoff.Type {
	case BackoffExponential, BackoffLinear, BackoffFixed:
	default:
		errs = append(errs, fmt.Errorf("retry.backoff.type %q is unknown", w.Retry.Backoff.Type))
	}

	for _, trigger := range w.Retry.RetryOn {
		if !slices.Contains(retryTriggers, trigger) {
			errs = append(errs, fmt.Errorf("retry.retry_on %q is unknown", trigger))
		}
	}

	return errs
}

// validateLogs rejects a log policy that cannot bound what it stores.
func (w *Worker) validateLogs() []error {
	if w.Logs.Rotate.MaxFiles < 0 {
		return []error{errors.New("logs.rotate.max_files must not be negative")}
	}

	return nil
}

// validateSchedule compiles the cron expression and refuses a schedule the
// daemon could not fire unattended.
//
// It also compiles once, at load time: a broken expression must fail the reload
// that introduced it, not the tick an hour later that nobody is watching.
func (w *Worker) validateSchedule() []error {
	if !w.Schedule.IsSet() {
		// Fail closed: the sibling keys only mean something next to an
		// expression, and silently ignoring them would hide a typo in `cron`.
		if w.Schedule.Timezone != "" || w.Schedule.CatchUp || w.Schedule.Jitter != 0 {
			return []error{errors.New("schedule.cron must be set for the other schedule keys to mean anything")}
		}

		return nil
	}

	errs := []error{}

	if w.Type == core.TypeService {
		errs = append(errs, errors.New(
			"schedule must not be set for a service: a service is already meant to be running at all times",
		))
	}

	if w.Schedule.Jitter < 0 {
		errs = append(errs, errors.New("schedule.jitter must not be negative"))
	}

	location := time.UTC

	if w.Schedule.Timezone != "" {
		loaded, err := time.LoadLocation(w.Schedule.Timezone)
		if err != nil {
			errs = append(errs, fmt.Errorf("schedule.timezone %q: %w", w.Schedule.Timezone, err))
		} else {
			location = loaded
		}
	}

	compiled, err := cron.Parse(w.Schedule.Cron, location)
	if err != nil {
		errs = append(errs, fmt.Errorf("schedule.cron: %w", err))
	} else {
		w.Schedule.compiled = compiled
	}

	// A scheduled firing carries no request, so there is nobody to answer for a
	// required param. Refusing at load time turns a silent daily failure into a
	// reload that does not apply.
	for name, param := range w.Params {
		if param.Required {
			errs = append(errs, fmt.Errorf(
				"param %q is required, so worker %q cannot be scheduled: a firing sends no params",
				name, w.Name,
			))
		}
	}

	return errs
}

// validate rejects a notification policy that could not be delivered, or that
// has no trigger to deliver on.
func (n Notify) validate() []error {
	if !n.IsSet() {
		return nil
	}

	errs := []error{}

	if len(n.On) == 0 {
		errs = append(errs, errors.New("notify.on must list at least one event for a notification to be delivered"))
	}

	for _, event := range n.On {
		if !slices.Contains(notifyEvents, event) {
			errs = append(errs, fmt.Errorf("notify.on %q is unknown", event))
		}
	}

	if n.Webhook == nil && n.Exec == nil {
		errs = append(errs, errors.New("notify needs a webhook or an exec target"))
	}

	if n.Webhook != nil {
		errs = append(errs, n.Webhook.validate()...)
	}

	if n.Exec != nil && n.Exec.Worker == "" {
		errs = append(errs, errors.New("notify.exec.worker must name a worker"))
	}

	return errs
}

func (h NotifyWebhook) validate() []error {
	errs := []error{}

	parsed, err := url.Parse(h.URL)

	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("notify.webhook.url %q: %w", h.URL, err))
	case parsed.Scheme != "http" && parsed.Scheme != "https":
		errs = append(errs, fmt.Errorf("notify.webhook.url %q must be http or https", h.URL))
	case parsed.Host == "":
		errs = append(errs, fmt.Errorf("notify.webhook.url %q has no host", h.URL))
	}

	if h.Timeout <= 0 {
		errs = append(errs, errors.New("notify.webhook.timeout must be greater than zero"))
	}

	if h.Retry < 0 {
		errs = append(errs, errors.New("notify.webhook.retry must not be negative"))
	}

	if h.IncludeLogTail < 0 {
		errs = append(errs, errors.New("notify.webhook.include_log_tail must not be negative"))
	}

	return errs
}

// validateType enforces the rules that only make sense for one execution type.
// The two have opposite semantics (docs/SPEC.md §4), so a key that is ordinary
// on one side is a contradiction on the other and is refused rather than
// ignored.
func (w *Worker) validateType() []error {
	if w.Type == core.TypeService {
		return w.validateService()
	}

	errs := []error{}

	if slices.Contains(w.Retry.RetryOn, RetryOnExit) {
		errs = append(errs, fmt.Errorf(
			"retry.retry_on %q is only valid for a service: a task that exits has finished",
			RetryOnExit,
		))
	}

	if w.Retry.MaxAttempts.Unlimited() {
		errs = append(errs, errors.New(
			`retry.max_attempts "unlimited" is only valid for a service`,
		))
	}

	return errs
}

func (w *Worker) validateService() []error {
	errs := []error{}

	if w.Retry.Enabled != nil && !*w.Retry.Enabled {
		errs = append(errs, errors.New(
			"retry.enabled must not be false for a service: a service that is not restarted is a task",
		))
	}

	if w.Timeout != 0 {
		errs = append(errs, errors.New(
			"timeout must not be set for a service: a service has no deadline to exceed",
		))
	}

	if len(w.Retry.SuccessExitCodes) > 0 {
		errs = append(errs, errors.New(
			"retry.success_exit_codes must not be set for a service: every exit is abnormal",
		))
	}

	if w.Logs.Rotate.MaxFiles < 1 {
		errs = append(errs, errors.New(
			"logs.rotate.max_files must be at least 1 for a service: "+
				"a single attempt runs long enough to fill its cap and would then store nothing",
		))
	}

	return errs
}
