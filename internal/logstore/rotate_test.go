package logstore

import (
	"os"
	"slices"
	"testing"
	"time"
)

// rotatingAttempt opens one attempt whose streams rotate, with a cap small
// enough that a handful of lines fills a generation.
func rotatingAttempt(t *testing.T, store *Store, at time.Time, limit int64, maxFiles int) *Attempt {
	t.Helper()

	attempt, err := store.Create("proc_01K", 1, at, Policy{
		MaxBytesPerStream: limit,
		MaxFiles:          maxFiles,
	})
	if err != nil {
		t.Fatalf("Create() returned %v, want nil", err)
	}

	t.Cleanup(func() {
		_ = attempt.Close()
	})

	return attempt
}

func write(t *testing.T, w *StreamWriter, text string) {
	t.Helper()

	if _, err := w.Write([]byte(text)); err != nil {
		t.Fatalf("Write(%q) returned %v, want nil", text, err)
	}
}

func TestStreamWriter_Rotate(t *testing.T) {
	t.Parallel()

	t.Run("keeps writing past the cap instead of going silent", func(t *testing.T) {
		t.Parallel()

		store := New(t.TempDir(), 1024)
		at := time.Now().UTC()

		// "aaaa\n" is 5 bytes, so every second line fills the 8-byte generation.
		attempt := rotatingAttempt(t, store, at, 8, 3)

		for range 5 {
			write(t, attempt.Stdout, "aaaa\n")
		}

		if attempt.Stdout.Truncated() {
			t.Error("Truncated() = true, want false while rotations stay within max_files")
		}

		if got := attempt.Stdout.Written(); got != 25 {
			t.Errorf("Written() = %d, want 25", got)
		}
	})

	t.Run("reports truncation once a generation is discarded", func(t *testing.T) {
		t.Parallel()

		store := New(t.TempDir(), 1024)
		at := time.Now().UTC()

		attempt := rotatingAttempt(t, store, at, 8, 1)

		// One kept generation plus the live file holds two rotations' worth; the
		// third rotation has to discard the oldest.
		for range 8 {
			write(t, attempt.Stdout, "aaaa\n")
		}

		if !attempt.Stdout.Truncated() {
			t.Error("Truncated() = false, want true once a generation was discarded")
		}
	})

	t.Run("never keeps more than max_files generations", func(t *testing.T) {
		t.Parallel()

		store := New(t.TempDir(), 1024)
		at := time.Now().UTC()

		attempt := rotatingAttempt(t, store, at, 8, 2)

		for range 20 {
			write(t, attempt.Stdout, "aaaa\n")
		}

		if _, err := os.Stat(store.RotatedPath("proc_01K", 1, StreamStdout, at, 2)); err != nil {
			t.Errorf("generation 2 missing: %v", err)
		}

		if _, err := os.Stat(store.RotatedPath("proc_01K", 1, StreamStdout, at, 3)); !os.IsNotExist(err) {
			t.Errorf("generation 3 exists, want it discarded past max_files")
		}
	})

	t.Run("a capped stream still drops what follows the limit", func(t *testing.T) {
		t.Parallel()

		store := New(t.TempDir(), 1024)
		at := time.Now().UTC()

		attempt := rotatingAttempt(t, store, at, 8, 0)

		for range 5 {
			write(t, attempt.Stdout, "aaaa\n")
		}

		if !attempt.Stdout.Truncated() {
			t.Error("Truncated() = false, want true once a stream without rotation filled its cap")
		}
	})
}

func TestStore_LinesAcrossRotations(t *testing.T) {
	t.Parallel()

	// Each line is 3 bytes, so a 6-byte generation holds exactly two of them.
	lines := []string{"l1", "l2", "l3", "l4", "l5", "l6"}

	newStore := func(t *testing.T) (*Store, time.Time) {
		t.Helper()

		store := New(t.TempDir(), 1024)
		at := time.Now().UTC()
		attempt := rotatingAttempt(t, store, at, 6, 5)

		for _, line := range lines {
			write(t, attempt.Stdout, line+"\n")
		}

		return store, at
	}

	t.Run("reads every generation oldest first", func(t *testing.T) {
		t.Parallel()

		store, at := newStore(t)

		got, err := store.Lines("proc_01K", 1, StreamStdout, at, 0)
		if err != nil {
			t.Fatalf("Lines() returned %v, want nil", err)
		}

		if !slices.Equal(got, lines) {
			t.Errorf("Lines() = %v, want %v", got, lines)
		}
	})

	t.Run("a tail spans the generation boundary", func(t *testing.T) {
		t.Parallel()

		store, at := newStore(t)

		got, err := store.Lines("proc_01K", 1, StreamStdout, at, 3)
		if err != nil {
			t.Fatalf("Lines() returned %v, want nil", err)
		}

		want := []string{"l4", "l5", "l6"}
		if !slices.Equal(got, want) {
			t.Errorf("Lines() = %v, want %v", got, want)
		}
	})

	t.Run("a tail longer than the history returns everything", func(t *testing.T) {
		t.Parallel()

		store, at := newStore(t)

		got, err := store.Lines("proc_01K", 1, StreamStdout, at, 100)
		if err != nil {
			t.Fatalf("Lines() returned %v, want nil", err)
		}

		if !slices.Equal(got, lines) {
			t.Errorf("Lines() = %v, want %v", got, lines)
		}
	})
}

func TestTailer_SurvivesRotation(t *testing.T) {
	t.Parallel()

	store := New(t.TempDir(), 1024)
	at := time.Now().UTC()
	attempt := rotatingAttempt(t, store, at, 6, 3)

	write(t, attempt.Stdout, "l1\n")

	tailer, err := store.Tail("proc_01K", 1, StreamStdout, at, 0)
	if err != nil {
		t.Fatalf("Tail() returned %v, want nil", err)
	}

	first, err := tailer.Read()
	if err != nil {
		t.Fatalf("Read() returned %v, want nil", err)
	}

	if len(first) != 1 || first[0].Text != "l1" {
		t.Fatalf("Read() = %v, want the first line", first)
	}

	// Fill the generation and roll into a fresh file under the follower.
	write(t, attempt.Stdout, "l2\n")
	write(t, attempt.Stdout, "l3\n")

	after, err := tailer.Read()
	if err != nil {
		t.Fatalf("Read() returned %v, want nil", err)
	}

	texts := make([]string, 0, len(after))
	for _, line := range after {
		texts = append(texts, line.Text)
	}

	// l2 filled the generation that was moved aside; the follower resumes on the
	// live file rather than stalling at an offset the new file never reaches.
	if !slices.Contains(texts, "l3") {
		t.Errorf("Read() = %v, want the output written after the rotation", texts)
	}
}
