// Package daemon wires the Processd components together and owns the process
// lifecycle. Dependencies are constructed here, once, by hand.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/curruwilla/processd/internal/api"
	"github.com/curruwilla/processd/internal/config"
	"github.com/curruwilla/processd/internal/logstore"
	"github.com/curruwilla/processd/internal/queue"
	"github.com/curruwilla/processd/internal/runner"
	"github.com/curruwilla/processd/internal/store"
	"github.com/curruwilla/processd/internal/store/sqlite"
	"github.com/curruwilla/processd/internal/supervisor"
)

// databaseFile is the SQLite file created inside the configured data dir.
const databaseFile = "processd.db"

// garbageInterval is how often the retention limits are enforced.
const garbageInterval = time.Hour

// Daemon is the assembled application.
type Daemon struct {
	cfg config.Config
	log *slog.Logger

	store      store.Store
	logs       *logstore.Store
	supervisor *supervisor.Supervisor
	scheduler  *queue.Scheduler
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
	sup := supervisor.New(cfg, db, runner.NewExecRunner(), logs, log)
	scheduler := queue.New(cfg, db, registry, sup, log)

	// The supervisor reads worker policy at attempt time and hands executions
	// back to the scheduler when an attempt ends.
	sup.SetWorkers(scheduler.Registry)
	sup.SetOnFinish(scheduler.OnAttemptFinished)
	sup.SetOnChange(scheduler.Notify)

	d := &Daemon{
		cfg:        cfg,
		log:        log,
		store:      db,
		logs:       logs,
		supervisor: sup,
		scheduler:  scheduler,
	}

	d.api = api.New(api.Options{
		Config:     cfg,
		Store:      db,
		Scheduler:  scheduler,
		Supervisor: sup,
		Logs:       logs,
		Logger:     log,
		Reload:     d.Reload,
	})

	if err := os.MkdirAll(cfg.LogDir, 0o750); err != nil {
		return nil, fmt.Errorf("creating log dir %q: %w", cfg.LogDir, err)
	}

	log.Info("workers loaded", slog.Int("count", registry.Len()))

	return d, nil
}

// Reload re-reads workers.d. A failed reload leaves the running set untouched.
func (d *Daemon) Reload(ctx context.Context) error {
	registry, err := config.LoadWorkers(d.cfg.WorkersDir)
	if err != nil {
		return fmt.Errorf("reloading workers: %w", err)
	}

	d.scheduler.SetRegistry(registry)
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

	grace := d.cfg.ShutdownGrace.Duration()

	err := d.api.Serve(ctx, grace)

	d.scheduler.Drain()

	if shutdownErr := d.supervisor.Shutdown(context.WithoutCancel(ctx), grace); shutdownErr != nil {
		err = errors.Join(err, fmt.Errorf("stopping executions: %w", shutdownErr))
	}

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
