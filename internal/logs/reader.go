package logs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
)

// DefaultChunkSize bounds a single incremental read during streaming.
const DefaultChunkSize = 64 << 10

// ReadFrom returns up to limit bytes of a log file starting at offset, along
// with the offset to resume from. A missing file yields no data rather than an
// error: a job that has not produced output yet has no log file, and callers
// should treat that as "nothing to show".
func ReadFrom(path string, offset, limit int64) (data []byte, next int64, err error) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = DefaultChunkSize
	}
	f, err := os.Open(path) //nolint:gosec // path comes from the logs table
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, offset, nil
		}
		return nil, offset, fmt.Errorf("logs: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return nil, offset, fmt.Errorf("logs: stat %s: %w", path, err)
	}
	size := st.Size()
	if offset >= size {
		// Nothing new; also covers a file that was truncated behind us.
		if offset > size {
			offset = size
		}
		return nil, offset, nil
	}
	if remaining := size - offset; limit > remaining {
		limit = remaining
	}

	buf := make([]byte, limit)
	n, err := f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, offset, fmt.Errorf("logs: read %s: %w", path, err)
	}
	return buf[:n], offset + int64(n), nil
}

// ReadAll returns a whole log file. A missing file yields empty output.
func ReadAll(path string) ([]byte, error) {
	b, err := os.ReadFile(path) //nolint:gosec // path comes from the logs table
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("logs: read %s: %w", path, err)
	}
	return b, nil
}

// Tail returns the last n lines of a log file. It reads backwards in blocks so
// that showing the tail of a large log does not load the whole thing.
func Tail(path string, n int) ([]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	f, err := os.Open(path) //nolint:gosec // path comes from the logs table
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("logs: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("logs: stat %s: %w", path, err)
	}
	size := st.Size()
	if size == 0 {
		return nil, nil
	}

	const block = 8 << 10
	var (
		collected []byte
		pos       = size
		newlines  int
	)
	for pos > 0 {
		readSize := int64(block)
		if pos < readSize {
			readSize = pos
		}
		pos -= readSize
		buf := make([]byte, readSize)
		if _, err := f.ReadAt(buf, pos); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("logs: read %s: %w", path, err)
		}
		collected = append(buf, collected...)

		// A trailing newline terminates the last line rather than starting a new one.
		newlines = bytes.Count(bytes.TrimSuffix(collected, []byte("\n")), []byte("\n"))
		if newlines >= n {
			break
		}
	}

	trimmed := bytes.TrimSuffix(collected, []byte("\n"))
	lines := bytes.Split(trimmed, []byte("\n"))
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := bytes.Join(lines, []byte("\n"))
	if len(out) > 0 {
		out = append(out, '\n')
	}
	return out, nil
}
