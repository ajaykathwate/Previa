package usage

import (
	"context"
	"fmt"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

type reconcileArgs struct{}

func (reconcileArgs) Kind() string { return "usage_reconcile" }

type reconcileWorker struct {
	river.WorkerDefaults[reconcileArgs]
	store *Store
}

func (w *reconcileWorker) Work(ctx context.Context, _ *river.Job[reconcileArgs]) error {
	return w.store.Reconcile(ctx)
}
func (*reconcileWorker) Timeout(*river.Job[reconcileArgs]) time.Duration { return 45 * time.Second }

func (s *Store) Register(cfg *river.Config) {
	const queue = "usage"
	if cfg.Queues == nil {
		cfg.Queues = map[string]river.QueueConfig{}
	}
	if cfg.Workers == nil {
		cfg.Workers = river.NewWorkers()
	}
	cfg.Queues[queue] = river.QueueConfig{MaxWorkers: 1}
	river.AddWorker(cfg.Workers, &reconcileWorker{store: s})
	cfg.PeriodicJobs = append(cfg.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(time.Minute), func() (river.JobArgs, *river.InsertOpts) {
		return reconcileArgs{}, &river.InsertOpts{Queue: queue, MaxAttempts: 3, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
			rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled,
		}}}
	}, &river.PeriodicJobOpts{RunOnStart: true}))
}

// Configure records the actual worker safety ceilings shown by the server.
// Operators restart all execution workers together when changing these keys.
func (s *Store) Configure(ctx context.Context) error {
	tag, err := s.pool.Exec(ctx, `UPDATE free_plan_policy SET operator_daily_seconds=$1,operator_concurrent_builds=$2,operator_checked_at=clock_timestamp(),
        preview_lifetime_seconds=$3,preview_idle_seconds=$4,log_retention_seconds=nullif($5,0)`, s.dailySeconds, s.buildCeiling, int64(s.previewLifetime/time.Second), int64(s.previewIdle/time.Second), int64(s.logRetention/time.Second))
	if err != nil {
		return fmt.Errorf("record execution safety ceilings: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("free plan policy is missing: apply migration 0042 before starting the worker")
	}
	return s.Reconcile(ctx)
}
