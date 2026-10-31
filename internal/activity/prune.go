package activity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ajaykathwatee/previa/internal/activity/dbgen"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

const (
	// Retention is how long an event is kept.
	Retention = 90 * 24 * time.Hour
	// PerPullRequest is how many of a pull request's newest events are kept.
	PerPullRequest = 5000
	// PruneQueue runs retention, one job at a time.
	PruneQueue = "activity_prune"
	// PruneInterval is how often retention runs.
	PruneInterval = time.Hour
	pruneBatch    = 1000
	pruneTimeout  = 5 * time.Minute
)

// Pruner applies retention to activity events.
type Pruner struct {
	pool           *pgxpool.Pool
	now            func() time.Time
	retention      time.Duration
	perPullRequest int
	batch          int
}

// NewPruner prunes events in pool with the published retention.
func NewPruner(pool *pgxpool.Pool) *Pruner {
	return &Pruner{pool: pool, now: time.Now, retention: Retention, perPullRequest: PerPullRequest, batch: pruneBatch}
}

// Prune deletes events past their age, then events beyond each pull
// request's cap, in batches until none remain or ctx ends. It returns how
// many it deleted.
func (p *Pruner) Prune(ctx context.Context) (int64, error) {
	q := dbgen.New(p.pool)
	cutoff := pgtype.Timestamptz{Time: p.now().Add(-p.retention), Valid: true}
	var total int64
	for {
		n, err := q.PruneAgedEvents(ctx, dbgen.PruneAgedEventsParams{Cutoff: cutoff, BatchSize: int32(p.batch)})
		if err != nil {
			return total, fmt.Errorf("prune aged activity: %w", err)
		}
		total += n
		if n < int64(p.batch) {
			break
		}
	}
	for {
		n, err := q.PruneOverflowEvents(ctx, dbgen.PruneOverflowEventsParams{PerPullRequest: int32(p.perPullRequest), BatchSize: int32(p.batch)})
		if err != nil {
			return total, fmt.Errorf("prune activity beyond its cap: %w", err)
		}
		total += n
		if n == 0 {
			return total, nil
		}
	}
}

// PruneArgs is the periodic retention job; it carries no data.
type PruneArgs struct{}

func (PruneArgs) Kind() string { return "activity_prune" }

// PruneWorker runs retention.
type PruneWorker struct {
	river.WorkerDefaults[PruneArgs]
	Pruner *Pruner
	Logger *slog.Logger
}

func (w *PruneWorker) Timeout(*river.Job[PruneArgs]) time.Duration { return pruneTimeout }

func (w *PruneWorker) Work(ctx context.Context, _ *river.Job[PruneArgs]) error {
	n, err := w.Pruner.Prune(ctx)
	if n > 0 {
		w.Logger.InfoContext(ctx, "old activity deleted", "events", n)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return nil // The next run continues where this one stopped.
	}
	return err
}

// Register adds retention to a worker: its queue, worker and an hourly
// schedule that also runs at start. River runs periodic jobs only on its
// elected leader. Retention needs only the database.
func Register(rc *river.Config, pool *pgxpool.Pool, logger *slog.Logger) {
	river.AddWorker(rc.Workers, &PruneWorker{Pruner: NewPruner(pool), Logger: logger})
	rc.Queues[PruneQueue] = river.QueueConfig{MaxWorkers: 1}
	rc.PeriodicJobs = append(rc.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(PruneInterval), func() (river.JobArgs, *river.InsertOpts) {
		return PruneArgs{}, &river.InsertOpts{Queue: PruneQueue, MaxAttempts: 3}
	}, &river.PeriodicJobOpts{RunOnStart: true}))
}
