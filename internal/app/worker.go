package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/platform/queue"
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
	client, err := queue.NewWorker(pool, cfg, logger)
	if err != nil {
		return err
	}
	// Keep job contexts alive while draining after the process signal is received.
	if err := client.Start(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("start River worker (run make migrate-up first): %w", err)
	}
	logger.Info("River worker started", "queue", queue.PingQueue, "max_workers", cfg.MaxConcurrentBuilds)
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
