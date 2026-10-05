package config

import (
	"errors"
	"fmt"
	"time"

	"github.com/kelseyhightower/envconfig"
)

// HousekeepingConfig bounds expiry, orphan cleanup and pruning. Only the
// worker runs housekeeping.
type HousekeepingConfig struct {
	// EnvTTLHours is how long, in hours, a preview lives after its latest
	// publication; zero keeps previews until their PR closes.
	EnvTTLHours int `envconfig:"ENV_TTL_HOURS" default:"48"`
	// HousekeepingDryRun logs every deletion housekeeping would make (orphans,
	// expiry, pruning) and makes none.
	HousekeepingDryRun bool `envconfig:"HOUSEKEEPING_DRY_RUN" default:"false"`
	// HousekeepingGrace is how old an unreferenced object must be before it
	// counts as an orphan.
	HousekeepingGrace time.Duration `envconfig:"HOUSEKEEPING_GRACE" default:"15m"`
	// BuildCacheBudget caps all build caches together; the oldest unused ones
	// go first.
	BuildCacheBudget ByteSize `envconfig:"BUILD_CACHE_BUDGET" default:"40GiB"`
}

// LoadHousekeeping reads only the housekeeping settings, for diagnostics.
func LoadHousekeeping() (HousekeepingConfig, error) {
	var cfg HousekeepingConfig
	if err := envconfig.Process("", &cfg); err != nil {
		return HousekeepingConfig{}, fmt.Errorf("load housekeeping configuration: %w", err)
	}
	if err := cfg.check(); err != nil {
		return HousekeepingConfig{}, fmt.Errorf("validate housekeeping configuration: %w", err)
	}
	return cfg, nil
}

// PreviewLifetime is ENV_TTL_HOURS as a duration; zero means never.
func (c HousekeepingConfig) PreviewLifetime() time.Duration {
	return time.Duration(c.EnvTTLHours) * time.Hour
}

func (c *HousekeepingConfig) check() error {
	switch {
	case c.EnvTTLHours < 0 || c.EnvTTLHours > 365*24:
		return errors.New("ENV_TTL_HOURS must be 0 (never expire) or a number of hours up to 8760")
	case c.HousekeepingGrace < time.Minute || c.HousekeepingGrace > 24*time.Hour:
		return errors.New("HOUSEKEEPING_GRACE must be between 1m and 24h")
	case c.BuildCacheBudget < 1<<30:
		return errors.New("BUILD_CACHE_BUDGET must be at least 1GiB")
	}
	return nil
}
