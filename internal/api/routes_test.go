package api

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/curruwilla/processd/internal/config"
)

// newTestServer builds a server with only the dependencies the routing and
// authentication tests exercise.
func newTestServer(t *testing.T) *Server {
	t.Helper()

	cfg := config.Default()
	cfg.Auth.Tokens = []config.Token{{Name: "ops", Hash: HashToken("ops-secret")}}

	return New(Options{
		Config: cfg,
		Logger: slog.New(slog.DiscardHandler),
	})
}

func TestServer_Handler_authentication(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		method     string
		path       string
		authHeader string
		wantStatus int
	}{
		{name: "health needs no token", method: http.MethodGet, path: "/v1/health", wantStatus: http.StatusOK},
		{
			name:       "processes without a token",
			method:     http.MethodGet,
			path:       "/v1/processes",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "processes with a wrong token",
			method:     http.MethodGet,
			path:       "/v1/processes",
			authHeader: "Bearer wrong",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "unknown route",
			method:     http.MethodGet,
			path:       "/v1/nope",
			authHeader: "Bearer ops-secret",
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}

			rec := httptest.NewRecorder()
			newTestServer(t).Handler().ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
		})
	}
}

func TestServer_health(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/health", nil)
	rec := httptest.NewRecorder()

	newTestServer(t).Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	if contentType := rec.Header().Get("Content-Type"); contentType != "application/json" {
		t.Errorf("content type = %q, want application/json", contentType)
	}
}
