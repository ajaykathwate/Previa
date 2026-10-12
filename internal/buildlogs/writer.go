package buildlogs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/textlines"
	"github.com/ajaykathwatee/previa/internal/secrets"
)

// Limits bound what one attempt stores. Output beyond them is discarded and
// counted, and a marker line says so; nothing is dropped silently.
type Limits struct {
	Bytes     int64 // stored text across all lines
	Lines     int64 // stored lines, including Previa's notices
	LineBytes int   // one line, before its truncation marker
}

// DefaultLimits are the current-build caps: 5 MiB and 20,000 lines per
// attempt, 8 KiB per line.
func DefaultLimits() Limits { return Limits{Bytes: 5 << 20, Lines: 20_000, LineBytes: 8 << 10} }

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.Bytes <= 0 {
		l.Bytes = d.Bytes
	}
	if l.Lines <= 0 {
		l.Lines = d.Lines
	}
	if l.LineBytes <= 0 {
		l.LineBytes = d.LineBytes
	}
	return l
}

const (
	kindOutput = "output"
	kindSystem = "system"

	flushInterval   = 250 * time.Millisecond
	idleFlush       = time.Second // a paused, unfinished line is shown after this
	batchLines      = 200
	batchBytes      = 64 << 10
	maxPendingLines = 5_000 // unstored lines held while storage is slow or failing
	maxPendingBytes = 2 << 20
	appendTimeout   = 5 * time.Second
	maxRetryDelay   = 5 * time.Second
)

var errWriterClosed = errors.New("write to closed build log writer")

// LostLinesError reports output that could not be stored because storage was
// unavailable or fell too far behind.
type LostLinesError struct {
	Lines int64
	Err   error // the last storage error, if any
}

func (e *LostLinesError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%d build log lines could not be stored: %v", e.Lines, e.Err)
	}
	return fmt.Sprintf("%d build log lines could not be stored", e.Lines)
}

func (e *LostLinesError) Unwrap() error { return e.Err }

type line struct {
	kind, text string
	at         time.Time
}

// batch is one storage write: lines plus counters accumulated since the last.
type batch struct {
	kinds, texts  []string
	times         []time.Time
	bytes         int64
	truncated     bool
	omitted, lost int64
}

// appender stores a batch for a binding and returns ErrSuperseded once the
// attempt no longer owns the stream.
type appender interface {
	append(ctx context.Context, b Binding, batch batch) error
}

// Writer stores one attempt's output. Bytes pass through control-character
// stripping, redaction and line splitting, then a background flusher stores
// them in bounded batches. Write never waits for the database; if storage falls
// behind, the excess is dropped, counted and marked where it happened.
// Writer is safe for concurrent use.
type Writer struct {
	store    appender
	binding  Binding
	limits   Limits
	notice   string // the truncation marker for these limits
	redactor *secrets.Redactor
	now      func() time.Time

	flushing  sync.Mutex // one storage write at a time, so a batch is stored once
	mu        sync.Mutex
	input     io.Writer
	redacting *secrets.RedactingWriter
	splitter  *textlines.Splitter

	pending       []line
	pendingBytes  int
	acceptedLines int64 // stored or pending, for Limits
	acceptedBytes int64
	truncated     bool
	omitted       int64 // discarded after a cap, not yet recorded
	lost          int64 // dropped because storage fell behind, not yet recorded
	lostTotal     int64
	gap           int64 // dropped since the last notice line
	lastWrite     time.Time
	retryAt       time.Time
	failures      int
	storageErr    error
	fenced        bool
	closed        bool

	wake    chan struct{}
	stop    chan struct{}
	stopped chan struct{}
	done    chan struct{}
}

// stored describes what the stream already holds when a writer opens it, so a
// later writer for the same attempt (startup after the build) continues the caps.
type stored struct {
	lines, bytes int64
	truncated    bool
}

func newWriter(store appender, b Binding, start stored, redactor *secrets.Redactor, limits Limits, now func() time.Time) *Writer {
	w := &Writer{
		store: store, binding: b, limits: limits.withDefaults(), redactor: redactor, now: now,
		acceptedLines: start.lines, acceptedBytes: start.bytes, truncated: start.truncated,
		wake: make(chan struct{}, 1), stop: make(chan struct{}), stopped: make(chan struct{}), done: make(chan struct{}),
	}
	w.notice = fmt.Sprintf("Log limit reached (%s or %d lines). Later output is not stored.", formatBytes(w.limits.Bytes), w.limits.Lines)
	w.splitter = &textlines.Splitter{MaxBytes: w.limits.LineBytes, Emit: func(text string) { w.accept(kindOutput, text) }}
	w.redacting = redactor.Writer(w.splitter)
	w.input = &textlines.Stripper{Dst: w.redacting}
	go w.run()
	return w
}

// Write records build or startup output. It fails with ErrSuperseded once a
// newer attempt owns the stream.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, errWriterClosed
	}
	if w.fenced {
		return 0, ErrSuperseded
	}
	w.lastWrite = w.now()
	if _, err := w.input.Write(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// System records a notice from Previa itself, such as a phase or failure reason.
// It is sanitized and redacted like output. An unfinished output line is ended
// first so the notice appears after it.
func (w *Writer) System(message string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.fenced {
		return
	}
	w.splitter.Flush()
	var clean strings.Builder
	stripper := textlines.Stripper{Dst: &clean}
	_, _ = stripper.Write([]byte(message))
	text := strings.ToValidUTF8(string(w.redactor.Redact([]byte(clean.String()))), "�")
	for _, part := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if len(part) > w.limits.LineBytes {
			part = strings.ToValidUTF8(part[:w.limits.LineBytes], "�") + " … [line truncated]"
		}
		w.accept(kindSystem, part)
	}
}

// Done is closed when a newer attempt has taken over the stream; producers
// should stop the superseded work.
func (w *Writer) Done() <-chan struct{} { return w.done }

// Close stores what is still buffered, records discarded and lost line counts
// and stops the flusher. It returns ErrSuperseded if a newer attempt owns the
// stream and a *LostLinesError if some output could not be stored. Close does
// not finish the stream: later work for the same attempt may open a new writer.
func (w *Writer) Close(ctx context.Context) error {
	w.mu.Lock()
	if w.closed {
		defer w.mu.Unlock()
		return w.result()
	}
	w.closed = true
	if !w.fenced {
		_ = w.redacting.Close() // Releases a held-back tail, still redacted.
		w.splitter.Flush()
	}
	w.mu.Unlock()
	close(w.stop)
	<-w.stopped
	for w.flush(ctx, true) {
		w.mu.Lock()
		failing, delay := w.storageErr != nil, w.retryAt.Sub(w.now())
		w.mu.Unlock()
		if !failing {
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(max(delay, 0)):
		}
		if ctx.Err() != nil {
			break
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.fenced && len(w.pending) > 0 {
		w.lostTotal += int64(len(w.pending))
		w.pending, w.pendingBytes = nil, 0
	}
	return w.result()
}

func (w *Writer) result() error {
	switch {
	case w.fenced:
		return ErrSuperseded
	case w.lostTotal > 0:
		return &LostLinesError{Lines: w.lostTotal, Err: w.storageErr}
	}
	return nil
}

// accept applies the stream caps. Callers hold w.mu.
func (w *Writer) accept(kind, text string) {
	if w.truncated {
		w.omitted++
		return
	}
	// Keep room for the truncation notice itself.
	if w.acceptedLines+2 > w.limits.Lines || w.acceptedBytes+int64(len(text)+len(w.notice)) > w.limits.Bytes {
		w.truncated = true
		w.omitted++
		w.queue(kindSystem, w.notice)
		return
	}
	w.queue(kind, text)
}

// queue buffers a line for storage, or drops and counts it when storage has
// fallen behind. The next line that fits is preceded by a notice of the gap.
func (w *Writer) queue(kind, text string) {
	if w.fenced {
		return
	}
	if len(w.pending) >= maxPendingLines || w.pendingBytes+len(text) > maxPendingBytes {
		w.lost++
		w.lostTotal++
		w.gap++
		return
	}
	if w.gap > 0 {
		notice := fmt.Sprintf("%d lines could not be stored because log storage was unavailable or too slow.", w.gap)
		w.gap = 0
		w.push(kindSystem, notice)
	}
	w.push(kind, text)
}

func (w *Writer) push(kind, text string) {
	w.pending = append(w.pending, line{kind: kind, text: text, at: w.now()})
	w.pendingBytes += len(text)
	w.acceptedLines++
	w.acceptedBytes += int64(len(text))
	if len(w.pending) >= batchLines || w.pendingBytes >= batchBytes {
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
}

func (w *Writer) run() {
	defer close(w.stopped)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-ticker.C:
		case <-w.wake:
		}
		w.flush(context.Background(), false)
	}
}

// flush stores at most one batch and reports whether anything is left. final
// ignores the retry backoff; the caller paces retries.
func (w *Writer) flush(ctx context.Context, final bool) bool {
	w.flushing.Lock()
	defer w.flushing.Unlock()
	w.mu.Lock()
	if w.fenced {
		w.mu.Unlock()
		return false
	}
	now := w.now()
	if !final && now.Before(w.retryAt) {
		w.mu.Unlock()
		return true
	}
	if !final && !w.closed && !w.lastWrite.IsZero() && now.Sub(w.lastWrite) >= idleFlush {
		w.splitter.Flush() // Output paused mid-line: show what arrived.
	}
	n, size := 0, 0
	for n < len(w.pending) && n < batchLines && (n == 0 || size+len(w.pending[n].text) <= batchBytes) {
		size += len(w.pending[n].text)
		n++
	}
	b := batch{bytes: int64(size), truncated: w.truncated, omitted: w.omitted, lost: w.lost}
	if n == 0 && b.omitted == 0 && b.lost == 0 {
		w.mu.Unlock()
		return false
	}
	b.kinds, b.texts, b.times = make([]string, n), make([]string, n), make([]time.Time, n)
	for i, l := range w.pending[:n] {
		b.kinds[i], b.texts[i], b.times[i] = l.kind, l.text, l.at
	}
	w.mu.Unlock()

	appendCtx, cancel := context.WithTimeout(ctx, appendTimeout)
	err := w.store.append(appendCtx, w.binding, b)
	cancel()

	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case err == nil:
		w.pending = w.pending[:copy(w.pending, w.pending[n:])]
		w.pendingBytes -= size
		w.omitted -= b.omitted
		w.lost -= b.lost
		w.failures, w.retryAt, w.storageErr = 0, time.Time{}, nil
	case errors.Is(err, ErrSuperseded):
		w.fence()
		return false
	default:
		w.failures++
		w.storageErr = err
		w.retryAt = now.Add(min(maxRetryDelay, flushInterval<<min(w.failures, 8)))
	}
	return len(w.pending) > 0 || w.omitted > 0 || w.lost > 0
}

// fence stops a superseded writer. Callers hold w.mu.
func (w *Writer) fence() {
	if w.fenced {
		return
	}
	w.fenced = true
	w.pending, w.pendingBytes = nil, 0
	close(w.done)
}

// formatBytes renders a byte count as the largest whole binary unit.
func formatBytes(n int64) string {
	switch {
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%d KiB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}
