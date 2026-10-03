package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

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
	syncService := pullrequests.NewSyncService(pool, producer, source)
	client, err := queue.NewWorker(pool, cfg, logger, func(workers *river.Workers, queues map[string]river.QueueConfig) {
		if source != nil {
			river.AddWorker(workers, &pullrequests.SyncWorker{Service: syncService})
			queues[pullrequests.SyncQueue] = river.QueueConfig{MaxWorkers: 2}
		}
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
