package buildlogs

import (
	"bytes"
	"strings"
	"testing"
)

func strip(t *testing.T, chunks ...string) string {
	t.Helper()
	var out bytes.Buffer
	s := &controlStripper{dst: &out}
	for _, c := range chunks {
		if _, err := s.Write([]byte(c)); err != nil {
			t.Fatal(err)
		}
	}
	return out.String()
}

func TestControlStripperRemovesTerminalSequences(t *testing.T) {
	for _, tt := range []struct {
		name   string
		chunks []string
		want   string
	}{
		{"colours", []string{"\x1b[1;31merror\x1b[0m: failed\n"}, "error: failed\n"},
		{"split across writes", []string{"ok \x1b[3", "2mgreen\x1b", "[0m\n"}, "ok green\n"},
		{"window title", []string{"\x1b]0;my title\x07visible\n"}, "visible\n"},
		{"string terminator", []string{"\x1b]8;;https://x\x1b\\link\x1b]8;;\x1b\\\n"}, "link\n"},
		{"charset designation", []string{"\x1b(Bplain\n"}, "plain\n"},
		{"controls and delete", []string{"a\x00b\x07c\x7fd\te\r\n"}, "abcd\te\r\n"},
		{"unterminated title cannot hide later lines", []string{"\x1b]0;never closed\nnext line\n"}, "\nnext line\n"},
		{"control aborts a sequence", []string{"\x1b[12\nkept\n"}, "\nkept\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := strip(t, tt.chunks...); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
	long := "\x1b]0;" + strings.Repeat("x", maxEscapeString+10) + "after"
	if got := strip(t, long); !strings.HasSuffix(got, "after") {
		t.Fatalf("an endless escape string must be abandoned: %q", got)
	}
}

func split(maxBytes int, chunks ...string) (lines []string, partial func() []string) {
	s := &lineSplitter{maxBytes: maxBytes}
	s.emit = func(text string) { lines = append(lines, text) }
	for _, c := range chunks {
		_, _ = s.Write([]byte(c))
	}
	return lines, func() []string { s.flush(); return lines }
}

func TestLineSplitterBoundsAndNormalizesLines(t *testing.T) {
	lines, _ := split(16, "one\r\ntwo\nthr", "ee\n")
	if strings.Join(lines, "|") != "one|two|three" {
		t.Fatalf("lines: %q", lines)
	}
	// A lone carriage return overwrites the line, as a terminal shows progress.
	lines, _ = split(64, "10%\r50%\r100%\ndone\n")
	if strings.Join(lines, "|") != "100%|done" {
		t.Fatalf("progress: %q", lines)
	}
	// CRLF split across writes is still one line break.
	lines, _ = split(64, "a\r", "\nb\n")
	if strings.Join(lines, "|") != "a|b" {
		t.Fatalf("split CRLF: %q", lines)
	}
	lines, _ = split(8, strings.Repeat("x", 20)+"\nshort\n")
	if len(lines) != 2 || lines[0] != "xxxxxxxx … [line truncated: 12 more bytes not stored]" || lines[1] != "short" {
		t.Fatalf("long line: %q", lines)
	}
	lines, _ = split(64, "bad \xff\xfe bytes\n")
	if lines[0] != "bad \uFFFD bytes" { // A run of invalid bytes becomes one replacement.
		t.Fatalf("invalid UTF-8: %q", lines[0])
	}
	_, flush := split(64, "no newline yet")
	if got := flush(); len(got) != 1 || got[0] != "no newline yet" {
		t.Fatalf("partial line: %q", got)
	}
	_, flush = split(64, "")
	if got := flush(); len(got) != 0 {
		t.Fatalf("empty flush emitted %q", got)
	}
}
