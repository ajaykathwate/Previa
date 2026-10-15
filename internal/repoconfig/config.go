package repoconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/go-playground/validator/v10"
	"github.com/goccy/go-yaml"
)

const Filename = ".previewrc.yaml"
const MaxConfigBytes = 64 * 1024

// Resources are effective per-container limits, not build-process limits.
type Resources struct {
	CPUMillis   int64 `json:"cpu_millis"`
	MemoryBytes int64 `json:"memory_bytes"`
}

func DefaultLimits() Resources {
	return Resources{CPUMillis: 2000, MemoryBytes: 2 * 1024 * 1024 * 1024}
}

func (r Resources) validate() error {
	if r.CPUMillis <= 0 || r.MemoryBytes <= 0 {
		return errors.New("platform CPU and memory ceilings must be positive")
	}
	return nil
}

// ServiceCatalog is the operator's pinned backend services; the worker's
// config.ServiceCatalog implements it. Repository YAML only chooses among
// the versions it offers and never names an image.
type ServiceCatalog interface {
	Image(kind, version string) (string, bool)
	Versions(kind string) []string
}

// Platform is what the operator decides and repository YAML cannot: resource
// ceilings and the backend services on offer.
type Platform struct {
	Limits Resources
	// BuildTimeout is the operator's overall build deadline. Zero uses 12m.
	BuildTimeout time.Duration
	// Services is nil when no catalog is configured: backend services are
	// then refused with a message that says so.
	Services ServiceCatalog
}

// Backend preview bounds.
const (
	// MaxProcesses caps the unrouted processes (queue workers and the like)
	// one preview runs next to its app.
	MaxProcesses = 3
	// MaxCommandArgs and MaxCommandBytes bound a setup or process command.
	MaxCommandArgs  = 64
	MaxCommandBytes = 4096
)

// ServiceKinds are the backend services a commit may declare.
var ServiceKinds = []string{"postgres", "redis", "mysql"}

var (
	processName          = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	reservedProcessNames = []string{"app", "setup", "postgres", "redis", "mysql"}
	versionText          = regexp.MustCompile(`^[0-9]{1,3}(\.[0-9]{1,3}){0,2}$`)
)

// Config contains normalized, effective values. Env may contain sensitive values;
// callers must not log Config or Result, and the YAML is intended for non-secrets.
type Config struct {
	Context         string        `yaml:"context" json:"context"`
	BuildTimeout    time.Duration `yaml:"-" json:"build_timeout"`
	Dockerfile      string        `yaml:"dockerfile" json:"dockerfile"`
	Port            int           `yaml:"port" json:"port"`
	Env             []string      `yaml:"env" json:"env"`
	HealthCheckPath string        `yaml:"health_check_path" json:"health_check_path"`
	ResourceTier    string        `yaml:"resource_tier" json:"resource_tier"`
	Resources       Resources     `yaml:"-" json:"resources"`
	// Services are the platform-managed backend services the commit declared,
	// sorted by kind, each resolved to the catalog's pinned image.
	Services []BackendService `yaml:"-" json:"services,omitempty"`
	// Setup is the optional one-off command (argv) that runs once per
	// candidate after its services are ready and before the app starts.
	Setup []string `yaml:"setup" json:"setup,omitempty"`
	// Processes are unrouted commands from the same image, sorted by name.
	Processes []Process `yaml:"processes" json:"processes,omitempty"`
	// DocsPath is an optional path on the preview's own host that reports
	// and the dashboard link to, such as /docs.
	DocsPath string `yaml:"docs_path" json:"docs_path,omitempty"`
}

// BackendService is one declared backend service.
type BackendService struct {
	Kind    string `json:"kind"`    // postgres, redis or mysql
	Version string `json:"version"` // a version the catalog offers
	Image   string `json:"image"`   // the catalog's image, pinned by digest
}

// Process is one unrouted command the preview keeps running.
type Process struct {
	Name         string    `json:"name"`
	Command      []string  `json:"command"`
	ResourceTier string    `json:"resource_tier"`
	Resources    Resources `json:"resources"`
}

// document is the YAML as written; Parse turns it into Config.
type document struct {
	Context         string            `yaml:"context"`
	BuildTimeout    string            `yaml:"build_timeout"`
	Dockerfile      string            `yaml:"dockerfile"`
	Port            int               `yaml:"port"`
	Env             []string          `yaml:"env"`
	HealthCheckPath string            `yaml:"health_check_path"`
	ResourceTier    string            `yaml:"resource_tier"`
	Services        map[string]any    `yaml:"services"` // read from the AST, which keeps versions as written
	Setup           []string          `yaml:"setup"`
	Processes       []processDocument `yaml:"processes"`
	DocsPath        string            `yaml:"docs_path"`
}

type processDocument struct {
	Name         string   `yaml:"name"`
	Command      []string `yaml:"command"`
	ResourceTier string   `yaml:"resource_tier"`
}

type Notice struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Result struct {
	Config  Config
	Notices []Notice
	Hash    string
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var configValidator = validator.New(validator.WithRequiredStructEnabled())

// Parse validates one YAML mapping under limits, with no backend services on
// offer. Explicit null/incorrect types are errors; defaults apply only to
// omitted fields. Limits are platform-owned, never YAML.
func Parse(data []byte, limits Resources) (Result, error) {
	return ParseFor(data, Platform{Limits: limits})
}

// ParseFor validates one YAML mapping against the platform: its ceilings and
// the backend services its catalog offers.
func ParseFor(data []byte, platform Platform) (Result, error) {
	limits := platform.Limits
	if err := limits.validate(); err != nil {
		return Result{}, err
	}
	if len(data) > MaxConfigBytes {
		return Result{}, validationIssue("$", "too_large", "Configuration must not exceed 64 KiB.")
	}
	found, err := validateShape(data)
	if err != nil {
		return Result{}, err
	}
	doc := document{Context: ".", Dockerfile: "Dockerfile", Port: 3000, Env: []string{}, ResourceTier: "standard"}
	if err := yaml.UnmarshalWithOptions(data, &doc, yaml.Strict()); err != nil {
		return Result{}, safeYAMLError(err)
	}
	cfg := Config{
		Context:    doc.Context,
		Dockerfile: doc.Dockerfile, Port: doc.Port, Env: doc.Env, HealthCheckPath: doc.HealthCheckPath,
		ResourceTier: doc.ResourceTier, Setup: doc.Setup, DocsPath: doc.DocsPath,
	}
	var issues []Issue
	cfg.Context, cfg.BuildTimeout, issues = resolveBuildSettings(doc.Context, doc.BuildTimeout, platform.BuildTimeout)
	if !validDockerfile(cfg.Dockerfile) {
		issues = append(issues, Issue{Field: "dockerfile", Code: "invalid_path", Message: "Use a relative file path inside the repository, without '..', backslashes or an absolute path."})
	} else {
		cfg.Dockerfile = path.Clean(cfg.Dockerfile)
	}
	if err := configValidator.Var(cfg.Port, "gte=1,lte=65535"); err != nil {
		issues = append(issues, Issue{Field: "port", Code: "invalid_port", Message: "Port must be an integer from 1 to 65535."})
	}
	if !validHealthPath(cfg.HealthCheckPath) {
		issues = append(issues, Issue{Field: "health_check_path", Code: "invalid_health_path", Message: "Use an absolute HTTP path starting with a single '/', without a host, query, fragment or traversal."})
	}
	seen := make(map[string]bool, len(cfg.Env))
	for i, entry := range cfg.Env {
		name, value, ok := strings.Cut(entry, "=")
		field := fmt.Sprintf("env[%d]", i)
		if !ok || !envName.MatchString(name) || strings.ContainsRune(value, '\x00') {
			issues = append(issues, Issue{Field: field, Code: "invalid_env", Message: "Use NAME=value with a valid environment variable name and no NUL bytes."})
		} else if seen[name] {
			issues = append(issues, Issue{Field: field, Code: "duplicate_env", Message: "Each environment variable name must appear only once."})
		}
		seen[name] = true
	}
	if err := configValidator.Var(cfg.ResourceTier, "oneof=small standard large"); err != nil {
		issues = append(issues, Issue{Field: "resource_tier", Code: "invalid_tier", Message: "Choose small, standard or large."})
	}
	var serviceIssues []Issue
	cfg.Services, serviceIssues = resolveServices(found.services, platform.Services)
	issues = append(issues, serviceIssues...)
	if doc.Setup != nil {
		issues = append(issues, validateCommand("setup", doc.Setup)...)
	}
	var processIssues []Issue
	cfg.Processes, processIssues = validateProcesses(doc.Processes)
	issues = append(issues, processIssues...)
	if !validHealthPath(cfg.DocsPath) {
		issues = append(issues, Issue{Field: "docs_path", Code: "invalid_docs_path", Message: "Use an absolute path on the preview, such as /docs, without a host, query, fragment or traversal."})
	}
	if len(issues) > 0 {
		return Result{}, &ValidationError{Issues: issues}
	}
	var notices []Notice
	cfg.Resources, notices = clampTier(cfg.ResourceTier, "resource_tier", limits, notices)
	for i := range cfg.Processes {
		cfg.Processes[i].Resources, notices = clampTier(cfg.Processes[i].ResourceTier, fmt.Sprintf("processes[%d].resource_tier", i), limits, notices)
	}
	// Environment order does not affect its meaning. Duplicate names were rejected.
	slices.Sort(cfg.Env)
	// Nor does the order of processes, whose names are unique.
	slices.SortFunc(cfg.Processes, func(a, b Process) int { return strings.Compare(a.Name, b.Name) })
	// A requested tier is descriptive; only its effective limits affect runtime.
	// Two tiers clamped to identical limits therefore have the same config hash.
	effective := cfg
	effective.ResourceTier = ""
	effective.Processes = slices.Clone(cfg.Processes)
	for i := range effective.Processes {
		effective.Processes[i].ResourceTier = ""
	}
	encoded, err := json.Marshal(effective)
	if err != nil {
		return Result{}, fmt.Errorf("encode effective preview configuration: %w", err)
	}
	defer clear(encoded)
	sum := sha256.Sum256(encoded)
	return Result{Config: cfg, Notices: notices, Hash: hex.EncodeToString(sum[:])}, nil
}

// clampTier resolves a tier to its limits and lowers them to the platform
// ceilings, noting each reduction.
func clampTier(tier, field string, limits Resources, notices []Notice) (Resources, []Notice) {
	resources := tierResources(tier)
	if resources.CPUMillis > limits.CPUMillis {
		resources.CPUMillis = limits.CPUMillis
		notices = append(notices, Notice{Field: field, Code: "cpu_clamped", Message: "CPU was reduced to the platform ceiling."})
	}
	if resources.MemoryBytes > limits.MemoryBytes {
		resources.MemoryBytes = limits.MemoryBytes
		notices = append(notices, Notice{Field: field, Code: "memory_clamped", Message: "Memory was reduced to the platform ceiling."})
	}
	return resources, notices
}

// resolveServices checks each declared service against the catalog and
// resolves it to the pinned image. Messages name only supported kinds and
// well-formed versions, never other source text.
func resolveServices(requested []requestedService, catalog ServiceCatalog) ([]BackendService, []Issue) {
	var services []BackendService
	var issues []Issue
	for _, r := range requested {
		issue := Issue{Field: "services", Line: r.Line, Column: r.Column}
		switch {
		case !slices.Contains(ServiceKinds, r.Kind):
			issue.Code, issue.Message = "unknown_service", "Unknown service; Previa runs postgres, redis and mysql."
		case catalog == nil || len(catalog.Versions(r.Kind)) == 0:
			issue.Field = "services." + r.Kind
			issue.Code, issue.Message = "service_unavailable", fmt.Sprintf("This server offers no %s service: its operator has not configured one (PREVIEW_SERVICE_CATALOG).", r.Kind)
		case !versionText.MatchString(r.Version):
			issue.Field = "services." + r.Kind
			issue.Code, issue.Message = "invalid_service_version", fmt.Sprintf("Give a version number, such as %s: %s.", r.Kind, catalog.Versions(r.Kind)[0])
		default:
			image, ok := catalog.Image(r.Kind, r.Version)
			if ok {
				services = append(services, BackendService{Kind: r.Kind, Version: r.Version, Image: image})
				continue
			}
			issue.Field = "services." + r.Kind
			issue.Code, issue.Message = "unknown_service_version", fmt.Sprintf("%s %s is not offered on this server; choose %s.", r.Kind, r.Version, strings.Join(catalog.Versions(r.Kind), " or "))
		}
		issues = append(issues, issue)
	}
	slices.SortFunc(services, func(a, b BackendService) int { return strings.Compare(a.Kind, b.Kind) })
	return services, issues
}

// validateCommand checks an argv: a program and its arguments, bounded, with
// no NUL bytes. Arguments are passed as they are; no shell parses them.
func validateCommand(field string, argv []string) []Issue {
	size := 0
	for _, arg := range argv {
		size += len(arg)
		if strings.ContainsRune(arg, '\x00') {
			return []Issue{{Field: field, Code: "invalid_command", Message: "Command arguments must not contain NUL bytes."}}
		}
	}
	switch {
	case len(argv) == 0 || strings.TrimSpace(argv[0]) == "":
		return []Issue{{Field: field, Code: "invalid_command", Message: "Name the program to run first, such as [\"npm\", \"run\", \"migrate\"]."}}
	case len(argv) > MaxCommandArgs || size > MaxCommandBytes:
		return []Issue{{Field: field, Code: "invalid_command", Message: fmt.Sprintf("Commands are limited to %d arguments and %d bytes.", MaxCommandArgs, MaxCommandBytes)}}
	}
	return nil
}

// validateProcesses checks names, commands and tiers; the tier defaults to
// small.
func validateProcesses(docs []processDocument) ([]Process, []Issue) {
	var issues []Issue
	if len(docs) > MaxProcesses {
		return nil, []Issue{{Field: "processes", Code: "too_many_processes", Message: fmt.Sprintf("A preview runs at most %d processes next to its app.", MaxProcesses)}}
	}
	processes := make([]Process, 0, len(docs))
	seen := map[string]bool{}
	for i, doc := range docs {
		field := fmt.Sprintf("processes[%d]", i)
		switch {
		case !processName.MatchString(doc.Name):
			issues = append(issues, Issue{Field: field + ".name", Code: "invalid_process_name", Message: "Use a lowercase name of letters, digits and hyphens, starting with a letter, such as worker."})
		case slices.Contains(reservedProcessNames, doc.Name):
			issues = append(issues, Issue{Field: field + ".name", Code: "reserved_process_name", Message: "app, setup, postgres, redis and mysql are reserved; choose another name."})
		case seen[doc.Name]:
			issues = append(issues, Issue{Field: field + ".name", Code: "duplicate_process", Message: "Each process needs its own name."})
		}
		seen[doc.Name] = true
		issues = append(issues, validateCommand(field+".command", doc.Command)...)
		tier := doc.ResourceTier
		if tier == "" {
			tier = "small"
		}
		if err := configValidator.Var(tier, "oneof=small standard large"); err != nil {
			issues = append(issues, Issue{Field: field + ".resource_tier", Code: "invalid_tier", Message: "Choose small, standard or large."})
		}
		processes = append(processes, Process{Name: doc.Name, Command: doc.Command, ResourceTier: tier})
	}
	return processes, issues
}

func tierResources(tier string) Resources {
	switch tier {
	case "small":
		return Resources{CPUMillis: 500, MemoryBytes: 512 * 1024 * 1024}
	case "large":
		return DefaultLimits()
	default:
		return Resources{CPUMillis: 1000, MemoryBytes: 1024 * 1024 * 1024}
	}
}

func validDockerfile(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || strings.HasSuffix(value, "/") ||
		strings.HasPrefix(value, "/") || strings.ContainsAny(value, `\:`) || strings.ContainsFunc(value, unicode.IsControl) {
		return false
	}
	for segment := range strings.SplitSeq(value, "/") {
		if segment == ".." {
			return false
		}
	}
	return path.Clean(value) != "."
}

func validHealthPath(value string) bool {
	if value == "" {
		return true
	}
	if !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || strings.ContainsAny(value, `\?#`) || strings.ContainsFunc(value, unicode.IsSpace) || strings.ContainsFunc(value, unicode.IsControl) {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || u.IsAbs() || u.Host != "" {
		return false
	}
	for segment := range strings.SplitSeq(u.Path, "/") {
		if segment == ".." {
			return false
		}
	}
	return true
}
