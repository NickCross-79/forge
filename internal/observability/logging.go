// Package observability configures forge's structured application logging.
//
// Application logs are separate from job logs: these describe what forge itself
// is doing (scheduling, HTTP requests, storage), while a job's stdout and stderr
// are captured by the logs package and never mixed in here.
package observability

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// Format selects the log encoding.
type Format string

const (
	// FormatText is the human-readable console format.
	FormatText Format = "text"
	// FormatJSON is one JSON object per line, for piping into a log processor.
	FormatJSON Format = "json"
)

// Options configures the logger.
type Options struct {
	Level  slog.Level
	Format Format
	Writer io.Writer
	// Color enables ANSI colour in the text format.
	Color bool
	// AddSource includes the calling file and line, useful when debugging forge
	// itself rather than a pipeline.
	AddSource bool
}

// NewLogger builds a slog.Logger.
func NewLogger(opts Options) *slog.Logger {
	if opts.Writer == nil {
		opts.Writer = os.Stderr
	}
	handlerOpts := &slog.HandlerOptions{Level: opts.Level, AddSource: opts.AddSource}

	if opts.Format == FormatJSON {
		return slog.New(slog.NewJSONHandler(opts.Writer, handlerOpts))
	}
	return slog.New(&consoleHandler{
		mu:    &sync.Mutex{},
		out:   opts.Writer,
		level: opts.Level,
		color: opts.Color,
	})
}

// ParseLevel converts a level name to a slog.Level, defaulting to info.
func ParseLevel(name string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// consoleHandler renders logs for a terminal.
//
// slog's built-in TextHandler emits key=value pairs including a full timestamp,
// which is noisy in the foreground of `forge run`. This prints a short level tag,
// the message, and the attributes, which reads far better next to a job's own
// output.
type consoleHandler struct {
	// mu is shared by every clone of a handler so that concurrent goroutines
	// writing through different loggers cannot interleave a single line.
	mu     *sync.Mutex
	out    io.Writer
	level  slog.Level
	color  bool
	attrs  []slog.Attr
	groups []string
}

func (h *consoleHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *consoleHandler) Handle(_ context.Context, rec slog.Record) error {
	var b strings.Builder

	b.WriteString(h.levelTag(rec.Level))
	b.WriteByte(' ')
	b.WriteString(rec.Message)

	writeAttr := func(a slog.Attr) bool {
		if a.Equal(slog.Attr{}) {
			return true
		}
		key := a.Key
		if len(h.groups) > 0 {
			key = strings.Join(h.groups, ".") + "." + key
		}
		if h.color {
			fmt.Fprintf(&b, " \033[90m%s=\033[0m%v", key, a.Value.Any())
		} else {
			fmt.Fprintf(&b, " %s=%v", key, a.Value.Any())
		}
		return true
	}
	for _, a := range h.attrs {
		writeAttr(a)
	}
	rec.Attrs(writeAttr)
	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.out, b.String())
	return err
}

func (h *consoleHandler) levelTag(level slog.Level) string {
	var text, color string
	switch {
	case level >= slog.LevelError:
		text, color = "ERR ", "\033[31m"
	case level >= slog.LevelWarn:
		text, color = "WARN", "\033[33m"
	case level >= slog.LevelInfo:
		text, color = "INFO", "\033[34m"
	default:
		text, color = "DBG ", "\033[90m"
	}
	if !h.color {
		return text
	}
	return color + text + "\033[0m"
}

func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := h.clone()
	clone.attrs = append(clone.attrs, attrs...)
	return clone
}

func (h *consoleHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := h.clone()
	clone.groups = append(clone.groups, name)
	return clone
}

// clone copies the handler's configuration, sharing the write mutex so all
// clones serialise against the same output.
func (h *consoleHandler) clone() *consoleHandler {
	return &consoleHandler{
		mu:     h.mu,
		out:    h.out,
		level:  h.level,
		color:  h.color,
		attrs:  append([]slog.Attr(nil), h.attrs...),
		groups: append([]string(nil), h.groups...),
	}
}
