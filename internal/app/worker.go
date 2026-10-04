package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ajaykathwatee/previa/internal/buildlogs"
	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/platform/githubapp"
	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/ajaykathwatee/previa/internal/pullrequests"
	"github.com/riverqueue/river"
)

// RunWorker checks the database and runs River until canceled, draining on shutdown.
func RunWorker(ctx context.Context, logger *slog.Logger) error {
	cfg, err := config.LoadWorker()
	if err != nil {
		return fmt.Errorf("configure worker: %w", err)
	}
	pool, err := database.NewPool(ctx, cfg.SharedConfig)
	if err != nil {
		return fmt.Errorf("initialize worker database: %w", err)
	}
	defer pool.Close()
	logger.Info("database health check passed")
	producer, err := queue.NewProducer(pool, logger)
	if err != nil {
		return err
	}
	var source pullrequests.Source
	if cfg.GitHubAppID > 0 {
		appClient, err := githubapp.NewApp(cfg.GitHubAppID, cfg.GitHubAppPrivateKeyPath)
		if err != nil {
			return fmt.Errorf("initialize worker GitHub App: %w", err)
		}
		source = pullrequests.NewGitHubSource(appClient)
	}
	syncService := pullrequests.NewSyncService(pool, producer, source, environments.NewBroker(pool, producer))
	client, err := queue.NewWorker(pool, cfg, logger, func(rc *river.Config) {
		if source != nil {
			river.AddWorker(rc.Workers, &pullrequests.SyncWorker{Service: syncService})
			river.AddWorker(rc.Workers, &pullrequests.MergeabilityWorker{Service: syncService})
			rc.Queues[pullrequests.SyncQueue] = river.QueueConfig{MaxWorkers: 2}
		}
		// Current-build log retention needs only the database.
		river.AddWorker(rc.Workers, &buildlogs.PruneWorker{Store: buildlogs.NewStore(pool), Logger: logger})
		rc.Queues[buildlogs.PruneQueue] = river.QueueConfig{MaxWorkers: 1}
		rc.PeriodicJobs = append(rc.PeriodicJobs, buildlogs.PeriodicPrune())
	})
	if err != nil {
		return err
	}
	// Keep job contexts alive while draining after the process signal is received.
	if err := client.Start(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("start River worker (run make migrate-up first): %w", err)
	}
	logger.Info("River worker started", "ping_workers", cfg.MaxConcurrentBuilds, "pr_discovery_configured", source != nil)
	select {
	case <-ctx.Done():
	case <-client.Stopped():
		return errors.New("river worker stopped unexpectedly")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.Stop(stopCtx); err != nil {
		cancelCtx, cancelJobs := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelJobs()
		if cancelErr := client.StopAndCancel(cancelCtx); cancelErr != nil {
			return fmt.Errorf("stop River worker: %w", errors.Join(err, cancelErr))
		}
		return fmt.Errorf("drain River worker: %w", err)
	}
	logger.Info("River worker stopped")
	return nil
}
