// Package daemon wires the Processd components together and owns the process
// lifecycle. Dependencies are constructed here, once, by hand.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/curruwilla/processd/internal/api"
	"github.com/curruwilla/processd/internal/config"
	"github.com/curruwilla/processd/internal/fleet"
	"github.com/curruwilla/processd/internal/logstore"
	"github.com/curruwilla/processd/internal/metrics"
	"github.com/curruwilla/processd/internal/notify"
	"github.com/curruwilla/processd/internal/queue"
	"github.com/curruwilla/processd/internal/runner"
	"github.com/curruwilla/processd/internal/schedule"
	"github.com/curruwilla/processd/internal/store"
	"github.com/curruwilla/processd/internal/store/sqlite"
	"github.com/curruwilla/processd/internal/supervisor"
	"github.com/curruwilla/processd/internal/webui"
)

// databaseFile is the SQLite file created inside the configured data dir.
const databaseFile = "processd.db"

// garbageInterval is how often the retention limits are enforced.
const garbageInterval = time.Hour

// notifyDrainBudget is how long shutdown waits for queued notifications.
//
// The same reasoning as apiShutdownBudget: the configured grace exists for the
// supervised processes, and an unreachable webhook must not hold the node up
// for tens of seconds on the way down.
const notifyDrainBudget = 5 * time.Second

// Daemon is the assembled application.
type Daemon struct {
	cfg config.Config
	log *slog.Logger

	store      store.Store
	logs       *logstore.Store
	supervisor *supervisor.Supervisor
	scheduler  *queue.Scheduler
	schedules  *schedule.Runner
	notifier   *notify.Notifier
	fleet      *fleet.Fleet
	api        *api.Server
}

// New builds the object graph from the configuration.
func New(cfg config.Config, log *slog.Logger) (*Daemon, error) {
	ensureFileLimit(cfg.MaxProcesses, log)

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, fmt.Errorf("creating data dir %q: %w", cfg.DataDir, err)
	}

	registry, err := config.LoadWorkers(cfg.WorkersDir)
	if err != nil {
		return nil, fmt.Errorf("loading workers: %w", err)
	}

	db, err := sqlite.Open(filepath.Join(cfg.DataDir, databaseFile))
	if err != nil {
		return nil, err
	}

	logs := logstore.New(cfg.LogDir, cfg.Logs.MaxBytesPerStream.Bytes())
	observed := metrics.NewRegistry()
	sup := supervisor.New(cfg, db, runner.NewExecRunner(), logs, log)
	scheduler := queue.New(cfg, db, registry, sup, log)

	// Only the supervisor follows an attempt from start to outcome, so the
	// counters behind /v1/metrics are fed from there.
	sup.SetMetrics(observed)

	// The supervisor reads worker policy at attempt time and hands executions
	// back to the scheduler when an attempt ends.
	sup.SetWorkers(scheduler.Registry)
	sup.SetOnSettle(scheduler.OnExecutionSettled)

	if err := validateNotifyTarget(cfg.Notify, registry); err != nil {
		return nil, err
	}

	// nil unless this daemon is a hub, which is what keeps the aggregation
	// routes off an ordinary node entirely.
	aggregate, err := fleet.New(cfg.Fleet, log)
	if err != nil {
		return nil, err
	}

	d := &Daemon{
		cfg:        cfg,
		log:        log,
		store:      db,
		logs:       logs,
		supervisor: sup,
		scheduler:  scheduler,
		schedules:  schedule.New(scheduler.Registry, scheduler, db, log),
		fleet:      aggregate,
		notifier: notify.New(notify.Options{
			Fallback: cfg.Notify,
			Workers:  scheduler.Registry,
			Submit:   scheduler,
			Logs:     logs,
			Node:     nodeName(log),
			Logger:   log,
		}),
	}

	// Injected rather than constructed inside the supervisor: a notification may
	// run a worker, which needs the scheduler the supervisor is built before.
	sup.SetNotifier(d.notifier)

	console, err := buildConsole(cfg)
	if err != nil {
		return nil, err
	}

	d.api = api.New(api.Options{
		Config:     cfg,
		Store:      db,
		Scheduler:  scheduler,
		Supervisor: sup,
		Logs:       logs,
		Metrics:    observed,
		Logger:     log,
		Reload:     d.Reload,
		Schedules:  d.schedules,
		Fleet:      d.fleet,
		UI:         console,
	})

	if err := os.MkdirAll(cfg.LogDir, 0o750); err != nil {
		return nil, fmt.Errorf("creating log dir %q: %w", cfg.LogDir, err)
	}

	log.Info("workers loaded", slog.Int("count", registry.Len()))

	return d, nil
}

// validateNotifyTarget refuses a daemon-wide notification pointing at a worker
// that is not there.
//
// A worker's own policy is checked when workers.d loads; the daemon-wide one
// cannot be, because the two files are read separately. Fail closed at boot
// rather than discover it on the first failure it was meant to report.
func validateNotifyTarget(policy config.Notify, registry *config.Registry) error {
	if policy.Exec == nil {
		return nil
	}

	target, err := registry.Get(policy.Exec.Worker)
	if err != nil {
		return fmt.Errorf("notify.exec.worker: %w", err)
	}

	if target.Notify.IsSet() {
		return fmt.Errorf(
			"notify.exec.worker %q declares notify of its own, which would notify about notifications",
			target.Name,
		)
	}

	return nil
}

// nodeName labels notifications with the host that produced them, so a payload
// arriving in a shared channel says which node it came from.
func nodeName(log *slog.Logger) string {
	name, err := os.Hostname()
	if err != nil {
		log.Warn("reading the hostname", slog.Any("error", err))
		return "unknown"
	}

	return name
}

// buildConsole returns the console handler, or nil when the operator turned the
// web UI off.
func buildConsole(cfg config.Config) (http.Handler, error) {
	if !cfg.UI.Enabled {
		return nil, nil
	}

	return webui.Handler()
}

// Reload re-reads workers.d. A failed reload leaves the running set untouched.
func (d *Daemon) Reload(ctx context.Context) error {
	registry, err := config.LoadWorkers(d.cfg.WorkersDir)
	if err != nil {
		return fmt.Errorf("reloading workers: %w", err)
	}

	d.scheduler.SetRegistry(registry)
	d.schedules.Reload()
	d.log.Info("workers reloaded", slog.Int("count", registry.Len()))

	return nil
}

// Run starts every component and blocks until the process is asked to stop.
//
// Shutdown order matters (docs/SPEC.md §11): stop admitting work, stop
// dispatching, terminate running process groups within the grace period, then
// persist and close.
func (d *Daemon) Run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := d.supervisor.Reconcile(ctx); err != nil {
		// A reconciliation failure must not block the daemon start: log it and
		// keep going, the affected executions stay unfinished in the store.
		d.log.Error("reconciling previous state", slog.Any("error", err))
	}

	go d.watchReload(ctx)
	go d.collectGarbage(ctx)

	// Anything left pending by the previous run is dispatched immediately.
	d.scheduler.Notify()

	var wg sync.WaitGroup

	wg.Go(func() {
		if err := d.scheduler.Run(ctx); err != nil {
			d.log.Error("scheduler stopped", slog.Any("error", err))
		}
	})

	wg.Go(func() {
		if err := d.schedules.Run(ctx); err != nil {
			d.log.Error("schedule runner stopped", slog.Any("error", err))
		}
	})

	wg.Go(func() {
		if err := d.notifier.Run(ctx); err != nil {
			d.log.Error("notifier stopped", slog.Any("error", err))
		}
	})

	if d.fleet != nil {
		wg.Go(func() {
			if err := d.fleet.Run(ctx); err != nil {
				d.log.Error("fleet poller stopped", slog.Any("error", err))
			}
		})
	}

	grace := d.cfg.ShutdownGrace.Duration()

	err := d.api.Serve(ctx, grace)

	d.scheduler.Drain()

	if shutdownErr := d.supervisor.Shutdown(context.WithoutCancel(ctx), grace); shutdownErr != nil {
		err = errors.Join(err, fmt.Errorf("stopping executions: %w", shutdownErr))
	}

	// After the supervisor, so the outcomes it settled on the way down are still
	// reported, and before the wait, so the notifier has something to return on.
	d.notifier.Close(notifyDrainBudget)

	wg.Wait()

	return err
}

// watchReload reloads workers on SIGHUP, the conventional way to re-read
// configuration without dropping running work.
func (d *Daemon) watchReload(ctx context.Context) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)

	defer signal.Stop(hup)

	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			if err := d.Reload(ctx); err != nil {
				d.log.Error("reload failed", slog.Any("error", err))
			}
		}
	}
}

// Close releases resources owned by the daemon.
func (d *Daemon) Close() error {
	if err := d.store.Close(); err != nil {
		return fmt.Errorf("closing store: %w", err)
	}

	return nil
}

// collectGarbage trims the history and the log directory on a slow tick.
// Without it, both grow without bound (docs/SPEC.md §10, §17).
func (d *Daemon) collectGarbage(ctx context.Context) {
	ticker := time.NewTicker(garbageInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.purgeOnce(ctx)
		}
	}
}

func (d *Daemon) purgeOnce(ctx context.Context) {
	if retention := d.cfg.History.Retention.Duration(); retention > 0 {
		removed, err := d.store.PurgeHistory(ctx, time.Now().UTC().Add(-retention), d.cfg.History.MaxRows)
		if err != nil {
			d.log.Error("purging history", slog.Any("error", err))
		}

		if removed > 0 {
			d.log.Info("purged history", slog.Int("executions", removed))
		}
	}

	if retention := d.cfg.Logs.Retention.Duration(); retention > 0 {
		removed, err := d.logs.Purge(time.Now().Add(-retention))
		if err != nil {
			d.log.Error("purging logs", slog.Any("error", err))
		}

		if removed > 0 {
			d.log.Info("purged logs", slog.Int("files", removed))
		}
	}
}
