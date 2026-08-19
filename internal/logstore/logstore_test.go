package logstore

import (
	"bytes"
	"path/filepath"
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

	attempt, err := store.Create("proc_01K", 1, at)
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
