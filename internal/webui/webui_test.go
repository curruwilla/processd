package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newHandler(t *testing.T) http.Handler {
	t.Helper()

	handler, err := Handler()
	if err != nil {
		t.Fatalf("Handler() returned %v, want nil", err)
	}

	return handler
}

func TestHandler_ServesTheConsole(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "root serves the page", path: "/", want: "<title>processd</title>"},
		{name: "index by name", path: "/index.html", want: "<title>processd</title>"},
		{name: "script", path: "/app.js", want: "processd console"},
		{name: "stylesheet", path: "/style.css", want: "--accent"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			handler := newHandler(t)
			handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, tc.path, nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want %d", tc.path, rec.Code, http.StatusOK)
			}

			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("GET %s does not contain %q", tc.path, tc.want)
			}
		})
	}
}

func TestHandler_RefusesAnythingElse(t *testing.T) {
	t.Parallel()

	paths := []string{"/missing.js", "/../webui.go", "/%2e%2e/webui.go"}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			handler := newHandler(t)
			handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))

			if rec.Code == http.StatusOK {
				t.Errorf("GET %s = %d, want a refusal", path, rec.Code)
			}
		})
	}
}
