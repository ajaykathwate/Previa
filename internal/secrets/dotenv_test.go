package secrets

import (
	"errors"
	"strings"
	"testing"
)

func TestParseDotenv(t *testing.T) {
	text := strings.Join([]string{
		"# a comment",
		"",
		"export API_URL=https://api.test",
		"PLAIN = spaced value # trailing note",
		`QUOTED="a \"quoted\" line\nwith a break"`,
		`LITERAL='keeps # and \n as typed'`,
		`PRIVATE_KEY="-----BEGIN KEY-----`,
		"abc",
		`-----END KEY-----"`,
		"EMPTY=",
	}, "\r\n")
	got, err := ParseDotenv(text)
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{
		{"API_URL", []byte("https://api.test")},
		{"PLAIN", []byte("spaced value")},
		{"QUOTED", []byte("a \"quoted\" line\nwith a break")},
		{"LITERAL", []byte(`keeps # and \n as typed`)},
		{"PRIVATE_KEY", []byte("-----BEGIN KEY-----\nabc\n-----END KEY-----")},
		{"EMPTY", []byte("")},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Name != want[i].Name || string(got[i].Value) != string(want[i].Value) {
			t.Errorf("entry %d = %s=%q, want %s=%q", i, got[i].Name, got[i].Value, want[i].Name, want[i].Value)
		}
	}
}

func TestParseDotenvErrorsNameTheLineNotTheValue(t *testing.T) {
	for text, line := range map[string]string{
		"GOOD=1\nno equals sign here":  "Line 2:",
		"=value-only":                  "Line 1:",
		"A=\"never closed\nstill open": "Line 1:",
		"B='unclosed":                  "Line 1:",
	} {
		_, err := ParseDotenv(text)
		var invalid *InvalidError
		if !errors.As(err, &invalid) || !strings.HasPrefix(invalid.Message, line) {
			t.Errorf("%q: error %v, want one starting %q", text, err, line)
		}
		if invalid != nil && (strings.Contains(invalid.Message, "never closed") || strings.Contains(invalid.Message, "value-only")) {
			t.Errorf("%q: the error repeats a value: %s", text, invalid.Message)
		}
	}
}
