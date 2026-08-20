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

// apiShutdownBudget caps how long the HTTP server waits for its connections.
//
// The configured grace period exists for the supervised processes, which may
// legitimately need tens of seconds; API requests are short, and a client
// holding an idle keep-alive connection must not delay the daemon by that long.
const apiShutdownBudget = 5 * time.Second

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

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), min(grace, apiShutdownBudget))
	defer cancel()

	// Graceful first, forced second. A client holding an idle connection open
	// must not keep the daemon alive past its grace period, and a shutdown that
	// had to force connections closed is still a successful shutdown.
	if err := srv.Shutdown(shutdownCtx); err != nil {
		s.log.Warn("forcing api shutdown", slog.Any("error", err))

		if closeErr := srv.Close(); closeErr != nil {
			return fmt.Errorf("closing api: %w", closeErr)
		}
	}

	return <-errCh
}
