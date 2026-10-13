package opensource

import (
	"context"
	"fmt"
	"html"
	"io"
	"strconv"
	"strings"

	"github.com/a-h/templ"
)

// inline renders copy in which `backticks` mark code. Everything is escaped;
// only the <code> elements are markup.
func inline(text string) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		var b strings.Builder
		for i, part := range strings.Split(text, "`") {
			if i%2 == 1 {
				b.WriteString("<code>")
				b.WriteString(html.EscapeString(part))
				b.WriteString("</code>")
				continue
			}
			b.WriteString(html.EscapeString(part))
		}
		_, err := io.WriteString(w, b.String())
		return err
	})
}

// Title is the repository's name without its owner.
func (r Repo) Title() string {
	_, name, _ := strings.Cut(r.Name, "/")
	return name
}

// Label is the repository's name in compact lists.
func (r Repo) Label() string {
	if r.Short != "" {
		return r.Short
	}
	return r.Title()
}

// Owner is the repository's owner on GitHub.
func (r Repo) Owner() string {
	owner, _, _ := strings.Cut(r.Name, "/")
	return owner
}

// StackLine is the first few stack entries, for compact listings.
func (r Repo) StackLine() string {
	const shown = 3
	if len(r.Stack) <= shown {
		return strings.Join(r.Stack, " · ")
	}
	return strings.Join(r.Stack[:shown], " · ") + " · …"
}

// planSummary describes how an app is tested in a few words, for its disclosure.
func planSummary(r Repo) string {
	parts := []string{"Config"}
	if n := len(r.Secrets); n > 0 {
		parts = append(parts, plural(n, "secret"))
	}
	if n := len(r.Changes); n > 0 {
		parts = append(parts, plural(n, "change"))
	}
	return strings.Join(append(parts, plural(len(r.Steps), "step")), " · ")
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// coreLead introduces the core set with its coverage, counted from the data.
func coreLead() string {
	covered := map[string]bool{}
	for _, r := range Core() {
		for _, c := range r.Covers {
			covered[c] = true
		}
	}
	return fmt.Sprintf("Static sites, server-rendered apps and APIs in TypeScript, Go, Python, Ruby, Java and PHP. Between them they exercise %d of the %d capabilities above, from the first build to cleanup.",
		len(covered), len(Capabilities()))
}
