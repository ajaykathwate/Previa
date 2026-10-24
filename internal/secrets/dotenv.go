package secrets

import (
	"fmt"
	"regexp"
	"strings"
)

// Entry is one name and value an owner submits. The caller clears Value after
// use; it never appears in errors. Line is its 1-based line in pasted text.
type Entry struct {
	Name  string
	Value []byte
	Line  int
}

// Line is one line of a .env file as the Secrets page shows it with values
// hidden: a comment or blank line as written, or a name and whether it has a
// value. A value spanning several lines is one Line.
type Line struct {
	Comment  string // the line as written, for comments and blank lines
	Name     string // set for NAME=value lines
	HasValue bool
}

// span is one parsed entry and the lines [first, last] it occupies.
type span struct {
	entry       Entry
	first, last int
}

// scanDotenv reads .env text: one NAME=value per line, with blank lines and #
// comments, an optional "export " prefix, single-quoted literal values,
// double-quoted values with \n, \t, \" and \\ escapes that may span lines
// (PEM keys), and unquoted values that end at " #". Errors name the line,
// never its value.
func scanDotenv(text string) ([]string, []span, error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var spans []span
	for i := 0; i < len(lines); i++ {
		number := i + 1
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		name, raw, found := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if !found || name == "" {
			return nil, nil, &InvalidError{Field: "dotenv", Message: fmt.Sprintf("Line %d: expected NAME=value.", number), Entry: number}
		}
		raw = strings.TrimSpace(raw)
		first := i
		var value string
		switch {
		case strings.HasPrefix(raw, `"`):
			// A double-quoted value may continue on the following lines.
			body := raw[1:]
			for {
				if end := closingQuote(body); end >= 0 {
					value = unescape(body[:end])
					break
				}
				i++
				if i >= len(lines) {
					return nil, nil, &InvalidError{Field: "dotenv", Message: fmt.Sprintf("Line %d: the quoted value of %s is not closed.", number, name), Entry: number}
				}
				body += "\n" + strings.TrimRight(lines[i], " \t")
			}
		case strings.HasPrefix(raw, "'"):
			end := strings.LastIndex(raw, "'")
			if end == 0 {
				return nil, nil, &InvalidError{Field: "dotenv", Message: fmt.Sprintf("Line %d: the quoted value of %s is not closed.", number, name), Entry: number}
			}
			value = raw[1:end]
		default:
			if comment := strings.Index(raw, " #"); comment >= 0 {
				raw = strings.TrimSpace(raw[:comment])
			}
			value = raw
		}
		spans = append(spans, span{entry: Entry{Name: name, Value: []byte(value), Line: number}, first: first, last: i})
	}
	return lines, spans, nil
}

// ParseDotenv reads .env text into its entries, in order; see scanDotenv.
func ParseDotenv(text string) ([]Entry, error) {
	_, spans, err := scanDotenv(text)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, len(spans))
	for i, s := range spans {
		entries[i] = s.entry
	}
	return entries, nil
}

// MaskDotenv lists a .env file's lines with every value hidden.
func MaskDotenv(text string) ([]Line, error) {
	lines, spans, err := scanDotenv(text)
	if err != nil {
		return nil, err
	}
	var out []Line
	next := 0
	for i := 0; i < len(lines); i++ {
		if next < len(spans) && spans[next].first == i {
			s := spans[next]
			out = append(out, Line{Name: s.entry.Name, HasValue: len(s.entry.Value) > 0})
			clear(s.entry.Value)
			i = s.last
			next++
			continue
		}
		out = append(out, Line{Comment: strings.TrimRight(lines[i], " \t")})
	}
	// A trailing newline is not a blank line of its own.
	for len(out) > 0 && out[len(out)-1].Name == "" && out[len(out)-1].Comment == "" {
		out = out[:len(out)-1]
	}
	return out, nil
}

// setDotenv returns text with name set to value: its line is rewritten in
// place when it exists, and appended otherwise. Comments and order stay.
func setDotenv(text, name string, value []byte) (string, error) {
	lines, spans, err := scanDotenv(text)
	if err != nil {
		return "", err
	}
	formatted := FormatDotenvLine(name, value)
	for _, s := range spans {
		clear(s.entry.Value)
	}
	for _, s := range spans {
		if s.entry.Name == name {
			lines = append(lines[:s.first], append([]string{formatted}, lines[s.last+1:]...)...)
			return strings.Join(lines, "\n"), nil
		}
	}
	text = strings.TrimRight(text, "\r\n")
	if text != "" {
		text += "\n"
	}
	return text + formatted + "\n", nil
}

// plainValue is a value that needs no quotes in a .env file.
var plainValue = regexp.MustCompile(`^[A-Za-z0-9_./:@%+,=~^-]+$`)

// FormatDotenvLine writes one NAME=value line that scanDotenv reads back to
// name and value, quoting and escaping only when needed. Line breaks become
// \n so the value stays on one line; a carriage return is dropped.
func FormatDotenvLine(name string, value []byte) string {
	if len(value) == 0 || plainValue.Match(value) {
		return name + "=" + string(value)
	}
	var b strings.Builder
	b.WriteString(name + `="`)
	for _, c := range string(value) {
		switch c {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			// A .env line ending is \n; a lone \r is not representable.
		default:
			b.WriteRune(c)
		}
	}
	b.WriteString(`"`)
	return b.String()
}

// closingQuote finds the first unescaped double quote.
func closingQuote(s string) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

func unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
