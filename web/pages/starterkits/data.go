// Package starterkits renders the Starter kits page: pick a stack and what it
// uses, and copy the Dockerfile and .previewrc.yaml the repository needs.
// The handler (internal/starterkits) generates the files; this package only
// shows them.
package starterkits

// Page is everything the page shows.
type Page struct {
	Stacks   []StackOption
	Form     Form
	Files    []File
	Notes    []string // what to check in the app itself
	Problems []Problem
	Offer    string   // which service versions this server offers
	Summary  []string // the kit at a glance: stack, services, setup, worker
	Status   string   // read aloud when the files change
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

// File is one generated file.
type File struct {
	Name     string // Dockerfile or .previewrc.yaml
	Kind     string // dockerfile or yaml, for highlighting
	Content  string
	Download string
	Purpose  string
}

// Problem is a choice to fix. Field names the form control it belongs to;
// Label names it for the list above the files ("" when it needs none).
type Problem struct {
	Field, Label, Message string
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
