package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// publicPaths never require authentication.
var publicPaths = map[string]bool{"/v1/health": true}

// uiPrefix is where the built-in console is mounted. Its assets are static and
// carry no execution data: the page asks the operator for a token and then
// calls the same authenticated API as every other client.
const uiPrefix = "/ui/"

// Handler builds the routing tree with its middleware chain.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/processes", s.createProcess)
	mux.HandleFunc("GET /v1/processes", s.listProcesses)
	mux.HandleFunc("GET /v1/processes/{id}", s.getProcess)
	mux.HandleFunc("DELETE /v1/processes/{id}", s.deleteProcess)
	mux.HandleFunc("POST /v1/processes/{id}/signal", s.signalProcess)
	mux.HandleFunc("GET /v1/processes/{id}/logs", s.processLogs)
	mux.HandleFunc("GET /v1/processes/{id}/logs/stream", s.streamProcessLogs)

	mux.HandleFunc("GET /v1/workers", s.listWorkers)
	mux.HandleFunc("POST /v1/reload", s.reloadWorkers)

	s.mountFleet(mux)

	mux.HandleFunc("GET /v1/health", s.health)
	mux.HandleFunc("GET /v1/stats", s.stats)
	mux.HandleFunc("GET /v1/metrics", s.metrics)

	s.mountUI(mux)

	return s.recoverPanics(s.logRequests(s.authenticate(mux)))
}

// mountFleet adds the aggregation routes, and only on a hub.
//
// A daemon that aggregates nothing does not answer them at all: the routes are
// absent rather than empty, so nothing suggests a fleet where there is none.
//
// Writes are proxied as well as reads, but only ever to the node the client
// named: the hub forwards, it never chooses. Whether a hub may write at all is
// the operator's decision and is made by which token they install for each node
// — a read_only one and the node refuses, whatever the hub was asked to do.
func (s *Server) mountFleet(mux *http.ServeMux) {
	if s.fleet == nil {
		return
	}

	mux.HandleFunc("GET /v1/fleet/nodes", s.listFleetNodes)

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		mux.HandleFunc(method+" /v1/fleet/nodes/{node}/{path...}", s.proxyToNode)
	}
}

// mountUI serves the console and sends the bare root to it, so that opening the
// listen address in a browser lands somewhere useful.
func (s *Server) mountUI(mux *http.ServeMux) {
	if s.ui == nil {
		return
	}

	mux.Handle("GET "+uiPrefix, http.StripPrefix(strings.TrimSuffix(uiPrefix, "/"), s.ui))

	redirect := func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, uiPrefix, http.StatusFound)
	}

	mux.HandleFunc("GET /ui", redirect)
	mux.HandleFunc("GET /{$}", redirect)
}

// isPublic reports whether a path is served without a token.
func (s *Server) isPublic(path string) bool {
	if publicPaths[path] {
		return true
	}

	if s.ui == nil {
		return false
	}

	return path == "/" || path == "/ui" || strings.HasPrefix(path, uiPrefix)
}

// authenticate rejects every request without a valid bearer token, except on
// the public liveness endpoint.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.isPublic(r.URL.Path) {
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

		// A read-only token may look at anything it is allowed to see and change
		// nothing. Every state-changing route in the API is a non-safe method,
		// so the rule is the method, not a list that a new route could fall off.
		if token.ReadOnly && !isSafeMethod(r.Method) {
			writeError(w, s.log, &apiError{
				Status:  http.StatusForbidden,
				Code:    "read_only_token",
				Message: fmt.Sprintf("token %q is read-only and may not %s", token.Name, r.Method),
			})

			return
		}

		next.ServeHTTP(w, withToken(r, token))
	})
}

// isSafeMethod reports whether a method only reads.
func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
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

// Unwrap exposes the writer underneath to http.ResponseController. Without it,
// the access log would cost every streaming handler its ability to flush and to
// clear the write deadline, and a log stream would arrive all at once, at the
// end, if at all.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
