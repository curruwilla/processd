package logstore

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// appendTo adds bytes to a log file the way a running process would.
func appendTo(t *testing.T, path, content string) {
	t.Helper()

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("opening %q: %v", path, err)
	}

	if _, err := file.WriteString(content); err != nil {
		t.Fatalf("appending to %q: %v", path, err)
	}

	if err := file.Close(); err != nil {
		t.Fatalf("closing %q: %v", path, err)
	}
}

// writeStream appends raw bytes to one attempt stream, creating it if needed.
func writeStream(t *testing.T, store *Store, id string, attempt int, stream Stream, at time.Time, content string) {
	t.Helper()

	written, err := store.Create(id, attempt, at, Policy{})
	if err != nil {
		t.Fatalf("Create() returned %v, want nil", err)
	}

	target := written.Stdout
	if stream == StreamStderr {
		target = written.Stderr
	}

	if _, err := target.Write([]byte(content)); err != nil {
		t.Fatalf("writing %s: %v", stream, err)
	}

	if err := written.Close(); err != nil {
		t.Fatalf("Close() returned %v, want nil", err)
	}
}

func TestStore_TailFollowsNewOutput(t *testing.T) {
	t.Parallel()

	store := New(t.TempDir(), 1<<20)
	at := time.Now().UTC()

	tailer, err := store.Tail("proc_01K", 1, StreamStdout, at, 0)
	if err != nil {
		t.Fatalf("Tail() returned %v, want nil", err)
	}

	// Nothing has been written yet: the attempt may not have started.
	lines, err := tailer.Read()
	if err != nil {
		t.Fatalf("Read() returned %v, want nil", err)
	}

	if len(lines) != 0 {
		t.Fatalf("Read() = %v, want no lines", lines)
	}

	writeStream(t, store, "proc_01K", 1, StreamStdout, at, "first\nsecond\n")

	lines, err = tailer.Read()
	if err != nil {
		t.Fatalf("Read() returned %v, want nil", err)
	}

	if len(lines) != 2 || lines[0].Text != "first" || lines[1].Text != "second" {
		t.Fatalf("Read() = %v, want first and second", lines)
	}

	// A second read must not repeat what was already delivered.
	lines, err = tailer.Read()
	if err != nil {
		t.Fatalf("Read() returned %v, want nil", err)
	}

	if len(lines) != 0 {
		t.Errorf("Read() = %v, want no lines", lines)
	}
}

func TestStore_TailHoldsBackPartialLines(t *testing.T) {
	t.Parallel()

	store := New(t.TempDir(), 1<<20)
	at := time.Now().UTC()

	writeStream(t, store, "proc_01K", 1, StreamStdout, at, "half")

	tailer, err := store.Tail("proc_01K", 1, StreamStdout, at, 0)
	if err != nil {
		t.Fatalf("Tail() returned %v, want nil", err)
	}

	lines, err := tailer.Read()
	if err != nil {
		t.Fatalf("Read() returned %v, want nil", err)
	}

	if len(lines) != 0 {
		t.Fatalf("Read() = %v, want the partial line held back", lines)
	}

	appendTo(t, store.Path("proc_01K", 1, StreamStdout, at), " and half\n")

	lines, err = tailer.Read()
	if err != nil {
		t.Fatalf("Read() returned %v, want nil", err)
	}

	if len(lines) != 1 || lines[0].Text != "half and half" {
		t.Errorf("Read() = %v, want the reassembled line", lines)
	}
}

func TestStore_TailBacklog(t *testing.T) {
	t.Parallel()

	store := New(t.TempDir(), 1<<20)
	at := time.Now().UTC()

	var content strings.Builder

	for i := range 50 {
		fmt.Fprintf(&content, "line %d\n", i)
	}

	writeStream(t, store, "proc_01K", 1, StreamStdout, at, content.String())

	tests := []struct {
		name    string
		backlog int
		want    int
		first   string
	}{
		{name: "everything", backlog: 0, want: 50, first: "line 0"},
		{name: "last three", backlog: 3, want: 3, first: "line 47"},
		{name: "only new output", backlog: -1, want: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tailer, err := store.Tail("proc_01K", 1, StreamStdout, at, tc.backlog)
			if err != nil {
				t.Fatalf("Tail() returned %v, want nil", err)
			}

			lines, err := tailer.Read()
			if err != nil {
				t.Fatalf("Read() returned %v, want nil", err)
			}

			if len(lines) != tc.want {
				t.Fatalf("Read() returned %d lines, want %d", len(lines), tc.want)
			}

			if tc.want > 0 && lines[0].Text != tc.first {
				t.Errorf("first line = %q, want %q", lines[0].Text, tc.first)
			}
		})
	}
}

func TestStore_TailBothStreamsAreLabelled(t *testing.T) {
	t.Parallel()

	store := New(t.TempDir(), 1<<20)
	at := time.Now().UTC()

	attempt, err := store.Create("proc_01K", 1, at, Policy{})
	if err != nil {
		t.Fatalf("Create() returned %v, want nil", err)
	}

	if _, err := attempt.Stdout.Write([]byte("out\n")); err != nil {
		t.Fatalf("writing stdout: %v", err)
	}

	if _, err := attempt.Stderr.Write([]byte("err\n")); err != nil {
		t.Fatalf("writing stderr: %v", err)
	}

	if err := attempt.Close(); err != nil {
		t.Fatalf("Close() returned %v, want nil", err)
	}

	tailer, err := store.Tail("proc_01K", 1, StreamBoth, at, 0)
	if err != nil {
		t.Fatalf("Tail() returned %v, want nil", err)
	}

	lines, err := tailer.Read()
	if err != nil {
		t.Fatalf("Read() returned %v, want nil", err)
	}

	if len(lines) != 2 {
		t.Fatalf("Read() returned %d lines, want 2", len(lines))
	}

	if lines[0].Stream != StreamStdout || lines[1].Stream != StreamStderr {
		t.Errorf("Read() = %v, want one line per stream, labelled", lines)
	}
}
