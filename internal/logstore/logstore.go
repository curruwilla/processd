// Package logstore captures the output of each attempt on disk.
//
// Output is stored per (process, attempt, stream). A file shared by a worker
// would interleave the output of every concurrent execution and make
// per-execution log retrieval impossible (docs/SPEC.md §10).
package logstore

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Stream names one of the two captured output streams.
type Stream string

// The captured output streams.
const (
	StreamStdout Stream = "stdout"
	StreamStderr Stream = "stderr"
)

const (
	dirPerm  os.FileMode = 0o750
	filePerm os.FileMode = 0o640
)

// Store writes and reads attempt logs under a root directory.
type Store struct {
	root     string
	maxBytes int64
}

// New returns a store rooted at dir, capping each stream at maxBytes.
func New(dir string, maxBytes int64) *Store {
	return &Store{root: dir, maxBytes: maxBytes}
}

// Path returns the file backing one stream of one attempt.
func (s *Store) Path(processID string, attempt int, stream Stream, at time.Time) string {
	day := at.UTC()
	name := fmt.Sprintf("%s.%d.%s.log", processID, attempt, stream)

	return filepath.Join(s.root, day.Format("2006"), day.Format("01"), name)
}

// Attempt holds the open writers of one attempt.
type Attempt struct {
	Stdout *CappedWriter
	Stderr *CappedWriter

	files []*os.File
}

// Create opens the log files of an attempt.
func (s *Store) Create(processID string, attempt int, at time.Time) (*Attempt, error) {
	stdout, stdoutFile, err := s.openStream(processID, attempt, StreamStdout, at)
	if err != nil {
		return nil, err
	}

	stderr, stderrFile, err := s.openStream(processID, attempt, StreamStderr, at)
	if err != nil {
		_ = stdoutFile.Close()
		return nil, err
	}

	return &Attempt{
		Stdout: stdout,
		Stderr: stderr,
		files:  []*os.File{stdoutFile, stderrFile},
	}, nil
}

func (s *Store) openStream(processID string, attempt int, stream Stream, at time.Time) (*CappedWriter, *os.File, error) {
	path := s.Path(processID, attempt, stream, at)

	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, nil, fmt.Errorf("creating log dir for %s: %w", processID, err)
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, filePerm) //nolint:gosec // the path is derived from the configured log dir
	if err != nil {
		return nil, nil, fmt.Errorf("creating log file %q: %w", path, err)
	}

	return &CappedWriter{w: file, limit: s.maxBytes}, file, nil
}

// Truncated reports whether either stream hit the size cap.
func (a *Attempt) Truncated() bool {
	return a.Stdout.Truncated() || a.Stderr.Truncated()
}

// Close flushes and closes the underlying files.
func (a *Attempt) Close() error {
	for _, file := range a.files {
		if err := file.Close(); err != nil {
			return fmt.Errorf("closing log file: %w", err)
		}
	}

	return nil
}

// Open returns a reader over one stream of one attempt.
func (s *Store) Open(processID string, attempt int, stream Stream, at time.Time) (io.ReadCloser, error) {
	path := s.Path(processID, attempt, stream, at)

	file, err := os.Open(path) //nolint:gosec // the path is derived from the configured log dir
	if err != nil {
		return nil, fmt.Errorf("opening log file %q: %w", path, err)
	}

	return file, nil
}

// CappedWriter writes until a byte limit is reached, then drops the rest.
//
// A process that logs in a loop would otherwise fill the disk, which is a
// trivial denial of service against every other worker on the node.
type CappedWriter struct {
	w       io.Writer
	limit   int64
	written int64
	capped  bool
}

// Write implements io.Writer. It never reports short writes to the process:
// the child must not receive an I/O error because the daemon stopped storing.
func (c *CappedWriter) Write(p []byte) (int, error) {
	if c.capped {
		return len(p), nil
	}

	remaining := c.limit - c.written
	if remaining <= 0 {
		return len(p), c.markCapped()
	}

	if int64(len(p)) <= remaining {
		n, err := c.w.Write(p)
		c.written += int64(n)

		return n, err
	}

	if _, err := c.w.Write(p[:remaining]); err != nil {
		return 0, err
	}

	c.written = c.limit

	return len(p), c.markCapped()
}

func (c *CappedWriter) markCapped() error {
	c.capped = true

	if _, err := fmt.Fprintf(c.w, "\n[processd] output truncated at %d bytes\n", c.limit); err != nil {
		return fmt.Errorf("writing truncation marker: %w", err)
	}

	return nil
}

// Truncated reports whether the cap was reached.
func (c *CappedWriter) Truncated() bool { return c.capped }

// Written returns how many bytes of process output were stored.
func (c *CappedWriter) Written() int64 { return c.written }
