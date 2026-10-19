package installations

import (
	"context"
	"fmt"
	"time"

	platformdb "github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

const ReconciliationQueue = "installation_reconcile"

type ReconcileArgs struct {
	InstallationID int64 `json:"installation_id,omitempty"`
}

func (ReconcileArgs) Kind() string { return "installation_reconcile" }

type reconcileWorker struct {
	river.WorkerDefaults[ReconcileArgs]
	service *Reconciler
}

func (w *reconcileWorker) Work(ctx context.Context, j *river.Job[ReconcileArgs]) error {
	return w.service.Run(ctx, j.Args.InstallationID)
}
func (*reconcileWorker) Timeout(*river.Job[ReconcileArgs]) time.Duration { return 4 * time.Minute }
func reconcileOptions() *river.InsertOpts {
	return &river.InsertOpts{Queue: ReconciliationQueue, MaxAttempts: 1, UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: 30 * time.Second}}
}
func (s *Reconciler) Register(cfg *river.Config) {
	if cfg.Workers == nil {
		cfg.Workers = river.NewWorkers()
	}
	if cfg.Queues == nil {
		cfg.Queues = map[string]river.QueueConfig{}
	}
	river.AddWorker(cfg.Workers, &reconcileWorker{service: s})
	cfg.Queues[ReconciliationQueue] = river.QueueConfig{MaxWorkers: 1}
	cfg.PeriodicJobs = append(cfg.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(ReconciliationInterval), func() (river.JobArgs, *river.InsertOpts) { return ReconcileArgs{}, reconcileOptions() }, &river.PeriodicJobOpts{RunOnStart: true}))
}

// Signal records only suspicion and a durable wake-up. Authentication failures
// cannot revoke a repository from a build, discovery, report or HTTP request.
func SignalReconciliation(pool *pgxpool.Pool, producer *river.Client[pgx.Tx]) func(context.Context, int64) error {
	return func(ctx context.Context, id int64) error {
		return platformdb.InTransaction(ctx, pool, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE installations SET reconcile_requested_at=clock_timestamp() WHERE github_installation_id=$1 AND status IN ('active','suspended') AND disconnected_at IS NULL`, id)
			if err != nil {
				return fmt.Errorf("record installation token signal: %w", err)
			}
			if tag.RowsAffected() == 0 {
				return nil
			}
			if _, err = producer.InsertTx(ctx, tx, ReconcileArgs{InstallationID: id}, reconcileOptions()); err != nil {
				return fmt.Errorf("wake installation reconciliation: %w", err)
			}
			return nil
		})
	}
}
