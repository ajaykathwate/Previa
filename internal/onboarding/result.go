package onboarding

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/ajaykathwatee/previa/internal/repoconfig"
)

// What a check found about .previewrc.yaml at the commit.
const (
	ConfigFound   = "found"
	ConfigMissing = "missing"
	ConfigInvalid = "invalid"
)

// Where the build context and Dockerfile stand. Unverified means a build
// decides (a link GitHub's API does not resolve, a file too large to inspect).
const (
	PathFound      = "found"
	PathMissing    = "missing"
	PathNotFile    = "not_file"
	PathNotDir     = "not_directory"
	PathUnverified = "unverified"
)

// Result statuses: whether the commit is ready to build as far as static
// checks can tell.
const (
	StatusPassed   = "passed"
	StatusProblems = "problems"
)

// maxShownArgs bounds a command kept for display; the parser already limits
// commands to 64 arguments and 4 KiB.
const maxShownArgs = 64

// Result is what one setup check found at one commit. It is stored as JSON
// and holds only safe facts: names, paths, numbers and the parser's own
// messages, never configuration values or file contents.
type Result struct {
	Status          string           `json:"status"`
	Config          string           `json:"config"`
	Context         string           `json:"context,omitempty"`
	Dockerfile      string           `json:"dockerfile,omitempty"`
	ContextState    string           `json:"context_state,omitempty"`
	DockerfileState string           `json:"dockerfile_state,omitempty"`
	RootDockerfile  bool             `json:"root_dockerfile,omitempty"` // no config, but ./Dockerfile exists
	Submodules      bool             `json:"submodules,omitempty"`      // a submodule makes the build fail
	Port            int              `json:"port,omitempty"`
	HealthCheckPath string           `json:"health_check_path,omitempty"`
	DocsPath        string           `json:"docs_path,omitempty"`
	ResourceTier    string           `json:"resource_tier,omitempty"`
	BuildTimeout    string           `json:"build_timeout,omitempty"`
	Services        []ManagedService `json:"services,omitempty"`
	Setup           []string         `json:"setup,omitempty"`
	Processes       []Process        `json:"processes,omitempty"`
	EnvNames        []string         `json:"env_names,omitempty"` // names only: YAML values may be sensitive
	Issues          []Issue          `json:"issues,omitempty"`
	Notices         []Issue          `json:"notices,omitempty"`
}

// ManagedService is a managed service the commit asks for.
type ManagedService struct {
	Kind    string `json:"kind"`
	Version string `json:"version"`
}

// Process is an unrouted command the preview runs next to its app.
type Process struct {
	Name    string   `json:"name"`
	Command []string `json:"command"`
}

// Issue is one parser finding: the key, a code, the parser's message and,
// where known, its line.
type Issue struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Line    int    `json:"line,omitempty"`
}

// Passed reports whether nothing found blocks a build.
func (r Result) Passed() bool { return r.Status == StatusPassed }

// finish decides the status from what was found.
func (r Result) finish() Result {
	r.Status = StatusPassed
	if r.Config != ConfigFound || len(r.Issues) > 0 || r.Submodules ||
		r.ContextState == PathMissing || r.ContextState == PathNotDir ||
		r.DockerfileState == PathMissing || r.DockerfileState == PathNotFile {
		r.Status = StatusProblems
	}
	return r
}

// fromConfig keeps the parsed configuration's displayable facts.
func fromConfig(parsed repoconfig.Result) Result {
	cfg := parsed.Config
	r := Result{Config: ConfigFound, Context: cfg.Context, Dockerfile: cfg.Dockerfile, Port: cfg.Port,
		HealthCheckPath: cfg.HealthCheckPath, DocsPath: cfg.DocsPath, ResourceTier: cfg.ResourceTier, Setup: shown(cfg.Setup)}
	if cfg.BuildTimeout > 0 {
		r.BuildTimeout = cfg.BuildTimeout.String()
	}
	for _, s := range cfg.Services {
		r.Services = append(r.Services, ManagedService{Kind: s.Kind, Version: s.Version})
	}
	for _, p := range cfg.Processes {
		r.Processes = append(r.Processes, Process{Name: p.Name, Command: shown(p.Command)})
	}
	for _, entry := range cfg.Env {
		if name, _, ok := strings.Cut(entry, "="); ok {
			r.EnvNames = append(r.EnvNames, name)
		}
	}
	slices.Sort(r.EnvNames)
	for _, n := range parsed.Notices {
		r.Notices = append(r.Notices, Issue{Field: n.Field, Code: n.Code, Message: n.Message})
	}
	return r
}

func shown(argv []string) []string {
	if len(argv) == 0 {
		return nil
	}
	return slices.Clone(argv[:min(len(argv), maxShownArgs)])
}

// invalidConfig is a result for a configuration the parser rejected.
func invalidConfig(issues []repoconfig.Issue) Result {
	r := Result{Config: ConfigInvalid}
	for _, i := range issues {
		r.Issues = append(r.Issues, Issue{Field: i.Field, Code: i.Code, Message: i.Message, Line: i.Line})
	}
	return r.finish()
}

func encodeResult(r Result) ([]byte, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("encode setup check result: %w", err)
	}
	return data, nil
}

// decodeResult reads a stored result; an unreadable one is treated as absent.
func decodeResult(data []byte) (Result, bool) {
	if len(data) == 0 {
		return Result{}, false
	}
	var r Result
	if err := json.Unmarshal(data, &r); err != nil || (r.Status != StatusPassed && r.Status != StatusProblems) {
		return Result{}, false
	}
	return r, true
}
