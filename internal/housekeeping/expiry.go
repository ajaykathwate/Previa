package housekeeping

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/housekeeping/dbgen"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Teardowns starts a local teardown; *environments.Repository implements it.
type Teardowns interface {
	RequestTeardown(ctx context.Context, pullRequestID int64, reason environments.TeardownReason) (bool, error)
}

// Expiry tears down previews whose lifetime ran out (ENV_TTL_HOURS after their
// latest publication). An expiry is a teardown request: the generation
// advances, the build request is superseded, and nothing rebuilds the preview
// until an owner asks again or a later relevant Automatic event arrives, so an
// expired preview never comes back by itself.
type Expiry struct {
	pool      *pgxpool.Pool
	teardowns Teardowns
	logger    *slog.Logger
	batch     int32
	dryRun    bool
	now       func() time.Time
}

func NewExpiry(pool *pgxpool.Pool, teardowns Teardowns, logger *slog.Logger, settings Settings) (*Expiry, error) {
	if pool == nil || teardowns == nil || logger == nil || settings.BatchSize <= 0 {
		return nil, errors.New("expiry requires a database, teardowns, a logger and a batch size")
	}
	return &Expiry{pool: pool, teardowns: teardowns, logger: logger, batch: settings.BatchSize, dryRun: settings.DryRun, now: time.Now}, nil
}

// ExpiryPass is one pass's outcome.
type ExpiryPass struct {
	Due     int // previews past their expiry
	Expired int // teardowns started (0 in a dry run)
}

func (e *Expiry) RunOnce(ctx context.Context) (ExpiryPass, error) {
	var pass ExpiryPass
	due, err := dbgen.New(e.pool).ListExpiredPreviews(ctx, dbgen.ListExpiredPreviewsParams{
		NowAt: pgtype.Timestamptz{Time: e.now(), Valid: true}, BatchSize: e.batch,
	})
	if err != nil {
		return pass, fmt.Errorf("list expired previews: %w", err)
	}
	pass.Due = len(due)
	var errs []error
	for _, preview := range due {
		if e.dryRun {
			e.logger.InfoContext(ctx, "preview expired", "dry_run", true, "pr_id", preview.PullRequestID, "expires_at", preview.ExpiresAt.Time)
			continue
		}
		started, err := e.teardowns.RequestTeardown(ctx, preview.PullRequestID, environments.TeardownExpired)
		if err != nil {
			errs = append(errs, fmt.Errorf("expire PR %d: %w", preview.PullRequestID, err))
			continue
		}
		if started {
			pass.Expired++
		}
		e.logger.InfoContext(ctx, "preview expired", "dry_run", false, "pr_id", preview.PullRequestID, "expires_at", preview.ExpiresAt.Time, "teardown_started", started)
	}
	return pass, errors.Join(errs...)
}
