package buildlogs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/secrets"
)

// memoryAppender is the storage boundary: it records stored batches and can
// fail or fence on demand.
type memoryAppender struct {
	mu        sync.Mutex
	lines     []line
	omitted   int64
	lost      int64
	truncated bool
	failures  int  // fail this many appends before succeeding
	failAll   bool // fail every append
	fenced    bool // a newer attempt owns the stream
	calls     int
	err       error // returned while failing
}

func (m *memoryAppender) append(_ context.Context, _ Binding, b batch) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.fenced {
		return ErrSuperseded
	}
	if m.failAll || m.failures > 0 {
		if m.failures > 0 {
			m.failures--
		}
		if m.err == nil {
			m.err = errors.New("storage unavailable")
		}
		return m.err
	}
	for i := range b.texts {
		m.lines = append(m.lines, line{kind: b.kinds[i], text: b.texts[i]})
	}
	m.omitted += b.omitted
	m.lost += b.lost
	m.truncated = m.truncated || b.truncated
	return nil
}

func (m *memoryAppender) texts() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.lines))
	for i, l := range m.lines {
		out[i] = l.text
	}
	return out
}

func testWriter(t *testing.T, store appender, redactor *secrets.Redactor, limits Limits) *Writer {
	t.Helper()
	if redactor == nil {
		var err error
		if redactor, err = secrets.NewRedactor(); err != nil {
			t.Fatal(err)
		}
	}
	return newWriter(store, Binding{EnvironmentID: 1, Epoch: 1}, stored{}, redactor, limits, time.Now)
}

func write(t *testing.T, w *Writer, chunks ...string) {
	t.Helper()
	for _, c := range chunks {
		if _, err := w.Write([]byte(c)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWriterStoresRedactedSanitizedLinesInOrder(t *testing.T) {
	store := &memoryAppender{}
	redactor, err := secrets.NewRedactor("hunter2-secret", "ghs_platformtoken")
	if err != nil {
		t.Fatal(err)
	}
	w := testWriter(t, store, redactor, Limits{})
	write(t, w,
		"step 1\n",
		"token=hunter2-", "secret\n", // split across writes
		"colour \x1b[31mhunter2-\x1b[0msecret\n", // split by escape codes
		"<script>alert(1)</script>\n",
		"clone with ghs_platformtoken failed\n",
		"tail without newline",
	)
	w.System("Build failed: ghs_platformtoken was rejected")
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := store.texts()
	want := []string{
		"step 1",
		"token=[redacted]",
		"colour [redacted]",
		"<script>alert(1)</script>", // stored as text; pages render it escaped
		"clone with [redacted] failed",
		"tail without newline",
		"Build failed: [redacted] was rejected",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("stored:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if store.lines[len(store.lines)-1].kind != kindSystem || store.lines[0].kind != kindOutput {
		t.Fatal("line kinds not recorded")
	}
}

func TestWriterTruncatesAtCapsWithMarkerAndCounts(t *testing.T) {
	store := &memoryAppender{}
	w := testWriter(t, store, nil, Limits{Lines: 5, Bytes: 1 << 20, LineBytes: 64})
	for i := 0; i < 10; i++ {
		write(t, w, "line\n")
	}
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := store.texts()
	if len(got) != 5 || got[4] != "Log limit reached (1 MiB or 5 lines). Later output is not stored." || store.lines[4].kind != kindSystem {
		t.Fatalf("stored %q", got)
	}
	if !store.truncated || store.omitted != 6 {
		t.Fatalf("truncated=%v omitted=%d, want true/6", store.truncated, store.omitted)
	}

	bytesStore := &memoryAppender{}
	w = testWriter(t, bytesStore, nil, Limits{Lines: 1000, Bytes: 100, LineBytes: 64})
	for i := 0; i < 20; i++ {
		write(t, w, "0123456789\n")
	}
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	var size int
	for _, text := range bytesStore.texts() {
		size += len(text)
	}
	if size > 100 || !bytesStore.truncated || bytesStore.omitted == 0 {
		t.Fatalf("byte cap: size=%d truncated=%v omitted=%d", size, bytesStore.truncated, bytesStore.omitted)
	}
}

func TestWriterStopsWhenANewerAttemptOwnsTheStream(t *testing.T) {
	store := &memoryAppender{fenced: true}
	w := testWriter(t, store, nil, Limits{})
	write(t, w, "late output from a superseded execution\n")
	w.flush(t.Context(), true)
	select {
	case <-w.Done():
	default:
		t.Fatal("Done not closed after fencing")
	}
	if _, err := w.Write([]byte("more\n")); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("write after fencing: %v", err)
	}
	if err := w.Close(t.Context()); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("close after fencing: %v", err)
	}
	if len(store.texts()) != 0 {
		t.Fatal("a superseded writer stored output")
	}
}

func TestWriterRecoversFromStorageFailuresAndMarksGaps(t *testing.T) {
	store := &memoryAppender{failAll: true}
	w := testWriter(t, store, nil, Limits{})
	write(t, w, "before outage\n")
	w.flush(t.Context(), true) // fails; the line stays buffered
	// Fill the buffer while storage is down; the excess is dropped and counted.
	for i := 0; i < maxPendingLines+25; i++ {
		write(t, w, "during outage\n")
	}
	store.mu.Lock()
	store.failAll = false
	store.mu.Unlock()
	w.flush(t.Context(), true) // Storage is back: one batch frees buffer space.
	write(t, w, "after recovery\n")
	if err := w.Close(t.Context()); err == nil {
		t.Fatal("lost lines must be reported")
	} else {
		var lost *LostLinesError
		if !errors.As(err, &lost) || lost.Lines != 26 {
			t.Fatalf("close error: %v", err)
		}
	}
	got := store.texts()
	if got[0] != "before outage" || got[len(got)-1] != "after recovery" {
		t.Fatalf("order not kept: first %q last %q", got[0], got[len(got)-1])
	}
	if notice := got[len(got)-2]; notice != "26 lines could not be stored because log storage was unavailable or too slow." {
		t.Fatalf("gap notice missing: %q", notice)
	}
	if store.lost != 26 {
		t.Fatalf("lost counter %d", store.lost)
	}
}

func TestWriterCloseReportsOutputThatCouldNotBeStored(t *testing.T) {
	store := &memoryAppender{failAll: true}
	w := testWriter(t, store, nil, Limits{})
	write(t, w, "one\ntwo\n")
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	err := w.Close(ctx)
	var lost *LostLinesError
	if !errors.As(err, &lost) || lost.Lines != 2 || lost.Err == nil {
		t.Fatalf("close error: %v", err)
	}
}

func TestWriterShowsAPausedPartialLine(t *testing.T) {
	store := &memoryAppender{}
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	clock := func() time.Time { return time.Unix(0, now.Load()) }
	redactor, _ := secrets.NewRedactor()
	w := newWriter(store, Binding{EnvironmentID: 1, Epoch: 1}, stored{}, redactor, Limits{}, clock)
	defer func() { _ = w.Close(t.Context()) }()
	write(t, w, "Listening on :8080")
	w.flush(t.Context(), false)
	if len(store.texts()) != 0 {
		t.Fatal("a partial line was stored before output paused")
	}
	now.Add(int64(idleFlush))
	w.flush(t.Context(), false)
	if got := store.texts(); len(got) != 1 || got[0] != "Listening on :8080" {
		t.Fatalf("paused partial line: %q", got)
	}
}

func TestWriterContinuesAnExistingStreamsCaps(t *testing.T) {
	store := &memoryAppender{}
	redactor, _ := secrets.NewRedactor()
	w := newWriter(store, Binding{EnvironmentID: 1, Epoch: 1}, stored{lines: 3, bytes: 30}, redactor, Limits{Lines: 5}, time.Now)
	write(t, w, "a\nb\nc\n")
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := store.texts(); len(got) != 2 || !strings.HasPrefix(got[1], "Log limit reached") {
		t.Fatalf("a later writer must respect lines already stored: %q", got)
	}
}
