package secrets

import (
	"bytes"
	"math/rand/v2"
	"strings"
	"testing"
)

func writeChunks(t *testing.T, r *Redactor, input string, sizes func(i int) int) string {
	t.Helper()
	var out bytes.Buffer
	w := r.Writer(&out)
	for i, at := 0, 0; at < len(input); i++ {
		n := min(sizes(i), len(input)-at)
		written, err := w.Write([]byte(input[at : at+n]))
		if err != nil || written != n {
			t.Fatalf("write: %d %v", written, err)
		}
		at += n
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestRedactingWriterCatchesValuesSplitAcrossWrites(t *testing.T) {
	secret := "sk_live_51Hx9QxLongSecretValue"
	token := "ghs_installationtoken0123456789"
	r, err := NewRedactor(secret, token)
	if err != nil {
		t.Fatal(err)
	}
	input := "start " + secret + " middle " + token + token + " and " + secret + "end"
	want := "start [redacted] middle [redacted][redacted] and [redacted]end"
	// Every possible split point of a two-write stream.
	for split := 0; split <= len(input); split++ {
		got := writeChunks(t, r, input, func(i int) int {
			if i == 0 {
				return max(split, 1)
			}
			return len(input)
		})
		if got != want {
			t.Fatalf("split at %d: %q", split, got)
		}
	}
	// One byte at a time, and random chunk sizes.
	if got := writeChunks(t, r, input, func(int) int { return 1 }); got != want {
		t.Fatalf("byte-by-byte: %q", got)
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for range 200 {
		if got := writeChunks(t, r, input, func(int) int { return 1 + rng.IntN(9) }); got != want {
			t.Fatalf("random chunks: %q", got)
		}
	}
}

func TestRedactorHandlesOverlapsLinesAndBounds(t *testing.T) {
	// Overlapping values: the union of the matched regions is removed.
	r, err := NewRedactor("abcdefgh", "efghijkl")
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range []int{1, 3, 5, 64} {
		got := writeChunks(t, r, "xx abcdefghijkl yy", func(int) int { return chunk })
		if got != "xx [redacted] yy" {
			t.Fatalf("overlap with chunk %d: %q", chunk, got)
		}
	}
	// A multi-line value is caught whole and line by line.
	pem := "-----BEGIN KEY-----\nMIIEvQIBADANBgkqhkiG\n-----END KEY-----"
	r, err = NewRedactor(pem)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(r.Redact([]byte("key=" + pem))); strings.Contains(got, "MIIEvQIBADANBgkqhkiG") {
		t.Fatalf("whole multi-line value leaked: %q", got)
	}
	if got := string(r.Redact([]byte("line MIIEvQIBADANBgkqhkiG printed alone"))); strings.Contains(got, "MIIEvQIBADANBgkqhkiG") {
		t.Fatalf("single line of a multi-line value leaked: %q", got)
	}
	// Values shorter than MinRedactBytes are not redacted (documented limit).
	r, err = NewRedactor("abc", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(r.Redact([]byte("abc abc"))); got != "abc abc" {
		t.Fatalf("short value handling changed: %q", got)
	}
	// With nothing to redact, the writer passes output straight through.
	if got := writeChunks(t, r, "plain output", func(int) int { return 4 }); got != "plain output" {
		t.Fatalf("pass-through: %q", got)
	}
	many := make([]string, MaxRedactPatterns+1)
	for i := range many {
		many[i] = strings.Repeat("v", 8) + strings.Repeat("x", i)
	}
	if _, err := NewRedactor(many...); err == nil {
		t.Fatal("pattern count must be bounded")
	}
}

func TestRedactingWriterHoldsBackOnlyAPossibleMatch(t *testing.T) {
	r, err := NewRedactor("topsecretvalue")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	w := r.Writer(&out)
	if _, err := w.Write([]byte("a long line of ordinary output that is safe to show now\ntopsec")); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "a long line of ordinary output") || strings.Contains(out.String(), "topsec") {
		t.Fatalf("writer must emit safe output promptly but hold back a partial match: %q", out.String())
	}
	if _, err := w.Write([]byte("retvalue done")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); strings.Contains(got, "topsecretvalue") || !strings.HasSuffix(got, "[redacted] done") {
		t.Fatalf("split value leaked: %q", got)
	}
	if _, err := w.Write([]byte("late")); err == nil {
		t.Fatal("writes after Close must fail")
	}
}

// Live logs depend on this: a stream that pauses mid-line must not hide its
// tail unless that tail could be the start of a known value.
func TestRedactingWriterEmitsEverythingThatCannotStartAValue(t *testing.T) {
	r, err := NewRedactor("topsecretvalue", "another-secret")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	w := r.Writer(&out)
	line := "Listening on :8080\n"
	if _, err := w.Write([]byte(line)); err != nil {
		t.Fatal(err)
	}
	if out.String() != line {
		t.Fatalf("output held back with no possible match: %q", out.String())
	}
	if _, err := w.Write([]byte("ready; token=top")); err != nil {
		t.Fatal(err)
	}
	if out.String() != line+"ready; token=" {
		t.Fatalf("only the possible start of a value may be held back: %q", out.String())
	}
	if _, err := w.Write([]byte("ic")); err != nil {
		t.Fatal(err)
	}
	if out.String() != line+"ready; token=topic" {
		t.Fatalf("a tail that can no longer match must be released: %q", out.String())
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}
