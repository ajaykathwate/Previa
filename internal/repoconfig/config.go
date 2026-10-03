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

// Config contains normalized, effective values. Env may contain sensitive values;
// callers must not log Config or Result, and the YAML is intended for non-secrets.
type Config struct {
	Dockerfile      string    `yaml:"dockerfile" json:"dockerfile"`
	Port            int       `yaml:"port" json:"port"`
	Env             []string  `yaml:"env" json:"env"`
	HealthCheckPath string    `yaml:"health_check_path" json:"health_check_path"`
	ResourceTier    string    `yaml:"resource_tier" json:"resource_tier"`
	Resources       Resources `yaml:"-" json:"resources"`
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

// Parse validates one YAML mapping. Explicit null/incorrect types are errors;
// defaults apply only to omitted fields. Limits are platform-owned, never YAML.
func Parse(data []byte, limits Resources) (Result, error) {
	if err := limits.validate(); err != nil {
		return Result{}, err
	}
	if len(data) > MaxConfigBytes {
		return Result{}, validationIssue("$", "too_large", "Configuration must not exceed 64 KiB.")
	}
	if err := validateShape(data); err != nil {
		return Result{}, err
	}
	cfg := Config{Dockerfile: "Dockerfile", Port: 3000, Env: []string{}, ResourceTier: "standard"}
	if err := yaml.UnmarshalWithOptions(data, &cfg, yaml.Strict()); err != nil {
		return Result{}, safeYAMLError(err)
	}
	var issues []Issue
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
	if len(issues) > 0 {
		return Result{}, &ValidationError{Issues: issues}
	}
	cfg.Resources = tierResources(cfg.ResourceTier)
	var notices []Notice
	if cfg.Resources.CPUMillis > limits.CPUMillis {
		cfg.Resources.CPUMillis = limits.CPUMillis
		notices = append(notices, Notice{Field: "resource_tier", Code: "cpu_clamped", Message: "CPU was reduced to the platform ceiling."})
	}
	if cfg.Resources.MemoryBytes > limits.MemoryBytes {
		cfg.Resources.MemoryBytes = limits.MemoryBytes
		notices = append(notices, Notice{Field: "resource_tier", Code: "memory_clamped", Message: "Memory was reduced to the platform ceiling."})
	}
	// Environment order does not affect its meaning. Duplicate names were rejected.
	slices.Sort(cfg.Env)
	// A requested tier is descriptive; only its effective limits affect runtime.
	// Two tiers clamped to identical limits therefore have the same config hash.
	effective := cfg
	effective.ResourceTier = ""
	encoded, err := json.Marshal(effective)
	if err != nil {
		return Result{}, fmt.Errorf("encode effective preview configuration: %w", err)
	}
	defer clear(encoded)
	sum := sha256.Sum256(encoded)
	return Result{Config: cfg, Notices: notices, Hash: hex.EncodeToString(sum[:])}, nil
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
