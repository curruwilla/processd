// Package logstore captures the output of each attempt on disk.
//
// Output is stored per (process, attempt, stream). A file shared by a worker
// would interleave the output of every concurrent execution and make
// per-execution log retrieval impossible (docs/SPEC.md §10).
package logstore

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	// StreamBoth is only valid when reading: it merges the two files.
	StreamBoth Stream = "both"
)

// maxLineBytes bounds one log line when reading, so a process that writes a
// single huge line cannot exhaust the daemon's memory.
const maxLineBytes = 1 << 20

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

// Create opens the log files of an attempt. A maxBytes of zero uses the
// store-wide cap; a worker may lower or raise it for its own executions.
func (s *Store) Create(processID string, attempt int, at time.Time, maxBytes int64) (*Attempt, error) {
	limit := maxBytes
	if limit <= 0 {
		limit = s.maxBytes
	}

	stdout, stdoutFile, err := s.openStream(processID, attempt, StreamStdout, at, limit)
	if err != nil {
		return nil, err
	}

	stderr, stderrFile, err := s.openStream(processID, attempt, StreamStderr, at, limit)
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

func (s *Store) openStream(
	processID string,
	attempt int,
	stream Stream,
	at time.Time,
	limit int64,
) (*CappedWriter, *os.File, error) {
	path := s.Path(processID, attempt, stream, at)

	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, nil, fmt.Errorf("creating log dir for %s: %w", processID, err)
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, filePerm) //nolint:gosec // the path is derived from the configured log dir
	if err != nil {
		return nil, nil, fmt.Errorf("creating log file %q: %w", path, err)
	}

	return &CappedWriter{w: file, limit: limit}, file, nil
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

// Lines reads the captured output of one attempt.
//
// A tail of zero returns everything stored. Reading "both" interleaves the two
// files in file order and marks the origin of each line, which is the closest
// approximation available: the streams are captured separately and lines carry
// no timestamps.
func (s *Store) Lines(processID string, attempt int, stream Stream, at time.Time, tail int) ([]string, error) {
	streams := []Stream{stream}
	labelled := false

	if stream == StreamBoth {
		streams = []Stream{StreamStdout, StreamStderr}
		labelled = true
	}

	lines := []string{}

	for _, current := range streams {
		read, err := s.readLines(processID, attempt, current, at)
		if err != nil {
			return nil, err
		}

		for _, line := range read {
			if labelled {
				line = string(current) + ": " + line
			}

			lines = append(lines, line)
		}
	}

	if tail > 0 && len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}

	return lines, nil
}

func (s *Store) readLines(processID string, attempt int, stream Stream, at time.Time) ([]string, error) {
	file, err := s.Open(processID, attempt, stream, at)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	defer func() {
		_ = file.Close()
	}()

	lines := []string{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading log of %s attempt %d: %w", processID, attempt, err)
	}

	return lines, nil
}

// Purge removes log files last written before the given instant, together with
// the directories left empty behind them.
func (s *Store) Purge(before time.Time) (int, error) {
	removed := 0

	// Walking and deleting through an os.Root keeps every path inside the log
	// directory, even if something plants a symlink between the two steps.
	root, err := os.OpenRoot(s.root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}

	if err != nil {
		return 0, fmt.Errorf("opening log dir: %w", err)
	}

	defer func() {
		_ = root.Close()
	}()

	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}

			return err
		}

		if entry.IsDir() {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}

		if info.ModTime().After(before) {
			return nil
		}

		if err := root.Remove(path); err != nil {
			return err
		}

		removed++

		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return removed, fmt.Errorf("purging logs: %w", err)
	}

	s.removeEmptyDirs()

	return removed, nil
}

// removeEmptyDirs prunes the year/month directories a purge emptied. Failures
// are not worth reporting: an empty directory costs nothing.
func (s *Store) removeEmptyDirs() {
	months, _ := filepath.Glob(filepath.Join(s.root, "*", "*"))
	for _, dir := range months {
		_ = os.Remove(dir)
	}

	years, _ := filepath.Glob(filepath.Join(s.root, "*"))
	for _, dir := range years {
		_ = os.Remove(dir)
	}
}
