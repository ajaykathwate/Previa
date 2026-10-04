package secrets

import (
	"bytes"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
)

// Redaction bounds.
const (
	// MinRedactBytes is the shortest value that is redacted. Shorter values match
	// ordinary output constantly; never use such values as secrets.
	MinRedactBytes = 4
	// MaxRedactPatterns bounds the work per write.
	MaxRedactPatterns = 512
	// Replacement is written in place of every redacted occurrence.
	Replacement = "[redacted]"
)

// ErrTooManyPatterns is returned when a redactor would exceed MaxRedactPatterns.
var ErrTooManyPatterns = errors.New("too many values to redact")

// Redactor replaces known sensitive values, such as injected secrets and platform
// credentials, in streamed output before it is stored or shown.
//
// Limits: it matches exact byte sequences only. It cannot catch values that the
// running code transforms (base64, hex, URL-encoding, reversing, splitting with
// other output in between), values shorter than MinRedactBytes, or anything the
// code sends elsewhere. Untrusted code that legitimately receives a credential can
// always exfiltrate it; redaction protects logs, not the credential.
type Redactor struct {
	patterns [][]byte
	maxLen   int
	first    [256]bool // first bytes of patterns, to skip impossible match starts
}

// NewRedactor builds a redactor for values. Each line of a multi-line value is
// also redacted on its own, since programs often print one line at a time.
func NewRedactor(values ...string) (*Redactor, error) {
	r := &Redactor{}
	if err := r.add(values...); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Redactor) add(values ...string) error {
	seen := map[string]bool{}
	for _, p := range r.patterns {
		seen[string(p)] = true
	}
	for _, value := range values {
		candidates := []string{value}
		if strings.ContainsAny(value, "\r\n") {
			candidates = append(candidates, strings.FieldsFunc(value, func(c rune) bool { return c == '\n' || c == '\r' })...)
		}
		for _, candidate := range candidates {
			if len(candidate) < MinRedactBytes || seen[candidate] {
				continue
			}
			if len(r.patterns) >= MaxRedactPatterns {
				return ErrTooManyPatterns
			}
			seen[candidate] = true
			r.patterns = append(r.patterns, []byte(candidate))
			r.maxLen = max(r.maxLen, len(candidate))
			r.first[candidate[0]] = true
		}
	}
	// Longer patterns first, so the longest overlapping match wins the region.
	sort.SliceStable(r.patterns, func(i, j int) bool { return len(r.patterns[i]) > len(r.patterns[j]) })
	return nil
}

// Redact rewrites a complete buffer.
func (r *Redactor) Redact(data []byte) []byte {
	out, _ := r.redactUpTo(data, len(data))
	return out
}

// interval is a [start, end) byte range to replace.
type interval struct{ start, end int }

// matches returns merged intervals covering every pattern occurrence in data.
func (r *Redactor) matches(data []byte) []interval {
	var found []interval
	for _, pattern := range r.patterns {
		for offset := 0; offset <= len(data)-len(pattern); {
			i := bytes.Index(data[offset:], pattern)
			if i < 0 {
				break
			}
			found = append(found, interval{offset + i, offset + i + len(pattern)})
			offset += i + 1
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].start < found[j].start })
	var merged []interval
	for _, m := range found {
		// Merge only true overlaps. Adjacent occurrences stay separate, so output
		// is identical however the stream was split into writes.
		if n := len(merged); n > 0 && m.start < merged[n-1].end {
			merged[n-1].end = max(merged[n-1].end, m.end)
			continue
		}
		merged = append(merged, m)
	}
	return merged
}

// holdFrom returns the earliest position whose remainder could still grow into
// a pattern occurrence, or len(data) when none can. Output before it is final,
// so a stream that pauses mid-line is shown at once unless its tail is the
// beginning of a known value. Only the last maxLen-1 bytes can start such a tail.
func (r *Redactor) holdFrom(data []byte) int {
	for i := max(0, len(data)-(r.maxLen-1)); i < len(data); i++ {
		if !r.first[data[i]] {
			continue
		}
		rest := data[i:]
		for _, pattern := range r.patterns {
			if len(pattern) > len(rest) && bytes.HasPrefix(pattern, rest) {
				return i
			}
		}
	}
	return len(data)
}

// redactUpTo rewrites data[:boundary], moving the boundary left so no match that
// crosses it is split, and returns the rewritten prefix and the new boundary.
func (r *Redactor) redactUpTo(data []byte, boundary int) ([]byte, int) {
	merged := r.matches(data)
	for i := len(merged) - 1; i >= 0; i-- {
		if merged[i].start < boundary && merged[i].end > boundary {
			boundary = merged[i].start // Defer a match that is still arriving.
		}
	}
	out := make([]byte, 0, boundary)
	at := 0
	for _, m := range merged {
		if m.start >= boundary {
			break
		}
		out = append(out, data[at:m.start]...)
		out = append(out, Replacement...)
		at = m.end
	}
	if at < boundary {
		out = append(out, data[at:boundary]...)
	}
	return out, boundary
}

// Writer returns a WriteCloser that redacts everything written to it and writes
// the result to dst. It holds back only a tail that could be the start of a
// known value (at most one pattern's length), so a value split across writes is
// still caught while ordinary output passes through at once; Close flushes the
// remainder.
func (r *Redactor) Writer(dst io.Writer) *RedactingWriter {
	return &RedactingWriter{r: r, dst: dst}
}

// RedactingWriter is safe for concurrent use.
type RedactingWriter struct {
	mu      sync.Mutex
	r       *Redactor
	dst     io.Writer
	pending []byte
	closed  bool
}

// Write consumes p, emitting only output that can no longer be part of a match.
// It reports len(p) on success; the bytes written to dst may differ in length.
func (w *RedactingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, errors.New("write to closed redacting writer")
	}
	if len(w.r.patterns) == 0 {
		_, err := w.dst.Write(p)
		if err != nil {
			return 0, err
		}
		return len(p), nil
	}
	w.pending = append(w.pending, p...)
	// Any match that starts before this boundary is entirely visible already.
	boundary := w.r.holdFrom(w.pending)
	if boundary <= 0 {
		return len(p), nil
	}
	out, boundary := w.r.redactUpTo(w.pending, boundary)
	if len(out) > 0 {
		if _, err := w.dst.Write(out); err != nil {
			return 0, err
		}
	}
	w.pending = append(w.pending[:0], w.pending[boundary:]...)
	return len(p), nil
}

// Close redacts and writes whatever is still held back.
func (w *RedactingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	out := w.r.Redact(w.pending)
	clear(w.pending)
	w.pending = nil
	if len(out) == 0 {
		return nil
	}
	_, err := w.dst.Write(out)
	return err
}
