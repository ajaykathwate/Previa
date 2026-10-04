package buildlogs

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/riverqueue/river"
)

const (
	// PruneQueue runs log retention, one job at a time.
	PruneQueue = "build_log_prune"
	// PruneInterval is how often expired logs are deleted.
	PruneInterval = time.Hour
	pruneTimeout  = 5 * time.Minute
)

// PruneArgs is the periodic retention job; it carries no data.
type PruneArgs struct{}

func (PruneArgs) Kind() string { return "build_log_prune" }

// PruneWorker deletes attempts' logs once they are older than Retention.
type PruneWorker struct {
	river.WorkerDefaults[PruneArgs]
	Store  *Store
	Logger *slog.Logger
}

func (w *PruneWorker) Timeout(*river.Job[PruneArgs]) time.Duration { return pruneTimeout }

func (w *PruneWorker) Work(ctx context.Context, _ *river.Job[PruneArgs]) error {
	report, err := w.Store.Prune(ctx, w.Store.now().Add(-Retention))
	if report.Streams > 0 || report.Lines > 0 {
		w.Logger.InfoContext(ctx, "expired build logs deleted", "streams", report.Streams, "lines", report.Lines)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return nil // The next run continues where this one stopped.
	}
	return err
}

// PeriodicPrune schedules retention hourly and once at worker start. River runs
// periodic jobs only on its elected leader.
func PeriodicPrune() *river.PeriodicJob {
	return river.NewPeriodicJob(river.PeriodicInterval(PruneInterval), func() (river.JobArgs, *river.InsertOpts) {
		return PruneArgs{}, &river.InsertOpts{Queue: PruneQueue, MaxAttempts: 3}
	}, &river.PeriodicJobOpts{RunOnStart: true})
}
