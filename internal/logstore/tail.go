package logstore

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"
)

// maxPollBytes bounds how much one poll of a growing log may read, so a process
// writing faster than a client reads cannot pull the whole file into memory at
// once. What is left over is returned by the next poll.
const maxPollBytes = 1 << 20

// Line is one captured output line and the stream it came from.
type Line struct {
	Stream Stream
	Text   string
}

// Tailer follows the log files of one attempt as they grow.
//
// A partial trailing line is held back until its newline arrives: output is
// captured as the child writes it, so a poll landing mid-line is the normal
// case, not the exception.
type Tailer struct {
	sources []*tailSource
}

// Tail follows one attempt, returning a tailer whose first read starts at the
// requested backlog. A backlog of zero starts at the beginning of the file; a
// negative one skips everything already written and reports only new output.
//
// Reading "both" follows the two files independently, so a line is labelled
// with its own stream instead of being guessed from interleaving.
func (s *Store) Tail(processID string, attempt int, stream Stream, at time.Time, backlog int) (*Tailer, error) {
	streams := []Stream{stream}
	if stream == StreamBoth {
		streams = []Stream{StreamStdout, StreamStderr}
	}

	tailer := &Tailer{sources: make([]*tailSource, 0, len(streams))}

	for _, current := range streams {
		source := &tailSource{
			path:   s.Path(processID, attempt, current, at),
			stream: current,
		}

		offset, err := startOffset(source.path, backlog)
		if err != nil {
			return nil, err
		}

		source.offset = offset

		tailer.sources = append(tailer.sources, source)
	}

	return tailer, nil
}

// Read returns the lines appended since the previous call, stdout first. It
// never blocks: an empty result means the files have not grown yet.
func (t *Tailer) Read() ([]Line, error) {
	lines := []Line{}

	for _, source := range t.sources {
		read, err := source.read()
		if err != nil {
			return nil, err
		}

		lines = append(lines, read...)
	}

	return lines, nil
}

// tailSource follows one file. The file is reopened on every poll rather than
// held open: an execution may not have created it yet, and a handle kept for
// the whole life of a stream would pin a deleted file after a log purge.
type tailSource struct {
	path    string
	stream  Stream
	offset  int64
	partial []byte

	// info identifies the file the previous poll read, so that a rotation is
	// recognised even when the replacement happens to be the same size.
	info os.FileInfo
}

func (t *tailSource) read() ([]Line, error) {
	file, err := os.Open(t.path)
	if errors.Is(err, os.ErrNotExist) {
		// The attempt has not written anything yet.
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("opening log file %q: %w", t.path, err)
	}

	defer func() {
		_ = file.Close()
	}()

	if err := t.rewindIfRotated(file); err != nil {
		return nil, err
	}

	if _, err := file.Seek(t.offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seeking log file %q: %w", t.path, err)
	}

	chunk, err := io.ReadAll(io.LimitReader(file, maxPollBytes))
	if err != nil {
		return nil, fmt.Errorf("reading log file %q: %w", t.path, err)
	}

	t.offset += int64(len(chunk))

	return t.split(chunk), nil
}

// rewindIfRotated restarts the follower at the top of the file when the stream
// has rotated underneath it.
//
// The live file always keeps the same path, so rotation replaces the file the
// follower is reading rather than moving it. Comparing identity catches that
// even when the replacement happens to be the same size, which a size
// comparison alone cannot; the size check stays as the case for a file
// truncated in place.
//
// Whatever the follower had not read yet moved into the previous generation and
// is not chased: a follower is a view of what the process is writing now, and
// reaching backwards for it would race the next rotation anyway. Lines(…) reads
// the full history, rotations included.
func (t *tailSource) rewindIfRotated(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("sizing log file %q: %w", t.path, err)
	}

	replaced := t.info != nil && !os.SameFile(t.info, info)
	t.info = info

	if !replaced && info.Size() >= t.offset {
		return nil
	}

	t.offset = 0
	t.partial = nil

	return nil
}

// split turns the bytes read so far into whole lines, keeping the remainder for
// the next poll.
func (t *tailSource) split(chunk []byte) []Line {
	data := slices.Concat(t.partial, chunk)
	lines := []Line{}

	for {
		end := bytes.IndexByte(data, '\n')
		if end < 0 {
			break
		}

		lines = append(lines, Line{Stream: t.stream, Text: string(data[:end])})
		data = data[end+1:]
	}

	// A process may write a line longer than the reader is willing to buffer.
	// Emitting it unfinished bounds the memory a single line can cost.
	if len(data) >= maxLineBytes {
		lines = append(lines, Line{Stream: t.stream, Text: string(data)})
		data = nil
	}

	t.partial = slices.Clone(data)

	return lines
}

// startOffset resolves where a follower begins reading: the start of the file,
// its end, or the start of its last backlog lines.
func startOffset(path string, backlog int) (int64, error) {
	if backlog == 0 {
		return 0, nil
	}

	file, err := os.Open(path) //nolint:gosec // the path is derived from the configured log dir
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}

	if err != nil {
		return 0, fmt.Errorf("opening log file %q: %w", path, err)
	}

	defer func() {
		_ = file.Close()
	}()

	return tailStart(File{File: file}, backlog)
}

// tailStart returns the offset of the first of the last n lines of a file,
// walking backwards so the cost stays proportional to what is skipped to,
// not to the size of the log.
func tailStart(file readerAt, n int) (int64, error) {
	size, err := file.Size()
	if err != nil {
		return 0, err
	}

	if n < 0 {
		return size, nil
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
			return 0, err
		}

		buffer = append(block, buffer...)
	}

	// The newline ending the last line separates nothing: counting it would
	// return one line too few.
	trimmed := bytes.TrimSuffix(buffer, newline)

	start := len(trimmed)

	for range n {
		previous := bytes.LastIndex(trimmed[:start], newline)
		if previous < 0 {
			return incompleteStart(trimmed, position, size)
		}

		start = previous
	}

	return position + int64(start) + 1, nil
}

// incompleteStart handles a window holding fewer than n lines: from the start
// of the file everything is returned, and from a partial window the first whole
// line is, since the line before it was cut in half by the walk's own bound.
func incompleteStart(window []byte, position, size int64) (int64, error) {
	if position == 0 {
		return 0, nil
	}

	first := bytes.IndexByte(window, '\n')
	if first < 0 {
		return size, nil
	}

	return position + int64(first) + 1, nil
}
