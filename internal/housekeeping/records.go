package housekeeping

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ajaykathwatee/previa/internal/housekeeping/dbgen"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// How long finished history is kept. Backups hold deleted rows until they
// age out (BACKUP_RETENTION_DAYS), so data is fully gone within that much
// longer.
const (
	// DeliveryRetention keeps webhook delivery records, which only catch
	// duplicate deliveries.
	DeliveryRetention = 30 * 24 * time.Hour
	// OperationRetention keeps finished operations for diagnosis.
	OperationRetention = 30 * 24 * time.Hour
	// ClosedPullRequestRetention keeps a closed or merged PR, with its
	// preview's records, operations, report and logs, after GitHub last
	// updated it.
	ClosedPullRequestRetention = 90 * 24 * time.Hour
	// UsageRetention keeps daily build-minute ledgers.
	UsageRetention = 90 * 24 * time.Hour
	// OutputRetention keeps live app output nobody watches any more.
	OutputRetention = time.Hour
	// recordRows bounds each delete statement.
	recordRows = 5000
)

// Records deletes history whose retention ended. Only finished work goes: a
// PR is deleted once its teardown finished and nothing about it is pending.
// A dry run counts what one batch would delete and keeps it.
type Records struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
	batch  int32
	dryRun bool
	now    func() time.Time
}

func NewRecords(pool *pgxpool.Pool, logger *slog.Logger, settings Settings) (*Records, error) {
	if pool == nil || logger == nil || settings.BatchSize <= 0 {
		return nil, errors.New("record retention requires a database, a logger and a batch size")
	}
	return &Records{pool: pool, logger: logger, batch: settings.BatchSize, dryRun: settings.DryRun, now: time.Now}, nil
}

// RecordsPass is one pass's outcome (what would go, in a dry run).
type RecordsPass struct {
	Deliveries, Operations, Usage, OutputWatches int64
	PullRequests                                 int
}

// errDryRun rolls back a dry run's deletions.
var errDryRun = errors.New("dry run")

func (r *Records) RunOnce(ctx context.Context) (RecordsPass, error) {
	var pass RecordsPass
	now := r.now()
	err := r.inTx(ctx, func(q *dbgen.Queries) error {
		var err error
		if pass.Deliveries, err = drain(r.dryRun, func() (int64, error) {
			return q.DeleteOldDeliveries(ctx, dbgen.DeleteOldDeliveriesParams{Cutoff: at(now.Add(-DeliveryRetention)), BatchSize: recordRows})
		}); err != nil {
			return fmt.Errorf("delete old webhook deliveries: %w", err)
		}
		if pass.Operations, err = drain(r.dryRun, func() (int64, error) {
			return q.DeleteOldOperations(ctx, dbgen.DeleteOldOperationsParams{Cutoff: at(now.Add(-OperationRetention)), BatchSize: recordRows})
		}); err != nil {
			return fmt.Errorf("delete old operations: %w", err)
		}
		day := now.Add(-UsageRetention).UTC()
		if pass.Usage, err = q.DeleteOldUsage(ctx, pgtype.Date{Time: time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC), Valid: true}); err != nil {
			return fmt.Errorf("delete old build-minute ledgers: %w", err)
		}
		if pass.OutputWatches, err = q.DeleteStaleOutputWatches(ctx, at(now.Add(-OutputRetention))); err != nil {
			return fmt.Errorf("delete unwatched app output: %w", err)
		}
		return nil
	})
	if err != nil {
		return pass, err
	}
	retired, err := dbgen.New(r.pool).ListRetiredPullRequests(ctx, dbgen.ListRetiredPullRequestsParams{
		Cutoff: at(now.Add(-ClosedPullRequestRetention)), BatchSize: r.batch,
	})
	if err != nil {
		return pass, fmt.Errorf("list retired pull requests: %w", err)
	}
	var errs []error
	for _, id := range retired {
		deleted, err := r.deletePullRequest(ctx, id, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("delete records of PR %d: %w", id, err))
			continue
		}
		if deleted {
			pass.PullRequests++
		}
	}
	if pass != (RecordsPass{}) {
		r.logger.InfoContext(ctx, "expired records deleted", "dry_run", r.dryRun, "webhook_deliveries", pass.Deliveries,
			"operations", pass.Operations, "usage_days", pass.Usage, "app_output", pass.OutputWatches, "pull_requests", pass.PullRequests)
	}
	return pass, errors.Join(errs...)
}

// deletePullRequest deletes one retired PR and everything recorded about
// it, after checking again under its lock that it is still retired.
func (r *Records) deletePullRequest(ctx context.Context, id int64, now time.Time) (bool, error) {
	var deleted bool
	err := r.inTx(ctx, func(q *dbgen.Queries) error {
		if _, err := q.LockPullRequest(ctx, id); errors.Is(err, pgx.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		still, err := q.ListRetiredPullRequests(ctx, dbgen.ListRetiredPullRequestsParams{
			Cutoff: at(now.Add(-ClosedPullRequestRetention)), PrID: pgtype.Int8{Int64: id, Valid: true}, BatchSize: 1,
		})
		if err != nil || len(still) == 0 {
			return err
		}
		for _, step := range []func(context.Context, int64) error{
			q.DeleteRetiredBuildLogs, q.DeleteRetiredRoutes, q.DeleteRetiredOperations,
			q.DeleteRetiredReports, q.DeleteRetiredReconciliations, q.DeleteRetiredEnvironments,
		} {
			if err := step(ctx, id); err != nil {
				return err
			}
		}
		n, err := q.DeleteRetiredPullRequest(ctx, id)
		deleted = n == 1
		return err
	})
	return deleted, err
}

// inTx runs fn in a transaction that a dry run rolls back.
func (r *Records) inTx(ctx context.Context, fn func(q *dbgen.Queries) error) error {
	err := database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		if err := fn(dbgen.New(tx)); err != nil {
			return err
		}
		if r.dryRun {
			return errDryRun
		}
		return nil
	})
	if errors.Is(err, errDryRun) {
		return nil
	}
	return err
}

// drain repeats a bounded delete until it deletes less than a batch; a dry
// run counts one batch.
func drain(dryRun bool, deleteBatch func() (int64, error)) (int64, error) {
	var total int64
	for {
		n, err := deleteBatch()
		total += n
		if err != nil || n < recordRows || dryRun {
			return total, err
		}
	}
}

func at(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
