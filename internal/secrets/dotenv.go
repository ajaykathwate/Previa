package secrets

import (
	"fmt"
	"strings"
)

// Entry is one name and value an owner submits. The caller clears Value after
// use; it never appears in errors.
type Entry struct {
	Name  string
	Value []byte
}

// ParseDotenv reads pasted .env text: one NAME=value per line, with blank
// lines and # comments ignored, an optional "export " prefix, single-quoted
// literal values, double-quoted values with \n, \" and \\ escapes that may
// span lines (PEM keys), and unquoted values that end at " #". Errors name the
// line, never its value.
func ParseDotenv(text string) ([]Entry, error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var entries []Entry
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
			return nil, &InvalidError{Field: "dotenv", Message: fmt.Sprintf("Line %d: expected NAME=value.", number)}
		}
		raw = strings.TrimSpace(raw)
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
					return nil, &InvalidError{Field: "dotenv", Message: fmt.Sprintf("Line %d: the quoted value of %s is not closed.", number, name)}
				}
				body += "\n" + strings.TrimRight(lines[i], " \t")
			}
		case strings.HasPrefix(raw, "'"):
			end := strings.LastIndex(raw, "'")
			if end == 0 {
				return nil, &InvalidError{Field: "dotenv", Message: fmt.Sprintf("Line %d: the quoted value of %s is not closed.", number, name)}
			}
			value = raw[1:end]
		default:
			if comment := strings.Index(raw, " #"); comment >= 0 {
				raw = strings.TrimSpace(raw[:comment])
			}
			value = raw
		}
		entries = append(entries, Entry{Name: name, Value: []byte(value)})
	}
	return entries, nil
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
