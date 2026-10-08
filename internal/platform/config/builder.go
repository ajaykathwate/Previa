package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kelseyhightower/envconfig"
)

// DefaultBuildKitImage is the pinned rootless BuildKit daemon used for builds.
const DefaultBuildKitImage = "moby/buildkit:v0.33.1-rootless@sha256:f8a833b2de9d68e27f0815e4a737abdfaf8a2e4c615650557df11025101557b4"

// ExecutionConfig is what the worker needs to build and run previews.
type ExecutionConfig struct {
	// EgressImage runs the proxy that is the only route out of build and
	// preview networks.
	EgressImage string `envconfig:"EGRESS_IMAGE" default:"previa-egress:dev"`
	// SourceWorkspaceRoot holds each build's extracted source while it builds;
	// empty uses previa-sources in the system temporary directory.
	SourceWorkspaceRoot string `envconfig:"SOURCE_WORKSPACE_ROOT"`
	BuilderConfig
	PreviewConfig
}

// LoadExecution reads only the build and preview settings, for tools and
// opt-in checks.
func LoadExecution() (ExecutionConfig, error) {
	var cfg ExecutionConfig
	if err := envconfig.Process("", &cfg); err != nil {
		return ExecutionConfig{}, fmt.Errorf("load execution configuration: %w", err)
	}
	if err := cfg.check(); err != nil {
		return ExecutionConfig{}, fmt.Errorf("validate execution configuration: %w", err)
	}
	return cfg, nil
}

func (c *ExecutionConfig) check() error {
	if strings.TrimSpace(c.EgressImage) == "" {
		return errors.New("EGRESS_IMAGE must not be empty")
	}
	if c.SourceWorkspaceRoot == "" {
		c.SourceWorkspaceRoot = filepath.Join(os.TempDir(), "previa-sources")
	}
	if !filepath.IsAbs(c.SourceWorkspaceRoot) || filepath.Clean(c.SourceWorkspaceRoot) != c.SourceWorkspaceRoot {
		return errors.New("SOURCE_WORKSPACE_ROOT must be an absolute, clean path")
	}
	if err := c.BuilderConfig.check(); err != nil {
		return err
	}
	return c.PreviewConfig.check()
}

// BuilderConfig bounds isolated image builds. Only the worker builds images.
type BuilderConfig struct {
	BuildKitImage     string   `envconfig:"BUILDKIT_IMAGE"`
	BuildCPUs         float64  `envconfig:"BUILD_CPUS" default:"2"`
	BuildMemory       ByteSize `envconfig:"BUILD_MEMORY" default:"4GiB"`
	BuildPIDs         int64    `envconfig:"BUILD_PIDS" default:"2048"`
	BuildDiskBudget   ByteSize `envconfig:"BUILD_DISK_BUDGET" default:"20GiB"`
	BuildMaxImageSize ByteSize `envconfig:"BUILD_MAX_IMAGE_SIZE" default:"4GiB"`
	BuildMinFreeDisk  ByteSize `envconfig:"BUILD_MIN_FREE_DISK" default:"15GiB"`
}

func (c *BuilderConfig) check() error {
	if strings.TrimSpace(c.BuildKitImage) == "" {
		c.BuildKitImage = DefaultBuildKitImage
	}
	switch {
	case math.IsNaN(c.BuildCPUs) || c.BuildCPUs < 0.25 || c.BuildCPUs > 64:
		return errors.New("BUILD_CPUS must be between 0.25 and 64")
	case c.BuildMemory < 256<<20:
		return errors.New("BUILD_MEMORY must be at least 256MiB")
	case c.BuildPIDs < 128 || c.BuildPIDs > 65536:
		return errors.New("BUILD_PIDS must be between 128 and 65536")
	case c.BuildDiskBudget < 1<<30:
		return errors.New("BUILD_DISK_BUDGET must be at least 1GiB")
	case c.BuildMaxImageSize < 16<<20 || c.BuildMaxImageSize > c.BuildDiskBudget:
		return errors.New("BUILD_MAX_IMAGE_SIZE must be at least 16MiB and at most BUILD_DISK_BUDGET")
	case c.BuildMinFreeDisk < 0:
		return errors.New("BUILD_MIN_FREE_DISK must not be negative")
	}
	return nil
}

// PreviewConfig hardens and bounds preview containers. The two compatibility
// switches are server-wide operator decisions; repositories cannot set them.
type PreviewConfig struct {
	PreviewAllowRoot      bool          `envconfig:"PREVIEW_ALLOW_ROOT" default:"false"`
	PreviewWritableRootfs bool          `envconfig:"PREVIEW_WRITABLE_ROOTFS" default:"false"`
	PreviewTmpfsSize      ByteSize      `envconfig:"PREVIEW_TMPFS_SIZE" default:"256MiB"`
	PreviewPIDs           int64         `envconfig:"PREVIEW_PIDS" default:"512"`
	PreviewStartupTimeout time.Duration `envconfig:"PREVIEW_STARTUP_TIMEOUT" default:"120s"`
	// Zero budgets derive from the Docker host: 60% of its memory and 75% of
	// its CPUs may be used by previews together.
	PreviewMemoryBudget     ByteSize `envconfig:"PREVIEW_MEMORY_BUDGET"`
	PreviewCPUBudget        float64  `envconfig:"PREVIEW_CPU_BUDGET"`
	PreviewMaxPerRepository int      `envconfig:"PREVIEW_MAX_PER_REPOSITORY" default:"10"`
	// PreviewOCIRuntime selects an installed Docker runtime such as runsc
	// (gVisor); empty uses Docker's default.
	PreviewOCIRuntime string `envconfig:"PREVIEW_OCI_RUNTIME"`
	// PreviewServiceCatalog lists the backend services (PostgreSQL, Redis)
	// previews may request, as kind:version=image@sha256:digest pairs; empty
	// uses Previa's pinned catalog and "none" offers none.
	PreviewServiceCatalog string `envconfig:"PREVIEW_SERVICE_CATALOG"`
	// PreviewSetupTimeout bounds a preview's setup command (migrations, seed
	// data).
	PreviewSetupTimeout time.Duration `envconfig:"PREVIEW_SETUP_TIMEOUT" default:"5m"`
}

// ServiceCatalog parses PREVIEW_SERVICE_CATALOG.
func (c PreviewConfig) ServiceCatalog() (ServiceCatalog, error) {
	return ParseServiceCatalog(c.PreviewServiceCatalog)
}

var runtimeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func (c *PreviewConfig) check() error {
	switch {
	case c.PreviewTmpfsSize < 16<<20 || c.PreviewTmpfsSize > 4<<30:
		return errors.New("PREVIEW_TMPFS_SIZE must be between 16MiB and 4GiB")
	case c.PreviewPIDs < 64 || c.PreviewPIDs > 65536:
		return errors.New("PREVIEW_PIDS must be between 64 and 65536")
	case c.PreviewStartupTimeout < 5*time.Second || c.PreviewStartupTimeout > 30*time.Minute:
		return errors.New("PREVIEW_STARTUP_TIMEOUT must be between 5s and 30m")
	case c.PreviewMemoryBudget < 0 || math.IsNaN(c.PreviewCPUBudget) || c.PreviewCPUBudget < 0:
		return errors.New("PREVIEW_MEMORY_BUDGET and PREVIEW_CPU_BUDGET must not be negative")
	case c.PreviewMaxPerRepository < 1 || c.PreviewMaxPerRepository > 1000:
		return errors.New("PREVIEW_MAX_PER_REPOSITORY must be between 1 and 1000")
	case c.PreviewOCIRuntime != "" && !runtimeName.MatchString(c.PreviewOCIRuntime):
		return errors.New("PREVIEW_OCI_RUNTIME must be a Docker runtime name such as runsc")
	case c.PreviewSetupTimeout < 10*time.Second || c.PreviewSetupTimeout > 30*time.Minute:
		return errors.New("PREVIEW_SETUP_TIMEOUT must be between 10s and 30m")
	}
	_, err := c.ServiceCatalog()
	return err
}

// ByteSize is a byte count written with an optional binary unit, such as
// 512MiB, 4GiB or 1048576.
type ByteSize int64

var byteUnits = []struct {
	suffix string
	shift  uint
}{{"TiB", 40}, {"GiB", 30}, {"MiB", 20}, {"KiB", 10}, {"B", 0}}

// Decode implements envconfig.Decoder.
func (b *ByteSize) Decode(value string) error {
	value = strings.TrimSpace(value)
	shift := uint(0)
	for _, unit := range byteUnits {
		if number, found := strings.CutSuffix(value, unit.suffix); found {
			value, shift = strings.TrimSpace(number), unit.shift
			break
		}
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 || n > math.MaxInt64>>shift {
		return fmt.Errorf("invalid size %q: use a whole number with an optional KiB, MiB, GiB or TiB unit", value)
	}
	*b = ByteSize(n << shift)
	return nil
}

func (b ByteSize) String() string {
	for _, unit := range byteUnits[:4] {
		if b >= 1<<unit.shift && int64(b)%(1<<unit.shift) == 0 {
			return strconv.FormatInt(int64(b)>>unit.shift, 10) + unit.suffix
		}
	}
	return strconv.FormatInt(int64(b), 10) + "B"
}
