package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/curruwilla/processd/internal/logstore"
)

const (
	// streamPoll is how often a followed attempt is checked for new output.
	// Polling is deliberate: inotify would add a watch per stream and per file,
	// and a quarter of a second is imperceptible for reading logs.
	streamPoll = 250 * time.Millisecond

	// streamHeartbeat bounds how long a stream stays silent. Proxies close a
	// connection that says nothing, and a comment line costs nothing to send.
	streamHeartbeat = 15 * time.Second

	// defaultStreamBacklog is how many already-written lines a stream opens
	// with when the client asks for no particular tail. Replaying a 32MiB log
	// to a client that wants to watch what happens next helps nobody.
	defaultStreamBacklog = 100
)

// streamProcessLogs follows one attempt over Server-Sent Events
// (docs/SPEC.md §6.8).
//
// SSE, not WebSocket: the traffic is one-way, and a plain HTTP response keeps
// the endpoint reachable with curl and with the same bearer token as the rest
// of the API.
func (s *Server) streamProcessLogs(w http.ResponseWriter, r *http.Request) {
	process, err := s.store.GetProcess(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, s.log, err)
		return
	}

	stream, err := parseStream(r.URL.Query().Get("stream"))
	if err != nil {
		writeError(w, s.log, err)
		return
	}

	attempt, err := parseAttempt(r.URL.Query().Get("attempt"), process.Attempt)
	if err != nil {
		writeError(w, s.log, err)
		return
	}

	backlog, err := parseBacklog(r.URL.Query().Get("tail"))
	if err != nil {
		writeError(w, s.log, err)
		return
	}

	tailer, err := s.logs.Tail(process.ID, attempt, stream, process.CreatedAt, backlog)
	if err != nil {
		writeError(w, s.log, err)
		return
	}

	s.serveStream(w, r, process.ID, attempt, tailer)
}

// serveStream writes the event stream until the attempt ends or the client
// disconnects. Once the headers are out, a failure can only be logged: the
// error contract needs a status code, and that ship has sailed.
func (s *Server) serveStream(w http.ResponseWriter, r *http.Request, id string, attempt int, tailer *logstore.Tailer) {
	control := http.NewResponseController(w)

	// A followed attempt may run for hours; the server-wide write deadline
	// exists for ordinary responses, which are short.
	if err := control.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		s.log.Warn("clearing stream write deadline", slog.Any("error", err))
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	// Reverse proxies buffer responses by default, which would hold every line
	// back until the attempt ends.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ticker := time.NewTicker(streamPoll)
	defer ticker.Stop()

	silent := time.Now()

	for {
		sent, err := s.pump(w, tailer)
		if err != nil {
			s.log.Debug("log stream ended", slog.String("process", id), slog.Any("error", err))
			return
		}

		if sent > 0 {
			silent = time.Now()
		}

		if sent == 0 && !s.attemptLive(r.Context(), id, attempt) {
			s.closeStream(r.Context(), w, tailer, id, attempt)
			return
		}

		if time.Since(silent) >= streamHeartbeat {
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}

			silent = time.Now()
		}

		if err := flush(control); err != nil {
			return
		}

		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

// pump writes every line the attempt has produced since the previous poll and
// reports how many were sent.
func (s *Server) pump(w io.Writer, tailer *logstore.Tailer) (int, error) {
	lines, err := tailer.Read()
	if err != nil {
		return 0, err
	}

	for _, line := range lines {
		event := logLine{Stream: string(line.Stream), Text: line.Text}
		if err := writeEvent(w, "line", event); err != nil {
			return 0, err
		}
	}

	return len(lines), nil
}

// closeStream drains whatever the attempt wrote as it was ending and sends the
// final event. Output flushed between the last poll and the exit would
// otherwise be lost exactly when it matters most.
func (s *Server) closeStream(
	ctx context.Context,
	w http.ResponseWriter,
	tailer *logstore.Tailer,
	id string,
	attempt int,
) {
	if _, err := s.pump(w, tailer); err != nil {
		s.log.Debug("draining log stream", slog.String("process", id), slog.Any("error", err))
		return
	}

	end := logStreamEnd{Attempt: attempt}

	if process, err := s.store.GetProcess(ctx, id); err == nil {
		end.Status = string(process.State)
		end.Truncated = process.LogTruncated
	}

	if err := writeEvent(w, "end", end); err != nil {
		return
	}

	_ = flush(http.NewResponseController(w))
}

// flush pushes what has been written so far to the client. A writer that cannot
// flush does not end the stream: the output still arrives, only later.
func flush(control *http.ResponseController) error {
	if err := control.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}

	return nil
}

// attemptLive reports whether the followed attempt can still produce output:
// it is running now, or it has not started yet.
func (s *Server) attemptLive(ctx context.Context, id string, attempt int) bool {
	if running, ok := s.supervisor.RunningAttempt(id); ok {
		return running == attempt
	}

	process, err := s.store.GetProcess(ctx, id)
	if err != nil {
		return false
	}

	// A queued or retrying execution has not written its attempt yet; a
	// terminal one never will again.
	return !process.State.IsTerminal() && process.Attempt <= attempt
}

// writeEvent renders one Server-Sent Event. Payloads are JSON, so a log line
// containing anything at all cannot break the framing.
func writeEvent(w io.Writer, name string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding %s event: %w", name, err)
	}

	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, encoded); err != nil {
		return fmt.Errorf("writing %s event: %w", name, err)
	}

	return nil
}

// parseBacklog reads the tail parameter of a stream: absent means the default
// backlog, and zero means everything already written.
func parseBacklog(raw string) (int, error) {
	if raw == "" {
		return defaultStreamBacklog, nil
	}

	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, badRequest("tail_invalid", fmt.Sprintf("tail %q is not a positive integer", raw))
	}

	return value, nil
}
