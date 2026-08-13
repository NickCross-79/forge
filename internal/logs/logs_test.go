package logs

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickcross-79/forge/internal/model"
)

// redactionFixture stands in for a secret value in these tests. It is a fixture,
// not a credential: the redactor only cares that the literal is long enough to
// be masked, so the content is deliberately self-describing.
const redactionFixture = "example-not-a-real-value"

// --- redaction --------------------------------------------------------------

func TestRedactorMasksSecrets(t *testing.T) {
	r := NewRedactor([]string{"example-value-alpha", "example-value-beta"})

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "value=example-value-alpha\n", "value=" + Mask + "\n"},
		{"twice", "a example-value-alpha b example-value-alpha", "a " + Mask + " b " + Mask},
		{"embedded", "prefixexample-value-alphasuffix", "prefix" + Mask + "suffix"},
		{"second value", "x example-value-beta y", "x " + Mask + " y"},
		{"both", "example-value-alpha and example-value-beta", Mask + " and " + Mask},
		{"absent", "nothing to see", "nothing to see"},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.RedactString(tc.in); got != tc.want {
				t.Errorf("RedactString(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRedactorIgnoresShortValues(t *testing.T) {
	// Masking a one- or two-character value would scribble over unrelated text.
	r := NewRedactor([]string{"a", "ab", "abc", "abcd"})
	if !r.Enabled() {
		t.Fatal("a value at the minimum length should still enable redaction")
	}
	got := r.RedactString("a ab abc abcd")
	if !strings.Contains(got, "a ab abc") {
		t.Errorf("short values should be left alone, got %q", got)
	}
	if !strings.Contains(got, Mask) {
		t.Errorf("the 4-character value should be masked, got %q", got)
	}
}

func TestRedactorPrefersLongestMatch(t *testing.T) {
	// "example" is contained in "example-extended"; the longer one must win so the
	// output is not left as "***-extended".
	r := NewRedactor([]string{"example", "example-extended"})
	got := r.RedactString("value=example-extended")
	if got != "value="+Mask {
		t.Errorf("RedactString() = %q, want the longest secret masked whole", got)
	}
}

func TestRedactorDeduplicatesAndReportsMaxLength(t *testing.T) {
	r := NewRedactor([]string{"same", "same", "longer-example"})
	if got := r.MaxSecretLen(); got != len("longer-example") {
		t.Errorf("MaxSecretLen() = %d, want %d", got, len("longer-example"))
	}
}

func TestRedactorDisabled(t *testing.T) {
	for _, r := range []*Redactor{nil, NewRedactor(nil), NewRedactor([]string{"ab"})} {
		if r.Enabled() {
			t.Errorf("Enabled() = true for %#v, want false", r)
		}
		if got := r.RedactString("untouched"); got != "untouched" {
			t.Errorf("RedactString() = %q, want the input unchanged", got)
		}
		if got := r.MaxSecretLen(); got != 0 {
			t.Errorf("MaxSecretLen() = %d, want 0", got)
		}
	}
}

func TestRedactDoesNotMutateInput(t *testing.T) {
	r := NewRedactor([]string{"example"})
	input := []byte("an example value")
	original := string(input)
	_ = r.Redact(input)
	if string(input) != original {
		t.Errorf("Redact mutated its input: %q", input)
	}
}

// --- writer -----------------------------------------------------------------

func newWriter(t *testing.T, opts WriterOptions) (*Writer, string) {
	t.Helper()
	if opts.Path == "" {
		opts.Path = filepath.Join(t.TempDir(), "out.log")
	}
	w, err := NewWriter(opts)
	if err != nil {
		t.Fatal(err)
	}
	return w, opts.Path
}

func TestWriterCapturesOutput(t *testing.T) {
	w, path := newWriter(t, WriterOptions{})
	if _, err := w.Write([]byte("first\nsecond\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "first\nsecond\n" {
		t.Errorf("file = %q", body)
	}
	if w.Bytes() != int64(len("first\nsecond\n")) {
		t.Errorf("Bytes() = %d, want %d", w.Bytes(), len("first\nsecond\n"))
	}
	if w.Truncated() {
		t.Error("Truncated() = true for a small write")
	}
}

func TestWriterFlushesPartialLineOnClose(t *testing.T) {
	// Output that never ends in a newline must still be captured.
	w, path := newWriter(t, WriterOptions{})
	if _, err := w.Write([]byte("no trailing newline")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	body, _ := os.ReadFile(path)
	if string(body) != "no trailing newline" {
		t.Errorf("file = %q, want the partial line flushed", body)
	}
}

func TestWriterRedactsBeforeDisk(t *testing.T) {
	w, path := newWriter(t, WriterOptions{Redactor: NewRedactor([]string{redactionFixture})})
	if _, err := w.Write([]byte("value=" + redactionFixture + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), redactionFixture) {
		t.Errorf("the secret reached disk: %q", body)
	}
	if !strings.Contains(string(body), Mask) {
		t.Errorf("expected a mask on disk, got %q", body)
	}
}

func TestWriterRedactsSecretSplitAcrossWrites(t *testing.T) {
	// The secret arrives in two pieces, as it would from a pipe. Line buffering
	// means it is only matched once the whole line is present.
	w, path := newWriter(t, WriterOptions{Redactor: NewRedactor([]string{redactionFixture})})
	if _, err := w.Write([]byte("value=example-not-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("-real-value\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), redactionFixture) {
		t.Errorf("a secret split across writes reached disk: %q", body)
	}
}

func TestWriterRedactsAcrossAForcedFlush(t *testing.T) {
	// A very long line with no newline forces an early flush. The writer holds
	// back a tail so a secret straddling that boundary is still caught.
	secret := redactionFixture
	w, path := newWriter(t, WriterOptions{Redactor: NewRedactor([]string{secret})})

	filler := strings.Repeat("x", forceFlushThreshold)
	if _, err := w.Write([]byte(filler + "example-not-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("-real-value tail\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), secret) {
		t.Error("a secret straddling a forced flush reached disk")
	}
}

func TestWriterTruncatesAtTheSizeCap(t *testing.T) {
	w, path := newWriter(t, WriterOptions{MaxBytes: 64})
	for i := 0; i < 100; i++ {
		if _, err := w.Write([]byte(strings.Repeat("y", 20) + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if !w.Truncated() {
		t.Error("Truncated() = false after exceeding the cap")
	}
	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), "truncated by forge") {
		t.Errorf("expected a visible truncation notice, got %q", body)
	}
	// The notice is written exactly once, however many writes follow.
	if n := strings.Count(string(body), "truncated by forge"); n != 1 {
		t.Errorf("truncation notice appears %d times, want 1", n)
	}
	// Payload is capped, though the notice itself is allowed past it.
	payload := strings.Count(string(body), "y")
	if payload > 64 {
		t.Errorf("wrote %d payload bytes, want at most 64", payload)
	}
}

func TestWriterAlwaysReportsFullConsumption(t *testing.T) {
	// A short write would be read as an I/O error by the job's pipe and could
	// kill the process for the wrong reason.
	w, _ := newWriter(t, WriterOptions{MaxBytes: 8})
	chunk := []byte(strings.Repeat("z", 100) + "\n")
	n, err := w.Write(chunk)
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if n != len(chunk) {
		t.Errorf("Write() = %d, want %d even when truncating", n, len(chunk))
	}
	_ = w.Close()
}

func TestWriterAfterClose(t *testing.T) {
	w, _ := newWriter(t, WriterOptions{})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("late")); err == nil {
		t.Error("Write() after Close() should fail")
	}
	// Close is idempotent.
	if err := w.Close(); err != nil {
		t.Errorf("second Close() error = %v, want nil", err)
	}
}

func TestWriterRequiresPath(t *testing.T) {
	if _, err := NewWriter(WriterOptions{}); err == nil {
		t.Error("NewWriter() with no path should fail")
	}
}

func TestWriterIsConcurrencySafe(t *testing.T) {
	w, path := newWriter(t, WriterOptions{})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_, _ = w.Write([]byte("concurrent line\n"))
			}
		}()
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	body, _ := os.ReadFile(path)
	if got := strings.Count(string(body), "concurrent line"); got != 400 {
		t.Errorf("wrote %d lines, want 400", got)
	}
}

func TestWriterNotifiesBroker(t *testing.T) {
	broker := NewBroker()
	notify, cancel := broker.Subscribe("topic")
	defer cancel()

	w, _ := newWriter(t, WriterOptions{Broker: broker, Topic: "topic"})
	if _, err := w.Write([]byte("line\n")); err != nil {
		t.Fatal(err)
	}

	select {
	case <-notify:
	case <-time.After(2 * time.Second):
		t.Fatal("the broker was not notified of new log bytes")
	}

	// Closing the writer closes the topic, which ends streaming.
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case _, open := <-notify:
		if open {
			// Drain a pending notification, then the channel must close.
			select {
			case _, open := <-notify:
				if open {
					t.Error("the topic channel should be closed after Close()")
				}
			case <-time.After(2 * time.Second):
				t.Error("the topic channel was not closed after Close()")
			}
		}
	case <-time.After(2 * time.Second):
		t.Error("the topic channel was not closed after Close()")
	}
}

// --- reader -----------------------------------------------------------------

func TestReadFromOffsets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.log")
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}

	data, next, err := ReadFrom(path, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "0123" || next != 4 {
		t.Errorf("first read = %q, next %d; want %q, 4", data, next, "0123")
	}

	data, next, err = ReadFrom(path, next, 100)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "456789" || next != 10 {
		t.Errorf("second read = %q, next %d; want %q, 10", data, next, "456789")
	}

	// At EOF there is nothing more, and the offset does not move.
	data, next, err = ReadFrom(path, next, 100)
	if err != nil || len(data) != 0 || next != 10 {
		t.Errorf("read at EOF = %q, %d, %v; want empty, 10, nil", data, next, err)
	}
}

func TestReadFromHandlesMissingAndOverlongOffsets(t *testing.T) {
	// A job that has not produced output yet has no file; that is not an error.
	data, next, err := ReadFrom(filepath.Join(t.TempDir(), "absent.log"), 0, 100)
	if err != nil || len(data) != 0 || next != 0 {
		t.Errorf("missing file = %q, %d, %v; want empty, 0, nil", data, next, err)
	}

	path := filepath.Join(t.TempDir(), "short.log")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An offset past the end (a truncated file) clamps rather than erroring.
	_, next, err = ReadFrom(path, 500, 100)
	if err != nil {
		t.Fatal(err)
	}
	if next != 3 {
		t.Errorf("clamped offset = %d, want 3", next)
	}
	// A negative offset is treated as the start.
	data, _, err = ReadFrom(path, -5, 100)
	if err != nil || string(data) != "abc" {
		t.Errorf("negative offset = %q, %v", data, err)
	}
}

func TestReadAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.log")
	if err := os.WriteFile(path, []byte("full body\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body, err := ReadAll(path)
	if err != nil || string(body) != "full body\n" {
		t.Errorf("ReadAll() = %q, %v", body, err)
	}

	body, err = ReadAll(filepath.Join(t.TempDir(), "absent.log"))
	if err != nil || len(body) != 0 {
		t.Errorf("ReadAll(missing) = %q, %v; want empty and no error", body, err)
	}
}

func TestTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.log")
	var b strings.Builder
	for i := 1; i <= 500; i++ {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", 40))
		b.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := Tail(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(out), "\n"); got != 3 {
		t.Errorf("Tail(3) returned %d lines, want 3", got)
	}

	// Asking for more lines than exist returns everything.
	small := filepath.Join(t.TempDir(), "small.log")
	if err := os.WriteFile(small, []byte("a\nb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = Tail(small, 100)
	if err != nil || string(out) != "a\nb\n" {
		t.Errorf("Tail(100) = %q, %v", out, err)
	}

	if out, _ := Tail(path, 0); out != nil {
		t.Errorf("Tail(0) = %q, want nil", out)
	}
	if out, err := Tail(filepath.Join(t.TempDir(), "absent.log"), 5); err != nil || out != nil {
		t.Errorf("Tail(missing) = %q, %v", out, err)
	}
}

// --- broker -----------------------------------------------------------------

func TestBrokerNotifiesSubscribers(t *testing.T) {
	b := NewBroker()
	a, cancelA := b.Subscribe("t")
	defer cancelA()
	c, cancelC := b.Subscribe("t")
	defer cancelC()

	b.Notify("t")

	for i, ch := range []<-chan struct{}{a, c} {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Errorf("subscriber %d was not notified", i)
		}
	}
}

func TestBrokerNotifyNeverBlocks(t *testing.T) {
	// A subscriber that never drains must not stall the producer: notifications
	// coalesce because the subscriber re-reads everything when it wakes.
	b := NewBroker()
	_, cancel := b.Subscribe("t")
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 10_000; i++ {
			b.Notify("t")
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Notify blocked on an undrained subscriber")
	}
}

func TestBrokerCloseEndsSubscribers(t *testing.T) {
	b := NewBroker()
	ch, cancel := b.Subscribe("t")
	defer cancel()

	b.Close("t")

	select {
	case _, open := <-ch:
		if open {
			t.Error("the channel should be closed, not signalled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close() did not close the subscriber channel")
	}

	if !b.Closed("t") {
		t.Error("Closed() = false after Close()")
	}
	// Notifying a closed topic is a harmless no-op.
	b.Notify("t")
	// Closing twice is also safe.
	b.Close("t")
}

func TestBrokerSubscribeAfterCloseReturnsClosedChannel(t *testing.T) {
	// A client that arrives after the job finished should take its normal
	// "stream ended" path rather than hanging forever.
	b := NewBroker()
	_, cancel := b.Subscribe("t")
	b.Close("t")
	cancel()

	ch, cancel2 := b.Subscribe("t")
	defer cancel2()
	select {
	case _, open := <-ch:
		if open {
			t.Error("expected an already-closed channel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscribing to a closed topic hung")
	}
}

func TestBrokerUnknownTopicIsClosed(t *testing.T) {
	if !NewBroker().Closed("never-opened") {
		t.Error("an unopened topic should report as closed")
	}
}

func TestBrokerCancelUnsubscribes(t *testing.T) {
	b := NewBroker()
	ch, cancel := b.Subscribe("t")
	cancel()

	select {
	case _, open := <-ch:
		if open {
			t.Error("cancel should close the subscriber channel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not close the channel")
	}
	// Cancelling twice must not panic.
	cancel()
}

func TestBrokerConcurrentUse(t *testing.T) {
	b := NewBroker()
	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, cancel := b.Subscribe("shared")
			defer cancel()
			b.Notify("shared")
			select {
			case <-ch:
			case <-time.After(2 * time.Second):
			}
		}()
	}
	wg.Wait()
	b.Close("shared")
}

func TestTopicNames(t *testing.T) {
	if got := LogTopic(7, model.StreamStdout); got != "log:7:stdout" {
		t.Errorf("LogTopic() = %q", got)
	}
	if got := RunTopic(42); got != "run:42" {
		t.Errorf("RunTopic() = %q", got)
	}
	// A job's two streams must never collide.
	if LogTopic(1, model.StreamStdout) == LogTopic(1, model.StreamStderr) {
		t.Error("stdout and stderr share a topic")
	}
}
