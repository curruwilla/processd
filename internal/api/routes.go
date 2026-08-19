package api

import (
	"log/slog"
	"net/http"
	"time"
)

// publicPaths never require authentication.
var publicPaths = map[string]bool{"/v1/health": true}

// Handler builds the routing tree with its middleware chain.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/processes", s.createProcess)
	mux.HandleFunc("GET /v1/processes", s.listProcesses)
	mux.HandleFunc("GET /v1/processes/{id}", s.getProcess)
	mux.HandleFunc("DELETE /v1/processes/{id}", s.deleteProcess)
	mux.HandleFunc("POST /v1/processes/{id}/signal", s.signalProcess)
	mux.HandleFunc("GET /v1/processes/{id}/logs", s.processLogs)

	mux.HandleFunc("GET /v1/workers", s.listWorkers)
	mux.HandleFunc("POST /v1/reload", s.reloadWorkers)

	mux.HandleFunc("GET /v1/health", s.health)
	mux.HandleFunc("GET /v1/stats", s.stats)

	return s.recoverPanics(s.logRequests(s.authenticate(mux)))
}

// authenticate rejects every request without a valid bearer token, except on
// the public liveness endpoint.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publicPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}

		token, ok := s.auth.authenticate(r.Header.Get("Authorization"))
		if !ok {
			writeError(w, s.log, &apiError{
				Status:  http.StatusUnauthorized,
				Code:    "unauthorized",
				Message: "missing or invalid api token",
			})

			return
		}

		next.ServeHTTP(w, withToken(r, token))
	})
}

// logRequests emits one structured line per request. The message stays constant
// so that log aggregation groups by route, not by URL.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(recorder, r)

		s.log.Info("http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", recorder.status),
			slog.Duration("duration", time.Since(started)),
		)
	})
}

// recoverPanics keeps one broken handler from taking the daemon down.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}

			s.log.Error("handler panic",
				slog.Any("panic", recovered),
				slog.String("path", r.URL.Path),
			)

			writeError(w, s.log, &apiError{
				Status:  http.StatusInternalServerError,
				Code:    "internal_error",
				Message: "internal error",
			})
		}()

		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the response status for the access log.
type statusRecorder struct {
	http.ResponseWriter

	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
