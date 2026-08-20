package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"time"

	"github.com/curruwilla/processd/internal/core"
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
)

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

	Retry Retry      `yaml:"retry"`
	Logs  WorkerLogs `yaml:"logs"`
}

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
type Retry struct {
	Enabled          bool           `yaml:"enabled"`
	MaxAttempts      int            `yaml:"max_attempts"`
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
	MaxBytesPerStream ByteSize `yaml:"max_bytes_per_stream"`
}

// IsEnabled reports whether the worker may be executed. Workers are enabled
// unless the file says otherwise.
func (w *Worker) IsEnabled() bool { return w.Enabled == nil || *w.Enabled }

// Allows reports whether a request may override the given field.
func (w *Worker) Allows(field Overridable) bool { return slices.Contains(w.Overridable, field) }

// RetriesOn reports whether the trigger is in the worker's retry policy.
func (w *Worker) RetriesOn(trigger RetryTrigger) bool {
	return w.Retry.Enabled && slices.Contains(w.Retry.RetryOn, trigger)
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

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	return registry, nil
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

	if w.Retry.Enabled {
		applyRetryDefaults(&w.Retry)
	}
}

func applyRetryDefaults(r *Retry) {
	if r.MaxAttempts == 0 {
		r.MaxAttempts = 1
	}

	if len(r.RetryOn) == 0 {
		r.RetryOn = []RetryTrigger{RetryOnNonZeroExit, RetryOnSignal, RetryOnStartError}
	}

	if len(r.SuccessExitCodes) == 0 {
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

	if w.Type != core.TypeTask {
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
	if !w.Retry.Enabled {
		return nil
	}

	errs := []error{}

	if w.Retry.MaxAttempts < 1 {
		errs = append(errs, errors.New("retry.max_attempts must be at least 1"))
	}

	if w.Retry.Backoff.Jitter < 0 || w.Retry.Backoff.Jitter > 1 {
		errs = append(errs, errors.New("retry.backoff.jitter must be between 0 and 1"))
	}

	switch w.Retry.Backoff.Type {
	case BackoffExponential, BackoffLinear, BackoffFixed:
	default:
		errs = append(errs, fmt.Errorf("retry.backoff.type %q is unknown", w.Retry.Backoff.Type))
	}

	for _, trigger := range w.Retry.RetryOn {
		switch trigger {
		case RetryOnNonZeroExit, RetryOnSignal, RetryOnStartError, RetryOnTimeout:
		default:
			errs = append(errs, fmt.Errorf("retry.retry_on %q is unknown", trigger))
		}
	}

	return errs
}
