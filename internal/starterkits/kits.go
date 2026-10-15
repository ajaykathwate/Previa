// Package starterkits writes the two files a repository needs for Previa
// previews, a Dockerfile and .previewrc.yaml, for a chosen stack and the
// services it uses. Nothing is stored: every kit is generated from the
// request's choices, and the handler checks the result with the same rules
// a build applies.
package starterkits

import (
	"regexp"
	"slices"
	"strings"
	"time"
)

// Stack is a framework Previa has a starter kit for.
type Stack struct {
	ID, Name string
	Kind     string // Full-stack, Frontend, Backend API or Any stack
	Language string // shown under the name
	Port     int
	Health   string // default health_check_path; "" waits for the port instead
	Docs     string // suggested docs_path; "" when the stack has no usual one
	DocsOn   bool   // docs are turned on in a fresh kit
	Static   bool   // a static site: no services, setup or processes
	OwnFile  bool   // the repository keeps its own Dockerfile; only .previewrc.yaml is written
	NoShell  bool   // the final image has no shell, so commands are never run by sh
	Detail   *Detail
	Setup    []Preset
	Workers  []Preset
	// Wrapper prefixes setup and process commands, which replace the image's
	// entrypoint, so they get the same environment mapping as the app.
	Wrapper    []string
	dockerfile func(Choices) string
	notes      func(Choices) []string
}

// Detail is a stack-specific value the files need, such as the Django project.
type Detail struct {
	Label, Hint, Default string
	Options              []string       // a fixed choice; empty means free text
	Pattern              *regexp.Regexp // the text it accepts
}

// Preset is a ready-made command for setup or a process.
type Preset struct {
	Key, Label string
	Name       string // the process name, for workers
	Command    func(Choices) []string
}

// Custom is the preset key for a command the user writes.
const Custom = "custom"

// Tiers are the preview sizes .previewrc.yaml accepts.
var Tiers = []string{"small", "standard", "large"}

// Choices are what the user picked. Normalize fills the stack's defaults.
type Choices struct {
	Stack         string
	Postgres      string // a version; empty when not used
	Redis         string
	MySQL         string
	Setup         bool
	SetupWith     string // a preset key or Custom
	SetupCommand  string
	Worker        bool
	WorkerWith    string
	WorkerCommand string
	Docs          bool
	DocsPath      string
	Port          int
	Health        string
	Tier          string
	Detail        string
}

// Offer is the service versions this server offers, newest first.
type Offer struct {
	Postgres, Redis, MySQL []string
	BuildTimeout           time.Duration
	// Known: read from a running worker. Otherwise these are Previa's
	// defaults, which a worker may narrow.
	Known bool
}

// Problem is something to fix before the files will work.
type Problem struct {
	Field, Message string
	Line           int // in an edited .previewrc.yaml; 0 for a choice
}

// Kit is a generated starter kit.
type Kit struct {
	Stack      Stack
	Choices    Choices
	Dockerfile string // empty when the repository keeps its own
	Config     string // .previewrc.yaml
	Notes      []string
	Problems   []Problem
}

// Stacks lists every starter kit, in the order the page shows them.
func Stacks() []Stack { return slices.Clone(catalog) }

// Find returns the stack with this ID, or the first one.
func Find(id string) Stack {
	for _, s := range catalog {
		if s.ID == id {
			return s
		}
	}
	return catalog[0]
}

// Fresh is a new kit's choices for a stack: its usual setup and docs, no
// services and the standard size.
func Fresh(id string) Choices {
	s := Find(id)
	c := Choices{Stack: s.ID, Port: s.Port, Health: s.Health, Tier: "standard", Docs: s.DocsOn, DocsPath: s.Docs}
	if len(s.Setup) > 0 && !s.Static {
		c.Setup, c.SetupWith = true, s.Setup[0].Key
	}
	if s.Detail != nil {
		c.Detail = s.Detail.Default
	}
	return c
}

// Generate writes the kit for these choices. Problems explain any choice
// that cannot work; the files are still written so the page can show them.
func Generate(c Choices, offer Offer) Kit {
	s := Find(c.Stack)
	c, problems := normalize(s, c, offer)
	kit := Kit{Stack: s, Choices: c, Problems: problems}
	setup, problem := commandFor(s, c.Setup, c.SetupWith, c.SetupCommand, s.Setup, c)
	if problem != "" {
		kit.Problems = append(kit.Problems, Problem{Field: "setup", Message: problem})
	}
	worker, problem := commandFor(s, c.Worker, c.WorkerWith, c.WorkerCommand, s.Workers, c)
	if problem != "" {
		kit.Problems = append(kit.Problems, Problem{Field: "worker", Message: problem})
	}
	if !s.OwnFile {
		kit.Dockerfile = s.dockerfile(c)
	}
	kit.Config = renderConfig(s, c, setup, worker, workerName(s, c))
	kit.Notes = notesFor(s, c)
	return kit
}

// normalize fills defaults and corrects choices that do not apply to the
// stack, reporting values that cannot be used.
func normalize(s Stack, c Choices, offer Offer) (Choices, []Problem) {
	c.Stack = s.ID
	var problems []Problem
	if s.Static {
		c.Postgres, c.Redis, c.MySQL, c.Setup, c.Worker = "", "", "", false, false
	}
	c.Postgres, problems = serviceVersion("postgres", "PostgreSQL", c.Postgres, offer.Postgres, problems)
	c.Redis, problems = serviceVersion("redis", "Redis", c.Redis, offer.Redis, problems)
	c.MySQL, problems = serviceVersion("mysql", "MySQL", c.MySQL, offer.MySQL, problems)
	c.SetupWith = presetKey(c.SetupWith, s.Setup)
	c.WorkerWith = presetKey(c.WorkerWith, s.Workers)
	if c.Port == 0 {
		c.Port = s.Port
	}
	if c.Port < 1 || c.Port > 65535 {
		problems = append(problems, Problem{Field: "port", Message: "Use a port from 1 to 65535."})
		c.Port = s.Port
	}
	c.Health = strings.TrimSpace(c.Health)
	if !validPath(c.Health) {
		problems = append(problems, Problem{Field: "health", Message: "Use a path that starts with /, such as /health, without spaces, ? or #."})
		c.Health = s.Health
	}
	c.DocsPath = strings.TrimSpace(c.DocsPath)
	if c.Docs && c.DocsPath == "" {
		c.DocsPath = cmpOr(s.Docs, "/docs")
	}
	if !validPath(c.DocsPath) {
		problems = append(problems, Problem{Field: "docs", Message: "Use a path on the preview that starts with /, such as /docs."})
		c.DocsPath = cmpOr(s.Docs, "/docs")
	}
	if !slices.Contains(Tiers, c.Tier) {
		c.Tier = "standard"
	}
	c.Detail, problems = detailValue(s.Detail, c.Detail, problems)
	return c, problems
}

func serviceVersion(field, name, version string, offered []string, problems []Problem) (string, []Problem) {
	switch {
	case version == "":
		return "", problems
	case len(offered) == 0:
		return "", append(problems, Problem{Field: field, Message: "This server offers no " + name + " right now, so previews that ask for it won't start."})
	case !slices.Contains(offered, version):
		return offered[0], problems
	}
	return version, problems
}

func presetKey(key string, presets []Preset) string {
	if key == Custom {
		return key
	}
	for _, p := range presets {
		if p.Key == key {
			return key
		}
	}
	if len(presets) > 0 {
		return presets[0].Key
	}
	return Custom
}

func detailValue(d *Detail, value string, problems []Problem) (string, []Problem) {
	if d == nil {
		return "", problems
	}
	value = strings.TrimSpace(value)
	switch {
	case value == "":
		return d.Default, problems
	case len(d.Options) > 0 && !slices.Contains(d.Options, value):
		return d.Default, problems
	case len(d.Options) == 0 && !d.Pattern.MatchString(value):
		return d.Default, append(problems, Problem{Field: "detail", Message: d.Hint})
	}
	return value, problems
}

// validPath accepts an empty path or an absolute HTTP path a preview can
// answer: Previa's own rule, checked again when the config is validated.
var pathText = regexp.MustCompile(`^/[A-Za-z0-9._~!$&'()*+,;=:@%/-]*$`)

func validPath(p string) bool {
	return p == "" || (pathText.MatchString(p) && !strings.HasPrefix(p, "//") && !slices.Contains(strings.Split(p, "/"), ".."))
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// shellSyntax is text only a shell understands; such commands run through
// sh -c, while plain ones are split into a program and its arguments.
var shellSyntax = regexp.MustCompile("[|&;<>$*?(){}\\[\\]\"'\\\\`~]")

// commandFor resolves setup or a process to argv: a preset's command, or the
// user's own, prefixed by the stack's wrapper.
func commandFor(s Stack, on bool, with, text string, presets []Preset, c Choices) ([]string, string) {
	if !on || s.Static {
		return nil, ""
	}
	for _, p := range presets {
		if p.Key == with {
			return p.Command(c), ""
		}
	}
	text = strings.TrimSpace(text)
	switch {
	case text == "":
		return nil, "Write the command to run, or pick a ready-made one."
	case len(text) > 1000:
		return nil, "Keep the command under 1,000 characters."
	case shellSyntax.MatchString(text) && s.NoShell:
		return nil, "This image has no shell: write one program and its arguments, without quotes, pipes or &&."
	case shellSyntax.MatchString(text):
		return append(slices.Clone(s.Wrapper), "sh", "-c", text), ""
	}
	return append(slices.Clone(s.Wrapper), strings.Fields(text)...), ""
}

func workerName(s Stack, c Choices) string {
	for _, p := range s.Workers {
		if p.Key == c.WorkerWith && p.Name != "" {
			return p.Name
		}
	}
	return "worker"
}
