package logstore

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestStore_Path(t *testing.T) {
	t.Parallel()

	store := New("/var/log/processd", 1024)
	at := time.Date(2026, time.August, 19, 17, 30, 0, 0, time.UTC)

	want := filepath.Join("/var/log/processd", "2026", "08", "proc_01K.2.stdout.log")

	if got := store.Path("proc_01K", 2, StreamStdout, at); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

func TestStore_Create(t *testing.T) {
	t.Parallel()

	store := New(t.TempDir(), 1024)
	at := time.Now().UTC()

	attempt, err := store.Create("proc_01K", 1, at, 0)
	if err != nil {
		t.Fatalf("Create() returned %v, want nil", err)
	}

	if _, err := attempt.Stdout.Write([]byte("hello\n")); err != nil {
		t.Fatalf("writing stdout: %v", err)
	}

	if err := attempt.Close(); err != nil {
		t.Fatalf("Close() returned %v, want nil", err)
	}

	reader, err := store.Open("proc_01K", 1, StreamStdout, at)
	if err != nil {
		t.Fatalf("Open() returned %v, want nil", err)
	}
	defer func() {
		_ = reader.Close()
	}()

	content := make([]byte, 16)
	n, _ := reader.Read(content)

	if string(content[:n]) != "hello\n" {
		t.Errorf("stored output = %q, want %q", content[:n], "hello\n")
	}
}

func TestCappedWriter_Write(t *testing.T) {
	t.Parallel()

	t.Run("stores up to the cap and marks truncation", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer

		writer := &CappedWriter{w: &buf, limit: 10}

		if _, err := writer.Write([]byte("0123456")); err != nil {
			t.Fatalf("first Write returned %v, want nil", err)
		}

		n, err := writer.Write([]byte("789abcdef"))
		if err != nil {
			t.Fatalf("second Write returned %v, want nil", err)
		}

		// The child must never see a short write because the daemon stopped
		// storing output.
		if n != len("789abcdef") {
			t.Errorf("Write() = %d, want %d", n, len("789abcdef"))
		}

		if !writer.Truncated() {
			t.Error("Truncated() = false, want true")
		}

		if writer.Written() != 10 {
			t.Errorf("Written() = %d, want 10", writer.Written())
		}

		if !strings.HasPrefix(buf.String(), "0123456789") {
			t.Errorf("stored prefix = %q, want it to start with the first 10 bytes", buf.String())
		}

		if !strings.Contains(buf.String(), "output truncated") {
			t.Error("stored output has no truncation marker, want one")
		}
	})

	t.Run("keeps accepting writes once capped", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer

		writer := &CappedWriter{w: &buf, limit: 2}

		if _, err := writer.Write([]byte("abcd")); err != nil {
			t.Fatalf("Write returned %v, want nil", err)
		}

		before := buf.Len()

		n, err := writer.Write([]byte("more output"))
		if err != nil {
			t.Fatalf("Write after cap returned %v, want nil", err)
		}

		if n != len("more output") {
			t.Errorf("Write() = %d, want %d", n, len("more output"))
		}

		if buf.Len() != before {
			t.Error("output grew after the cap, want it dropped")
		}
	})
}

func TestStore_Lines(t *testing.T) {
	t.Parallel()

	store := New(t.TempDir(), 1024)
	at := time.Now().UTC()

	attempt, err := store.Create("proc_1", 1, at, 0)
	if err != nil {
		t.Fatalf("Create() returned %v, want nil", err)
	}

	if _, err := attempt.Stdout.Write([]byte("first\nsecond\nthird\n")); err != nil {
		t.Fatalf("writing stdout: %v", err)
	}

	if _, err := attempt.Stderr.Write([]byte("boom\n")); err != nil {
		t.Fatalf("writing stderr: %v", err)
	}

	if err := attempt.Close(); err != nil {
		t.Fatalf("Close() returned %v, want nil", err)
	}

	tests := []struct {
		name   string
		stream Stream
		tail   int
		want   []string
	}{
		{name: "stdout only", stream: StreamStdout, want: []string{"first", "second", "third"}},
		{name: "stderr only", stream: StreamStderr, want: []string{"boom"}},
		{
			name:   "both streams are labelled",
			stream: StreamBoth,
			want:   []string{"stdout: first", "stdout: second", "stdout: third", "stderr: boom"},
		},
		{name: "tail keeps the last lines", stream: StreamStdout, tail: 2, want: []string{"second", "third"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := store.Lines("proc_1", 1, tt.stream, at, tt.tail)
			if err != nil {
				t.Fatalf("Lines() returned %v, want nil", err)
			}

			if !slices.Equal(got, tt.want) {
				t.Errorf("Lines() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStore_Lines_missingAttempt(t *testing.T) {
	t.Parallel()

	lines, err := New(t.TempDir(), 1024).Lines("proc_absent", 1, StreamBoth, time.Now().UTC(), 0)
	if err != nil {
		t.Fatalf("Lines() returned %v, want nil", err)
	}

	if len(lines) != 0 {
		t.Errorf("Lines() = %q, want none", lines)
	}
}

func TestStore_Purge(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store := New(root, 1024)

	stale := time.Now().Add(-48 * time.Hour)

	staleAttempt, err := store.Create("proc_old", 1, stale, 0)
	if err != nil {
		t.Fatalf("Create() returned %v, want nil", err)
	}

	if err := staleAttempt.Close(); err != nil {
		t.Fatalf("Close() returned %v, want nil", err)
	}

	// Files are selected by modification time, so the old attempt has to look
	// old on disk as well as in its path.
	stalePath := store.Path("proc_old", 1, StreamStdout, stale)
	if err := os.Chtimes(stalePath, stale, stale); err != nil {
		t.Fatalf("ageing the log file: %v", err)
	}

	if err := os.Chtimes(store.Path("proc_old", 1, StreamStderr, stale), stale, stale); err != nil {
		t.Fatalf("ageing the log file: %v", err)
	}

	fresh, err := store.Create("proc_new", 1, time.Now().UTC(), 0)
	if err != nil {
		t.Fatalf("Create() returned %v, want nil", err)
	}

	if err := fresh.Close(); err != nil {
		t.Fatalf("Close() returned %v, want nil", err)
	}

	removed, err := store.Purge(time.Now().Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("Purge() returned %v, want nil", err)
	}

	if removed != 2 {
		t.Errorf("purge removed %d files, want 2", removed)
	}

	if _, err := os.Stat(stalePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale log still exists, want it purged")
	}

	if _, err := os.Stat(store.Path("proc_new", 1, StreamStdout, time.Now().UTC())); err != nil {
		t.Errorf("recent log was purged: %v", err)
	}
}

func TestStore_Purge_missingRoot(t *testing.T) {
	t.Parallel()

	removed, err := New(filepath.Join(t.TempDir(), "absent"), 1024).Purge(time.Now())
	if err != nil {
		t.Fatalf("Purge() returned %v, want nil", err)
	}

	if removed != 0 {
		t.Errorf("purge removed %d files, want 0", removed)
	}
}

func TestTailLines(t *testing.T) {
	t.Parallel()

	write := func(t *testing.T, content string) File {
		t.Helper()

		path := filepath.Join(t.TempDir(), "log")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("writing fixture: %v", err)
		}

		file, err := os.Open(path)
		if err != nil {
			t.Fatalf("opening fixture: %v", err)
		}

		t.Cleanup(func() {
			_ = file.Close()
		})

		return File{File: file}
	}

	tests := []struct {
		name    string
		content string
		tail    int
		want    []string
	}{
		{name: "empty file", content: "", tail: 5, want: []string{}},
		{name: "fewer lines than asked", content: "a\nb\n", tail: 5, want: []string{"a", "b"}},
		{name: "exactly the last lines", content: "a\nb\nc\nd\n", tail: 2, want: []string{"c", "d"}},
		{name: "no trailing newline", content: "a\nb\nc", tail: 2, want: []string{"b", "c"}},
		{name: "single line", content: "only\n", tail: 3, want: []string{"only"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := tailLines(write(t, tt.content), tt.tail)
			if err != nil {
				t.Fatalf("tailLines() returned %v, want nil", err)
			}

			if !slices.Equal(got, tt.want) {
				t.Errorf("tailLines() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTailLines_crossesChunkBoundary(t *testing.T) {
	t.Parallel()

	// More than one 64KiB chunk, so the walk has to stitch blocks together and
	// discard the partial line it lands on.
	var content strings.Builder

	const lines = 40_000

	for i := range lines {
		fmt.Fprintf(&content, "line-%06d\n", i)
	}

	path := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(path, []byte(content.String()), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening fixture: %v", err)
	}

	defer func() {
		_ = file.Close()
	}()

	got, err := tailLines(File{File: file}, 3)
	if err != nil {
		t.Fatalf("tailLines() returned %v, want nil", err)
	}

	want := []string{"line-039997", "line-039998", "line-039999"}
	if !slices.Equal(got, want) {
		t.Errorf("tailLines() = %q, want %q", got, want)
	}
}

func TestStore_Lines_tailMatchesFullRead(t *testing.T) {
	t.Parallel()

	store := New(t.TempDir(), 1<<20)
	at := time.Now().UTC()

	attempt, err := store.Create("proc_1", 1, at, 0)
	if err != nil {
		t.Fatalf("Create() returned %v, want nil", err)
	}

	for i := range 5000 {
		if _, err := fmt.Fprintf(attempt.Stdout, "out-%04d\n", i); err != nil {
			t.Fatalf("writing stdout: %v", err)
		}
	}

	for i := range 10 {
		if _, err := fmt.Fprintf(attempt.Stderr, "err-%04d\n", i); err != nil {
			t.Fatalf("writing stderr: %v", err)
		}
	}

	if err := attempt.Close(); err != nil {
		t.Fatalf("Close() returned %v, want nil", err)
	}

	tests := []struct {
		name   string
		stream Stream
		tail   int
	}{
		{name: "stdout", stream: StreamStdout, tail: 7},
		{name: "stderr", stream: StreamStderr, tail: 3},
		{name: "both", stream: StreamBoth, tail: 12},
		{name: "tail larger than the log", stream: StreamStderr, tail: 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			full, err := store.Lines("proc_1", 1, tt.stream, at, 0)
			if err != nil {
				t.Fatalf("Lines() returned %v, want nil", err)
			}

			tailed, err := store.Lines("proc_1", 1, tt.stream, at, tt.tail)
			if err != nil {
				t.Fatalf("Lines() returned %v, want nil", err)
			}

			want := full
			if len(want) > tt.tail {
				want = want[len(want)-tt.tail:]
			}

			// The backwards walk must return exactly what trimming a full read
			// would have returned.
			if !slices.Equal(tailed, want) {
				t.Errorf("tail read = %q, want %q", tailed, want)
			}
		})
	}
}
