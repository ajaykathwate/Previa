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
		{"API_URL", []byte("https://api.test"), 3},
		{"PLAIN", []byte("spaced value"), 4},
		{"QUOTED", []byte("a \"quoted\" line\nwith a break"), 5},
		{"LITERAL", []byte(`keeps # and \n as typed`), 6},
		{"PRIVATE_KEY", []byte("-----BEGIN KEY-----\nabc\n-----END KEY-----"), 7},
		{"EMPTY", []byte(""), 10},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Name != want[i].Name || string(got[i].Value) != string(want[i].Value) || got[i].Line != want[i].Line {
			t.Errorf("entry %d = %s=%q (line %d), want %s=%q (line %d)", i, got[i].Name, got[i].Value, got[i].Line, want[i].Name, want[i].Value, want[i].Line)
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

func TestMaskDotenvKeepsCommentsAndHidesValues(t *testing.T) {
	text := "# Payments\n\nexport STRIPE_KEY=sk_test_123 # sandbox\nPEM=\"-----BEGIN\nabc\n-----END\"\n  # indented note\nUNSET=\n"
	lines, err := MaskDotenv(text)
	if err != nil {
		t.Fatal(err)
	}
	want := []Line{{Comment: "# Payments"}, {}, {Name: "STRIPE_KEY", HasValue: true}, {Name: "PEM", HasValue: true}, {Comment: "  # indented note"}, {Name: "UNSET"}}
	if len(lines) != len(want) {
		t.Fatalf("lines = %+v, want %+v", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d = %+v, want %+v", i, lines[i], want[i])
		}
	}
	for _, line := range lines {
		if strings.Contains(line.Comment, "sk_test") || strings.Contains(line.Comment, "BEGIN") {
			t.Fatalf("a value leaked into the masked view: %+v", line)
		}
	}
}

func TestFormatDotenvLineReadsBackExactly(t *testing.T) {
	for _, value := range []string{"plain-value_1.2:3@x/y", "has space", `quote " and \ backslash`, "tab\there", "line\nbreak  \nkept", "# not a comment", "ends with #", "'single'", "=leading"} {
		entries, err := ParseDotenv(FormatDotenvLine("NAME", []byte(value)))
		if err != nil || len(entries) != 1 || string(entries[0].Value) != value {
			t.Errorf("%q: read back %+v, %v", value, entries, err)
		}
	}
}

func TestSetDotenvRewritesInPlaceOrAppends(t *testing.T) {
	text := "# top\nA=1\nPEM=\"x\ny\"\n# keep me\n"
	got, err := setDotenv(text, "PEM", []byte("new"))
	if err != nil || got != "# top\nA=1\nPEM=new\n# keep me\n" {
		t.Fatalf("replace: %q %v", got, err)
	}
	got, err = setDotenv(text, "B", []byte("two words"))
	if err != nil || got != "# top\nA=1\nPEM=\"x\ny\"\n# keep me\nB=\"two words\"\n" {
		t.Fatalf("append: %q %v", got, err)
	}
}
