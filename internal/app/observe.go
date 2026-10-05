package app

import (
	"context"
	"log/slog"
	"os"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/metrics"
	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/ajaykathwatee/previa/internal/platform/workers"
	"github.com/jackc/pgx/v5/pgxpool"
)

// registerServerState exports durable state as gauges read at scrape time:
// queue depth and age, environments, operations, current requests, live
// workers and, when BACKUP_STATUS_FILE is set, the last successful backup.
// The server exports them because it keeps running when no worker does,
// which is exactly when queue depth matters.
func registerServerState(pool *pgxpool.Pool, repo *environments.Repository, caps *workers.Reader, backupStatusFile string, logger *slog.Logger) {
	metrics.RegisterState(func(ctx context.Context, s *metrics.State) (err error) {
		defer func() {
			if err != nil {
				logger.WarnContext(ctx, "metrics state read failed", "error", err)
			}
		}()
		queues, err := queue.Stats(ctx, pool)
		if err != nil {
			return err
		}
		for _, q := range queues {
			s.QueueJobs(q.Queue, q.State, q.Jobs)
			if q.State == "available" {
				s.QueueOldest(q.Queue, q.OldestDue)
			}
		}
		stats, err := repo.Stats(ctx)
		if err != nil {
			return err
		}
		for status, n := range stats.Environments {
			s.Environments(status, n)
		}
		for _, o := range stats.Operations {
			s.Operations(o.Kind, o.State, o.Count)
		}
		for _, r := range stats.Requests {
			s.Requests(r.State, r.Waiting, r.Count)
		}
		c, live, err := caps.Live(ctx)
		if err != nil {
			return err
		}
		s.Workers(live, map[string]bool{
			"execution": c.Execution, "routing": c.Routing, "reporting": c.Reporting,
			"housekeeping": c.Housekeeping, "discovery": c.Discovery, "paused": c.Paused,
		})
		if backupStatusFile != "" {
			if info, err := os.Stat(backupStatusFile); err == nil {
				s.BackupSucceeded(info.ModTime())
			}
		}
		return nil
	})
}
