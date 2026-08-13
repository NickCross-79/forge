package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/nickcross-79/forge/internal/model"
)

// ANSI colours, emptied when colour is disabled so every call site can stay
// unconditional.
var (
	colorReset  = "\033[0m"
	colorBold   = "\033[1m"
	colorDim    = "\033[2m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
	colorGrey   = "\033[90m"
)

// disableColor blanks the escape sequences. It is called when output is not a
// terminal, when NO_COLOR is set, or when --no-color is passed.
func disableColor() {
	colorReset, colorBold, colorDim = "", "", ""
	colorRed, colorGreen, colorYellow, colorBlue, colorGrey = "", "", "", "", ""
}

// shouldColor reports whether to emit escape sequences. It honours the NO_COLOR
// convention and refuses to colour output that is being piped somewhere.
func shouldColor(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// statusGlyph is the single-character marker shown next to a status.
func statusGlyph(s model.Status) string {
	switch s {
	case model.StatusSuccess:
		return colorGreen + "✔" + colorReset
	case model.StatusFailed:
		return colorRed + "✘" + colorReset
	case model.StatusRunning:
		return colorBlue + "▸" + colorReset
	case model.StatusCancelled:
		return colorYellow + "⊘" + colorReset
	case model.StatusSkipped:
		return colorGrey + "–" + colorReset
	case model.StatusRetrying:
		return colorYellow + "↻" + colorReset
	case model.StatusAwaitingManual:
		return colorYellow + "⏸" + colorReset
	default:
		return colorGrey + "•" + colorReset
	}
}

// colorStatus renders a status name in its semantic colour.
func colorStatus(s model.Status) string {
	switch s {
	case model.StatusSuccess:
		return colorGreen + string(s) + colorReset
	case model.StatusFailed:
		return colorRed + string(s) + colorReset
	case model.StatusRunning:
		return colorBlue + string(s) + colorReset
	case model.StatusCancelled, model.StatusRetrying, model.StatusAwaitingManual:
		return colorYellow + string(s) + colorReset
	case model.StatusSkipped:
		return colorGrey + string(s) + colorReset
	default:
		return string(s)
	}
}

// formatDuration renders a millisecond count compactly: sub-second values keep
// their precision, longer ones are rounded to something readable at a glance.
func formatDuration(ms int64) string {
	if ms <= 0 {
		return "–"
	}
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", ms)
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// formatBytes renders a byte count in binary units.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// formatRelative renders a timestamp as an age, which is what a run list wants.
func formatRelative(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "–"
	}
	d := time.Since(*t)
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
}

// table is a thin wrapper over tabwriter that keeps column setup in one place.
type table struct {
	w  *tabwriter.Writer
	tw io.Writer
}

func newTable(w io.Writer) *table {
	return &table{w: tabwriter.NewWriter(w, 0, 0, 2, ' ', 0), tw: w}
}

// header writes a dimmed heading row.
func (t *table) header(cols ...string) {
	fmt.Fprintln(t.w, colorDim+strings.Join(cols, "\t")+colorReset)
}

func (t *table) row(cols ...string) {
	fmt.Fprintln(t.w, strings.Join(cols, "\t"))
}

func (t *table) flush() {
	_ = t.w.Flush()
}

// hint prints a dimmed follow-up suggestion, the "what next" line that makes a
// CLI feel navigable.
func hint(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "%s%s%s\n", colorGrey, fmt.Sprintf(format, args...), colorReset)
}

// success, warn and fail print a single status line.
func success(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "%s✔%s %s\n", colorGreen, colorReset, fmt.Sprintf(format, args...))
}

func warn(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "%s!%s %s\n", colorYellow, colorReset, fmt.Sprintf(format, args...))
}

func fail(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "%s✘%s %s\n", colorRed, colorReset, fmt.Sprintf(format, args...))
}

// bold emphasises a fragment of a line.
func bold(s string) string { return colorBold + s + colorReset }

// dim de-emphasises a fragment of a line.
func dim(s string) string { return colorGrey + s + colorReset }

// truncate shortens a string for a table cell, marking that it was cut.
func truncate(s string, max int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if max <= 1 || len([]rune(s)) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max-1]) + "…"
}
