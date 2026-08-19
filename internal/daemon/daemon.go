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
