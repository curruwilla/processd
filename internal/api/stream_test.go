package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// sseEvent is one parsed frame of an event stream.
type sseEvent struct {
	name string
	data string
}

// parseEvents splits a recorded event stream into its frames, dropping the
// heartbeat comments.
func parseEvents(t *testing.T, body string) []sseEvent {
	t.Helper()

	events := []sseEvent{}

	for frame := range strings.SplitSeq(body, "\n\n") {
		var event sseEvent

		for line := range strings.SplitSeq(frame, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				event.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				event.data += strings.TrimPrefix(line, "data: ")
			}
		}

		if event.name != "" {
			events = append(events, event)
		}
	}

	return events
}

func decodeEvent[T any](t *testing.T, event sseEvent) T {
	t.Helper()

	var out T
	if err := json.Unmarshal([]byte(event.data), &out); err != nil {
		t.Fatalf("decoding %s event %q: %v", event.name, event.data, err)
	}

	return out
}

func TestServer_streamProcessLogs_finishedAttempt(t *testing.T) {
	t.Parallel()

	handler := newLiveServer(t)

	created := decode[createProcessResponse](t,
		do(t, handler, http.MethodPost, "/v1/processes", `{"worker":"hello","params":{"id":"42"}}`, nil))
	awaitState(t, handler, created.ID)

	rec := do(t, handler, http.MethodGet, "/v1/processes/"+created.ID+"/logs/stream?tail=0", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET stream returned %d (%s), want 200", rec.Code, rec.Body.String())
	}

	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}

	events := parseEvents(t, rec.Body.String())
	if len(events) < 2 {
		t.Fatalf("stream produced %d events, want the line and the end:\n%s", len(events), rec.Body.String())
	}

	line := decodeEvent[logLine](t, events[0])
	if line.Stream != "stdout" || !strings.Contains(line.Text, "hello 42") {
		t.Errorf("first event = %+v, want the captured stdout line", line)
	}

	end := decodeEvent[logStreamEnd](t, events[len(events)-1])
	if events[len(events)-1].name != "end" || end.Status != "COMPLETED" || end.Attempt != 1 {
		t.Errorf("last event = %+v, want the end of a completed attempt", end)
	}
}

func TestServer_streamProcessLogs_followsRunningAttempt(t *testing.T) {
	t.Parallel()

	handler := newLiveServer(t)

	created := decode[createProcessResponse](t,
		do(t, handler, http.MethodPost, "/v1/processes", `{"worker":"chatty"}`, nil))

	// The stream is opened while the execution is still writing, and returns
	// only once the attempt has ended.
	rec := do(t, handler, http.MethodGet, "/v1/processes/"+created.ID+"/logs/stream", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET stream returned %d (%s), want 200", rec.Code, rec.Body.String())
	}

	streams := map[string]string{}

	for _, event := range parseEvents(t, rec.Body.String()) {
		if event.name != "line" {
			continue
		}

		line := decodeEvent[logLine](t, event)
		streams[line.Stream] += line.Text
	}

	if !strings.Contains(streams["stdout"], "one") {
		t.Errorf("stdout = %q, want the first line", streams["stdout"])
	}

	if !strings.Contains(streams["stderr"], "two") {
		t.Errorf("stderr = %q, want the line written after the pause", streams["stderr"])
	}
}

func TestServer_streamProcessLogs_validation(t *testing.T) {
	t.Parallel()

	handler := newLiveServer(t)

	created := decode[createProcessResponse](t,
		do(t, handler, http.MethodPost, "/v1/processes", `{"worker":"hello","params":{"id":"3"}}`, nil))
	awaitState(t, handler, created.ID)

	tests := []struct {
		name  string
		query string
		want  int
	}{
		{name: "unknown stream", query: "?stream=syslog", want: http.StatusBadRequest},
		{name: "attempt that never ran", query: "?attempt=9", want: http.StatusBadRequest},
		{name: "negative tail", query: "?tail=-1", want: http.StatusBadRequest},
		{name: "unknown execution", query: "", want: http.StatusNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			id := created.ID
			if tc.want == http.StatusNotFound {
				id = "proc_missing"
			}

			rec := do(t, handler, http.MethodGet, "/v1/processes/"+id+"/logs/stream"+tc.query, "", nil)
			if rec.Code != tc.want {
				t.Errorf("GET stream%s returned %d, want %d", tc.query, rec.Code, tc.want)
			}
		})
	}
}
