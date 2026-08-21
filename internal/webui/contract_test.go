package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The console reads the public API, so a field renamed on the server side turns
// a panel blank instead of failing anything. These are the response fields the
// page depends on by name; renaming one has to break here, next to the JSON tag
// that moved, rather than in a browser nobody has open.
func TestConsole_readsTheDocumentedFields(t *testing.T) {
	t.Parallel()

	fields := []string{
		// GET /v1/stats
		"slots_used", "queue_depth", "stats.services",
		// GET /v1/processes
		"item.type", "item.restarts", "item.retry_at", "item.duration_ms",
		"item.max_attempts", "item.log_truncated",
	}

	body := fetch(t, "/app.js")

	for _, field := range fields {
		if !strings.Contains(body, field) {
			t.Errorf("the console no longer reads %q: the API field was renamed, or the panel was dropped", field)
		}
	}
}

// The service panels are wired by id from the script, so the two files have to
// agree on them.
func TestConsole_serviceElementsExist(t *testing.T) {
	t.Parallel()

	page := fetch(t, "/index.html")
	script := fetch(t, "/app.js")

	for _, id := range []string{
		"card-services", "card-services-hint", "card-services-wrap",
		"card-restarts", "card-restarts-wrap", "filter-type",
	} {
		if !strings.Contains(page, `id="`+id+`"`) {
			t.Errorf("index.html has no element %q", id)
		}

		if !strings.Contains(script, `'`+id+`'`) {
			t.Errorf("app.js never reads element %q", id)
		}
	}
}

func fetch(t *testing.T, path string) string {
	t.Helper()

	rec := httptest.NewRecorder()
	newHandler(t).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want %d", path, rec.Code, http.StatusOK)
	}

	return rec.Body.String()
}
