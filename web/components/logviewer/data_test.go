package logviewer

import (
	"strings"
	"testing"
	"time"
)

func TestEventTimestampPreservesApplicationDatesAndIndentation(t *testing.T) {
	at := time.Date(2026, 10, 13, 12, 1, 2, 345000000, time.FixedZone("offset", 3600))
	cases := []struct{ text, timestamp, message string }{
		{"\t  indented stack frame", "2026-10-13T11:01:02.345Z", "\t  indented stack frame"},
		{"2026/10/12 15:22:34   indented", "2026/10/12 15:22:34", "  indented"},
		{"2026/10/12 15:22:34 [notice] nginx", "2026/10/12 15:22:34", "[notice] nginx"},
		{"[2026-10-12T15:22:34.123+05:30] ready", "[2026-10-12T15:22:34.123+05:30]", "ready"},
		{"HTTP request at 2026/10/12 15:22:34", "2026-10-13T11:01:02.345Z", "HTTP request at 2026/10/12 15:22:34"},
	}
	for _, c := range cases {
		t.Run(c.text, func(t *testing.T) {
			l := Line{Text: c.text, At: at}
			if l.Timestamp() != c.timestamp || l.Message() != c.message {
				t.Fatalf("got %q %q", l.Timestamp(), l.Message())
			}
		})
	}
	if got := (Line{Text: "legacy"}).Timestamp(); got != "[timestamp unavailable]" {
		t.Fatalf("invented time: %q", got)
	}
}
func TestFilterURLPreservesLiteralSearch(t *testing.T) {
	got := FilterURL("/logs", "previa", "a&b + 50%")
	if !strings.Contains(got, "source=previa") || !strings.Contains(got, "q=a%26b+%2B+50%25") {
		t.Fatal(got)
	}
}
