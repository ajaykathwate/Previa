package starterkits

import (
	"context"
	"io"
	"regexp"
	"strings"

	"github.com/a-h/templ"
)

// Token is a run of text with a highlight class ("" for plain text).
type Token struct {
	Class, Text string
}

// Line is one highlighted line.
type Line []Token

var instructions = regexp.MustCompile(`^(FROM|RUN|COPY|ADD|ENV|ARG|WORKDIR|EXPOSE|CMD|ENTRYPOINT|USER|LABEL|HEALTHCHECK|SHELL|VOLUME|STOPSIGNAL|ONBUILD)\b`)

// Highlight splits a generated file into lines of tokens. It only colours;
// the text is rendered escaped and copied verbatim from the file itself.
func Highlight(kind, content string) []Line {
	raw := strings.Split(strings.TrimRight(content, "\n"), "\n")
	out := make([]Line, 0, len(raw))
	heredoc := false
	for _, line := range raw {
		switch {
		case kind == "yaml":
			out = append(out, yamlLine(line))
		case heredoc:
			heredoc = strings.TrimSpace(line) != "EOF"
			out = append(out, Line{{Class: "tk-here", Text: line}})
		default:
			heredoc = strings.Contains(line, "<<'EOF'")
			out = append(out, dockerLine(line))
		}
	}
	return out
}

func dockerLine(line string) Line {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "#") {
		return Line{{Class: "tk-cm", Text: line}}
	}
	if m := instructions.FindString(line); m != "" {
		return append(Line{{Class: "tk-kw", Text: m}}, quoted(line[len(m):])...)
	}
	return quoted(line)
}

// quoted marks double-quoted strings.
func quoted(text string) Line {
	var out Line
	for text != "" {
		start := strings.IndexByte(text, '"')
		if start < 0 {
			return append(out, Token{Text: text})
		}
		end := closingQuote(text, start)
		if start > 0 {
			out = append(out, Token{Text: text[:start]})
		}
		out = append(out, Token{Class: "tk-str", Text: text[start : end+1]})
		text = text[end+1:]
	}
	return out
}

func closingQuote(text string, start int) int {
	for i := start + 1; i < len(text); i++ {
		switch text[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return len(text) - 1
}

var yamlKey = regexp.MustCompile(`^(\s*)(- )?([a-z_]+)(:)`)

func yamlLine(line string) Line {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "#") {
		return Line{{Class: "tk-cm", Text: line}}
	}
	body, comment := line, ""
	if i := strings.Index(line, "  # "); i >= 0 && !strings.Contains(line[:i], `"`) {
		body, comment = line[:i], line[i:]
	}
	var out Line
	if m := yamlKey.FindStringSubmatch(body); m != nil {
		out = append(out, Token{Text: m[1]})
		if m[2] != "" {
			out = append(out, Token{Class: "tk-punct", Text: m[2]})
		}
		out = append(out, Token{Class: "tk-key", Text: m[3]}, Token{Class: "tk-punct", Text: m[4]})
		body = body[len(m[0]):]
	}
	for _, t := range quoted(body) {
		if t.Class == "" && strings.TrimSpace(t.Text) != "" && strings.Trim(strings.TrimSpace(t.Text), "0123456789.") == "" {
			t.Class = "tk-num"
		}
		out = append(out, t)
	}
	if comment != "" {
		out = append(out, Token{Class: "tk-cm", Text: comment})
	}
	return out
}

// code writes the lines as escaped, highlighted text joined by newlines. It
// is Go rather than a template so no template whitespace reaches the file a
// reader selects and copies.
func code(lines []Line) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		var b strings.Builder
		for i, line := range lines {
			if i > 0 {
				b.WriteByte('\n')
			}
			for _, t := range line {
				text := templ.EscapeString(t.Text)
				if t.Class == "" {
					b.WriteString(text)
					continue
				}
				b.WriteString(`<span class="` + t.Class + `">` + text + `</span>`) // classes are the tk-* constants
			}
		}
		_, err := io.WriteString(w, b.String())
		return err
	})
}
