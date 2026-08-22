// Package logstore captures the output of each attempt on disk.
//
// Output is stored per (process, attempt, stream). A file shared by a worker
// would interleave the output of every concurrent execution and make
// per-execution log retrieval impossible (docs/SPEC.md §10).
package logstore

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

const (
	// maxLineBytes bounds one log line when reading, so a process that writes a
	// single huge line cannot exhaust the daemon's memory.
	maxLineBytes = 1 << 20

	// tailChunkBytes is how much of the file a backwards walk reads at a time.
	tailChunkBytes = 64 << 10

	// maxTailBytes stops the backwards walk on a file without newlines, so one
	// enormous line cannot pull the whole log into memory.
	maxTailBytes = 8 << 20
)

// newline is the separator the backwards walk counts.
var newline = []byte{'\n'}

const (
	dirPerm  os.FileMode = 0o750
	filePerm os.FileMode = 0o640
)

// Policy bounds what one attempt may store.
//
// MaxFiles is how many rotated files are kept behind the live one. Zero keeps
// none and caps the stream instead: what follows the limit is dropped. That
// suits a task, whose attempt is short by definition, and is unusable for a
// service, whose single attempt may run for months and would otherwise go
// silent forever the first time it filled its cap.
type Policy struct {
	MaxBytesPerStream int64
	MaxFiles          int
}

// Store writes and reads attempt logs under a root directory.
type Store struct {
	root     string
	maxBytes int64

	// mu guards open, which names the files the attempts under supervision are
	// writing to right now.
	//
	// Retention walks the directory by age, and an attempt is not necessarily
	// younger than the window: a service that logs at start-up and then goes
	// quiet outlives it. Unlinking the file underneath the writer costs every
	// line it produces from then on — the descriptor stays valid, so the process
	// notices nothing and the output lands in an inode nobody can open again.
	mu   sync.Mutex
	open map[string]int
}

// New returns a store rooted at dir, capping each stream at maxBytes.
func New(dir string, maxBytes int64) *Store {
	return &Store{root: dir, maxBytes: maxBytes, open: map[string]int{}}
}

// Path returns the file backing one stream of one attempt. The live output of a
// rotating stream always stays here: rotation moves the older content aside, so
// a follower has one path to poll rather than a moving target.
func (s *Store) Path(processID string, attempt int, stream Stream, at time.Time) string {
	return filepath.Join(s.root, filepath.FromSlash(relPath(processID, attempt, stream, at)))
}

// relPath is the same file, named as the retention walk sees it: relative to
// the log root and slash-separated, which is what io/fs hands a WalkDir func
// whatever the host separator is.
func relPath(processID string, attempt int, stream Stream, at time.Time) string {
	day := at.UTC()

	return fmt.Sprintf("%s/%s/%s.%d.%s.log", day.Format("2006"), day.Format("01"), processID, attempt, stream)
}

// markOpen records that an attempt is writing to a file, so retention leaves it
// alone until it is closed.
func (s *Store) markOpen(rel string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.open[rel]++
}

// markClosed releases the protection markOpen took.
func (s *Store) markClosed(rel string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.open[rel] <= 1 {
		delete(s.open, rel)
		return
	}

	s.open[rel]--
}

// isOpen reports whether an attempt is writing to the file right now.
func (s *Store) isOpen(rel string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.open[rel] > 0
}

// RotatedPath returns the file holding the generation-th rotation of a stream.
// Generation 1 is the content moved aside most recently.
func (s *Store) RotatedPath(processID string, attempt int, stream Stream, at time.Time, generation int) string {
	return rotatedPath(s.Path(processID, attempt, stream, at), generation)
}

func rotatedPath(path string, generation int) string {
	return fmt.Sprintf("%s.%d", path, generation)
}

// Attempt holds the open writers of one attempt.
type Attempt struct {
	Stdout *StreamWriter
	Stderr *StreamWriter

	// store and files are what Close needs to hand the attempt's files back to
	// retention. An Attempt built without a store closes just the same.
	store *Store
	files []string
}

// Create opens the log files of an attempt. A MaxBytesPerStream of zero uses
// the store-wide cap; a worker may lower or raise it for its own executions.
func (s *Store) Create(processID string, attempt int, at time.Time, policy Policy) (*Attempt, error) {
	if policy.MaxBytesPerStream <= 0 {
		policy.MaxBytesPerStream = s.maxBytes
	}

	stdout, err := s.openStream(processID, attempt, StreamStdout, at, policy)
	if err != nil {
		return nil, err
	}

	stderr, err := s.openStream(processID, attempt, StreamStderr, at, policy)
	if err != nil {
		_ = stdout.Close()

		s.markClosed(relPath(processID, attempt, StreamStdout, at))

		return nil, err
	}

	return &Attempt{
		Stdout: stdout,
		Stderr: stderr,
		store:  s,
		files: []string{
			relPath(processID, attempt, StreamStdout, at),
			relPath(processID, attempt, StreamStderr, at),
		},
	}, nil
}

func (s *Store) openStream(
	processID string,
	attempt int,
	stream Stream,
	at time.Time,
	policy Policy,
) (*StreamWriter, error) {
	path := s.Path(processID, attempt, stream, at)

	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, fmt.Errorf("creating log dir for %s: %w", processID, err)
	}

	// Claimed before the file exists, so a retention pass running in between
	// cannot delete what is about to be written to.
	s.markOpen(relPath(processID, attempt, stream, at))

	file, err := createLogFile(path)
	if err != nil {
		s.markClosed(relPath(processID, attempt, stream, at))
		return nil, err
	}

	writer := &StreamWriter{w: file, limit: policy.MaxBytesPerStream, closer: file}

	if policy.MaxFiles > 0 {
		rotating := &rotator{path: path, maxFiles: policy.MaxFiles, current: file}
		writer.rotate = rotating.next
		writer.closer = rotating
	}

	return writer, nil
}

func createLogFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, filePerm) //nolint:gosec // the path is derived from the configured log dir
	if err != nil {
		return nil, fmt.Errorf("creating log file %q: %w", path, err)
	}

	return file, nil
}

// Truncated reports whether either stream lost output, by hitting the size cap
// or by rotating past the files it is allowed to keep.
func (a *Attempt) Truncated() bool {
	return a.Stdout.Truncated() || a.Stderr.Truncated()
}

// Close flushes and closes the underlying files, and hands them back to
// retention.
func (a *Attempt) Close() error {
	if a.store != nil {
		defer func() {
			for _, file := range a.files {
				a.store.markClosed(file)
			}

			a.files = nil
		}()
	}

	err := a.Stdout.Close()

	if stderrErr := a.Stderr.Close(); err == nil {
		err = stderrErr
	}

	return err
}

// File is an open log file. It adds the size lookup the backwards tail walk
// needs to the usual reader interface.
type File struct {
	*os.File
}

// Size reports how many bytes the log holds.
func (f File) Size() (int64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("sizing log file: %w", err)
	}

	return info.Size(), nil
}

// Open returns a reader over the live file of one stream of one attempt.
func (s *Store) Open(processID string, attempt int, stream Stream, at time.Time) (File, error) {
	return openFile(s.Path(processID, attempt, stream, at))
}

func openFile(path string) (File, error) {
	file, err := os.Open(path) //nolint:gosec // the path is derived from the configured log dir
	if err != nil {
		return File{}, fmt.Errorf("opening log file %q: %w", path, err)
	}

	return File{File: file}, nil
}

// StreamWriter bounds what one stream of one attempt may store.
//
// A process that logs in a loop would otherwise fill the disk, which is a
// trivial denial of service against every other worker on the node. Two bounded
// behaviours are available at the limit: dropping the rest, and rotating.
type StreamWriter struct {
	w       io.Writer
	limit   int64
	written int64
	stored  int64
	capped  bool
	dropped bool

	// rotate installs the next file once the current one is full, reporting
	// whether an older one had to be discarded to make room. A nil rotate caps
	// the stream instead.
	rotate func() (io.Writer, bool, error)
	closer io.Closer
}

// Write implements io.Writer. It never reports short writes to the process: the
// child must not receive an I/O error because the daemon stopped storing.
func (c *StreamWriter) Write(p []byte) (int, error) {
	total := len(p)

	for len(p) > 0 {
		if c.capped {
			return total, nil
		}

		remaining := c.limit - c.written
		if remaining <= 0 {
			if err := c.roll(); err != nil {
				return total, err
			}

			continue
		}

		size := min(int64(len(p)), remaining)

		if _, err := c.w.Write(p[:size]); err != nil {
			return 0, err
		}

		c.written += size
		c.stored += size
		p = p[size:]
	}

	return total, nil
}

// roll makes room for more output, either by moving the full file aside or, for
// a stream that does not rotate, by giving up on the rest.
func (c *StreamWriter) roll() error {
	// A non-positive limit can never be rolled out of, so it must cap: rotating
	// a file that is full the moment it is opened would loop forever.
	if c.rotate == nil || c.limit <= 0 {
		return c.markCapped()
	}

	next, dropped, err := c.rotate()
	if err != nil {
		return err
	}

	c.w = next
	c.written = 0
	c.dropped = c.dropped || dropped

	return nil
}

func (c *StreamWriter) markCapped() error {
	c.capped = true

	if _, err := fmt.Fprintf(c.w, "\n[processd] output truncated at %d bytes\n", c.limit); err != nil {
		return fmt.Errorf("writing truncation marker: %w", err)
	}

	return nil
}

// Truncated reports whether output was lost, either at the cap or by rotating
// past the files the stream is allowed to keep.
func (c *StreamWriter) Truncated() bool { return c.capped || c.dropped }

// Written returns how many bytes of process output were stored.
func (c *StreamWriter) Written() int64 { return c.stored }

// Close releases the file the stream is writing to.
func (c *StreamWriter) Close() error {
	if c.closer == nil {
		return nil
	}

	if err := c.closer.Close(); err != nil {
		return fmt.Errorf("closing log file: %w", err)
	}

	return nil
}

// rotator moves the live file aside once it is full and opens a fresh one in
// its place, keeping at most maxFiles generations behind it.
type rotator struct {
	path     string
	maxFiles int
	current  *os.File
}

// next retires the current file and returns the one that replaces it, reporting
// whether the oldest generation had to be discarded to make room.
func (r *rotator) next() (io.Writer, bool, error) {
	if err := r.current.Close(); err != nil {
		return nil, false, fmt.Errorf("closing log file %q: %w", r.path, err)
	}

	dropped, err := r.shift()
	if err != nil {
		return nil, dropped, err
	}

	file, err := createLogFile(r.path)
	if err != nil {
		return nil, dropped, err
	}

	r.current = file

	return file, dropped, nil
}

// shift renames each kept generation one step older, discards what falls past
// maxFiles and moves the live file into generation 1.
func (r *rotator) shift() (bool, error) {
	dropped := false

	err := os.Remove(rotatedPath(r.path, r.maxFiles))

	switch {
	case err == nil:
		dropped = true
	case !errors.Is(err, os.ErrNotExist):
		return false, fmt.Errorf("discarding oldest log of %q: %w", r.path, err)
	}

	for generation := r.maxFiles - 1; generation >= 1; generation-- {
		from := rotatedPath(r.path, generation)
		to := rotatedPath(r.path, generation+1)

		if err := os.Rename(from, to); err != nil && !errors.Is(err, os.ErrNotExist) {
			return dropped, fmt.Errorf("rotating log %q: %w", from, err)
		}
	}

	if err := os.Rename(r.path, rotatedPath(r.path, 1)); err != nil {
		return dropped, fmt.Errorf("rotating log %q: %w", r.path, err)
	}

	return dropped, nil
}

// Close releases whichever file the rotator is currently writing to.
func (r *rotator) Close() error { return r.current.Close() }

// Lines reads the captured output of one attempt.
//
// A tail of zero returns everything stored, oldest rotation first. Reading
// "both" interleaves the two files in file order and marks the origin of each
// line, which is the closest approximation available: the streams are captured
// separately and lines carry no timestamps.
func (s *Store) Lines(processID string, attempt int, stream Stream, at time.Time, tail int) ([]string, error) {
	streams := []Stream{stream}
	labelled := false

	if stream == StreamBoth {
		streams = []Stream{StreamStdout, StreamStderr}
		labelled = true
	}

	lines := []string{}

	for _, current := range streams {
		read, err := s.readLines(processID, attempt, current, at, tail)
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

// readLines reads one stream across its rotations, oldest first, stopping as
// soon as a bounded tail has been satisfied.
func (s *Store) readLines(processID string, attempt int, stream Stream, at time.Time, tail int) ([]string, error) {
	paths := s.streamFiles(processID, attempt, stream, at)
	lines := []string{}

	// A bounded tail walks the generations from the newest backwards and stops
	// as soon as it has enough, so rotations older than the request are never
	// opened at all.
	for _, path := range slices.Backward(paths) {
		read, err := readFileLines(path, tail)
		if err != nil {
			return nil, fmt.Errorf("reading log of %s attempt %d: %w", processID, attempt, err)
		}

		lines = append(read, lines...)

		if tail > 0 && len(lines) >= tail {
			return lines[len(lines)-tail:], nil
		}
	}

	return lines, nil
}

// streamFiles returns the files holding one stream, oldest rotation first and
// the live file last.
func (s *Store) streamFiles(processID string, attempt int, stream Stream, at time.Time) []string {
	live := s.Path(processID, attempt, stream, at)

	rotations := []string{}

	for generation := 1; ; generation++ {
		path := rotatedPath(live, generation)
		if _, err := os.Stat(path); err != nil {
			break
		}

		// Generation 1 is the newest, so prepending keeps the walk chronological.
		rotations = append([]string{path}, rotations...)
	}

	return append(rotations, live)
}

func readFileLines(path string, tail int) ([]string, error) {
	file, err := openFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	defer func() {
		_ = file.Close()
	}()

	if tail > 0 {
		return tailLines(file, tail)
	}

	lines := []string{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return lines, nil
}

// tailLines returns the last n lines of a file by walking it backwards.
//
// Reading the whole file to keep its last hundred lines costs as much memory as
// the cap allows — with a 32MiB stream that is 32MiB per request, per stream.
// Walking back from the end keeps the cost proportional to what is returned.
func tailLines(file readerAt, n int) ([]string, error) {
	size, err := file.Size()
	if err != nil {
		return nil, err
	}

	var (
		buffer   []byte
		position = size
	)

	for position > 0 && bytes.Count(buffer, newline) <= n && int64(len(buffer)) < maxTailBytes {
		length := min(int64(tailChunkBytes), position)
		position -= length

		block := make([]byte, length)
		if _, err := file.ReadAt(block, position); err != nil {
			return nil, err
		}

		buffer = append(block, buffer...)
	}

	if len(buffer) == 0 {
		return []string{}, nil
	}

	lines := strings.Split(strings.TrimRight(string(buffer), "\n"), "\n")

	// The first line of the window is only whole when the walk reached the start
	// of the file.
	if position > 0 && len(lines) > 0 {
		lines = lines[1:]
	}

	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	return lines, nil
}

// readerAt is the part of *os.File that tailLines needs, kept narrow so the
// backwards walk can be tested without touching the filesystem.
type readerAt interface {
	ReadAt(p []byte, off int64) (int, error)
	Size() (int64, error)
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

		// An attempt that is still writing keeps its file, however old the last
		// line in it is. Age is the wrong question for a service that has been
		// up for a month.
		if s.isOpen(path) {
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
