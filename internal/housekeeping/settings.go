package housekeeping

import (
	"errors"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/config"
)

// Queue runs housekeeping jobs one at a time, apart from builds and cleanup,
// so a full build queue never delays them.
const Queue = "housekeeping"

// How often each job runs. Every job also runs once when a worker's River
// client becomes leader (startup), so a restart recovers at once.
const (
	RecoverInterval     = time.Minute
	ResourcesInterval   = 5 * time.Minute
	PullRequestInterval = 15 * time.Minute
	ExpiryInterval      = 10 * time.Minute
	PruneInterval       = time.Hour
	RecordsInterval     = time.Hour
	// EmergencyPruneEvery bounds how often a build waiting for disk space
	// triggers a prune outside the hourly schedule.
	EmergencyPruneEvery = 5 * time.Minute
)

// Settings bound every pass.
type Settings struct {
	DryRun bool          // log deletions, make none (HOUSEKEEPING_DRY_RUN)
	Grace  time.Duration // unreferenced objects younger than this are left alone
	// CacheBudget caps all build caches together (BUILD_CACHE_BUDGET).
	CacheBudget int64
	// WorkspaceMaxAge: no build runs longer, so an older source workspace is a
	// crash leftover (BUILD_TIMEOUT plus the grace period).
	WorkspaceMaxAge  time.Duration
	BatchSize        int32 // PRs, operations or previews per pass
	PullRequestBatch int32 // GitHub reads per pass
}

// SettingsFrom adapts the worker's settings.
func SettingsFrom(c config.HousekeepingConfig, buildTimeout time.Duration) Settings {
	return Settings{
		DryRun: c.HousekeepingDryRun, Grace: c.HousekeepingGrace, CacheBudget: int64(c.BuildCacheBudget),
		WorkspaceMaxAge: buildTimeout + c.HousekeepingGrace, BatchSize: 50, PullRequestBatch: 10,
	}
}

func (s Settings) validate() error {
	switch {
	case s.Grace <= 0:
		return errors.New("housekeeping needs a positive grace period")
	case s.BatchSize <= 0 || s.PullRequestBatch <= 0:
		return errors.New("housekeeping needs positive batch sizes")
	case s.CacheBudget <= 0 || s.WorkspaceMaxAge <= 0:
		return errors.New("housekeeping needs a cache budget and a workspace age limit")
	}
	return nil
}
