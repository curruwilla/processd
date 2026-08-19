package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/curruwilla/processd/internal/config"
	"github.com/curruwilla/processd/internal/logstore"
	"github.com/curruwilla/processd/internal/queue"
	"github.com/curruwilla/processd/internal/store"
	"github.com/curruwilla/processd/internal/supervisor"
)

// Server serves the REST API.
type Server struct {
	cfg        config.Config
	store      store.Store
	scheduler  *queue.Scheduler
	supervisor *supervisor.Supervisor
	logs       *logstore.Store
	auth       *authenticator
	log        *slog.Logger

	// reload re-reads workers.d. Injected so the API does not own file access.
	reload func(context.Context) error
}

// Options groups the server dependencies, keeping the constructor readable.
type Options struct {
	Config     config.Config
	Store      store.Store
	Scheduler  *queue.Scheduler
	Supervisor *supervisor.Supervisor
	Logs       *logstore.Store
	Logger     *slog.Logger
	Reload     func(context.Context) error
}

// New wires the API server.
func New(opts Options) *Server {
	return &Server{
		cfg:        opts.Config,
		store:      opts.Store,
		scheduler:  opts.Scheduler,
		supervisor: opts.Supervisor,
		logs:       opts.Logs,
		auth:       newAuthenticator(opts.Config.Auth.Tokens),
		log:        opts.Logger,
		reload:     opts.Reload,
	}
}

// Serve listens and serves until ctx is cancelled, then shuts down gracefully
// within grace.
func (s *Server) Serve(ctx context.Context, grace time.Duration) error {
	srv := &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	errCh := make(chan error, 1)

	go func() {
		s.log.Info("api listening", slog.String("addr", s.cfg.Listen))

		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("serving api: %w", err)
			return
		}

		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down api: %w", err)
	}

	return <-errCh
}
