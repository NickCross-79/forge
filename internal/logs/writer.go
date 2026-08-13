package logs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// forceFlushThreshold bounds how much unterminated output is buffered while
// waiting for a newline. Progress bars that only emit carriage returns would
// otherwise buffer forever.
const forceFlushThreshold = 32 << 10

// Writer captures one stream (stdout or stderr) of one job attempt to a file,
// redacting secrets and enforcing a size cap on the way.
//
// Output is buffered per line so that a secret is matched against a whole line
// rather than against whatever fragment happened to arrive in a single write.
// When a line is implausibly long the buffer is flushed early, holding back the
// last MaxSecretLen-1 bytes so a secret straddling the flush boundary is still
// caught.
type Writer struct {
	mu        sync.Mutex
	file      *os.File
	redactor  *Redactor
	broker    *Broker
	topic     string
	max       int64
	written   int64
	payload   int64
	truncated bool
	noticed   bool
	pending   []byte
	closed    bool
}

// WriterOptions configures a Writer.
type WriterOptions struct {
	// Path is the file to create. Parent directories are created as needed.
	Path string
	// Redactor masks secrets. May be nil.
	Redactor *Redactor
	// Broker is notified whenever new bytes land, so SSE subscribers can catch up.
	// May be nil.
	Broker *Broker
	// Topic identifies this stream to the broker.
	Topic string
	// MaxBytes truncates the stream beyond this size. Zero means unlimited.
	MaxBytes int64
}

// NewWriter creates the log file and returns a Writer for it.
func NewWriter(opts WriterOptions) (*Writer, error) {
	if opts.Path == "" {
		return nil, errors.New("logs: writer path is required")
	}
	if err := os.MkdirAll(filepath.Dir(opts.Path), 0o750); err != nil {
		return nil, fmt.Errorf("logs: create dir: %w", err)
	}
	f, err := os.OpenFile(opts.Path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640) //nolint:gosec // path built from config
	if err != nil {
		return nil, fmt.Errorf("logs: create %s: %w", opts.Path, err)
	}
	return &Writer{
		file:     f,
		redactor: opts.Redactor,
		broker:   opts.Broker,
		topic:    opts.Topic,
		max:      opts.MaxBytes,
	}, nil
}

// Write implements io.Writer. It always reports the full input as consumed, even
// when output was truncated by the size cap: the producer is a job's pipe and
// reporting a short write there would kill the job for the wrong reason.
func (w *Writer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}

	w.pending = append(w.pending, p...)

	// Flush every complete line.
	if idx := lastNewline(w.pending); idx >= 0 {
		if err := w.flushLocked(w.pending[:idx+1]); err != nil {
			return len(p), err
		}
		w.pending = append(w.pending[:0], w.pending[idx+1:]...)
	} else if len(w.pending) >= forceFlushThreshold {
		// No newline in sight. Emit everything except a tail long enough to hold
		// any secret that might be split across this boundary.
		hold := w.redactor.MaxSecretLen()
		if hold > 0 {
			hold--
		}
		if hold >= len(w.pending) {
			hold = len(w.pending) - 1
		}
		cut := len(w.pending) - hold
		if err := w.flushLocked(w.pending[:cut]); err != nil {
			return len(p), err
		}
		w.pending = append(w.pending[:0], w.pending[cut:]...)
	}
	return len(p), nil
}

// flushLocked redacts and writes a chunk, honouring the size cap.
func (w *Writer) flushLocked(chunk []byte) error {
	if len(chunk) == 0 {
		return nil
	}
	out := w.redactor.Redact(chunk)

	if w.max > 0 {
		remaining := w.max - w.payload
		if remaining < 0 {
			remaining = 0
		}
		if int64(len(out)) > remaining {
			out = out[:remaining]
			w.truncated = true
		}
	}

	if len(out) > 0 {
		n, err := w.file.Write(out)
		w.written += int64(n)
		w.payload += int64(n)
		if err != nil {
			return fmt.Errorf("logs: write: %w", err)
		}
	}

	if w.truncated && !w.noticed {
		// Leave a visible marker so a reader knows output was cut short. The
		// notice is not counted against the cap.
		w.noticed = true
		n, err := w.file.WriteString("\n... output truncated by forge (log size limit reached) ...\n")
		w.written += int64(n)
		if err != nil {
			return fmt.Errorf("logs: write truncation notice: %w", err)
		}
	}

	if w.broker != nil && w.topic != "" {
		w.broker.Notify(w.topic)
	}
	return nil
}

// Close flushes any buffered partial line and closes the file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	flushErr := w.flushLocked(w.pending)
	w.pending = nil
	closeErr := w.file.Close()
	if w.broker != nil && w.topic != "" {
		w.broker.Close(w.topic)
	}
	return errors.Join(flushErr, closeErr)
}

// Bytes reports the size of the log file on disk, after redaction and any
// truncation notice.
func (w *Writer) Bytes() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.written
}

// Truncated reports whether the size cap dropped output.
func (w *Writer) Truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.truncated
}

func lastNewline(b []byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == '\n' {
			return i
		}
	}
	return -1
}
