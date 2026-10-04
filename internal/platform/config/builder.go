package config

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/kelseyhightower/envconfig"
)

// DefaultBuildKitImage is the pinned rootless BuildKit daemon used for builds.
const DefaultBuildKitImage = "moby/buildkit:v0.33.1-rootless@sha256:f8a833b2de9d68e27f0815e4a737abdfaf8a2e4c615650557df11025101557b4"

// ExecutionConfig is what the worker needs to build images.
type ExecutionConfig struct {
	// EgressImage runs the proxy that is the only route out of build
	// networks (and, with the runtime, preview networks).
	EgressImage string `envconfig:"EGRESS_IMAGE" default:"previa-egress:dev"`
	BuilderConfig
}

// LoadExecution reads only the build settings, for tools and opt-in checks.
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
	return c.BuilderConfig.check()
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
