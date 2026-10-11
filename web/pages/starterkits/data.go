// Package starterkits renders the Starter kits page: pick a stack and what it
// uses, and copy the Dockerfile and .previewrc.yaml the repository needs.
// The handler (internal/starterkits) generates the files; this package only
// shows them.
package starterkits

import "strings"

// Page is everything the page shows.
type Page struct {
	Stacks   []StackOption
	Form     Form
	Files    []File
	Notes    []string // what to check in the app itself
	Problems []Problem
	Offer    string // which service versions this server offers
	Status   string // read aloud when the files change
	CSRF     string // for the download forms and the check of an edited file
}

// Selected is the chosen stack.
func (p Page) Selected() StackOption {
	for _, s := range p.Stacks {
		if s.Selected {
			return s
		}
	}
	return StackOption{}
}

// StackOption is one stack card.
type StackOption struct {
	ID, Name, Kind, Language string
	Selected                 bool
}

// Form holds the current choices.
type Form struct {
	Stack, StackName string
	Static, OwnFile  bool
	Postgres, Redis  Service
	Setup, Worker    Command
	Docs             bool
	DocsPath         string
	Port             int
	Health           string
	Tier             string
	Tiers            []string
	Detail           *Detail
}

// Service is PostgreSQL or Redis.
type Service struct {
	On       bool
	Version  string
	Versions []string // offered; empty when the server offers none
}

// Command is setup or the background worker.
type Command struct {
	Available   bool // the stack can run one
	On          bool
	With        string // a preset value, or "custom"
	Options     []Option
	Text        string // the user's own command
	Placeholder string
}

// Custom reports whether the user writes their own command.
func (c Command) Custom() bool { return c.With == "custom" }

// Option is a select option.
type Option struct {
	Value, Label string
}

// Detail is the stack's own value, such as the Django project package.
type Detail struct {
	Label, Hint, Value string
	Options            []string
}

// File is one generated file, an editor tab.
type File struct {
	ID      string // dockerfile or previewrc: its tab, panel and download path
	Name    string // Dockerfile or .previewrc.yaml
	Kind    string // dockerfile or yaml, for highlighting
	Content string
	Purpose string
}

// Lines counts the file's lines as an editor numbers them, the empty line
// after a final newline included.
func (f File) Lines() int { return strings.Count(f.Content, "\n") + 1 }

// Problem is a choice or a line to fix. Field names the form control it
// belongs to; Label names it in the list under the files ("" when it needs
// none); Line is set for a problem in an edited file.
type Problem struct {
	Field, Label, Message string
	Line                  int
}

// ProblemFor returns the message for one field, if any.
func (p Page) ProblemFor(field string) string {
	for _, problem := range p.Problems {
		if problem.Field == field {
			return problem.Message
		}
	}
	return ""
}

// Monogram is a stack's two-letter mark on its card.
func Monogram(id string) string {
	switch id {
	case "nextjs":
		return "Nx"
	case "vite":
		return "Vi"
	case "node-api":
		return "JS"
	case "django":
		return "Dj"
	case "fastapi":
		return "Fa"
	case "rails":
		return "Rb"
	case "go":
		return "Go"
	case "spring":
		return "Sp"
	case "laravel":
		return "La"
	}
	return "+"
}
