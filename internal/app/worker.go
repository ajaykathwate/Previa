package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/platform/githubapp"
	"github.com/ajaykathwatee/previa/internal/platform/httpx"
	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/ajaykathwatee/previa/internal/platform/version"
	"github.com/ajaykathwatee/previa/internal/platform/workers"
	"github.com/jackc/pgx/v5"
	"github.com/moby/moby/client"
	"github.com/riverqueue/river"
)

// drainTimeout is how long a stopping worker lets running jobs finish before
// it cancels them. Cancelled builds are not failures: they run again on the
// next start; operations and reports resume from their durable rows.
const drainTimeout = 30 * time.Second

// RunWorker checks its dependencies, registers exactly the work it can do,
// records those capabilities for the server, and runs River until canceled,
// draining on shutdown.
func RunWorker(ctx context.Context, logger *slog.Logger) error {
	cfg, err := config.LoadWorker()
	if err != nil {
		return fmt.Errorf("configure worker: %w", err)
	}
	cfg.ApplicationName = "previa-worker"
	pool, err := database.NewPool(ctx, cfg.SharedConfig)
	if err != nil {
		return fmt.Errorf("initialize worker database: %w", err)
	}
	defer pool.Close()
	if err := database.CheckSchema(ctx, pool); err != nil {
		return err
	}
	logger.Info("database health check passed", "version", version.Version, "schema", database.SchemaVersion, "environment", cfg.AppEnv)
	producer, err := queue.NewProducer(pool, logger)
	if err != nil {
		return err
	}
	in := pipelineInput{cfg: cfg, pool: pool, producer: producer, logger: logger}
	if cfg.GitHubAppID > 0 {
		app, err := githubapp.NewApp(cfg.GitHubAppID, cfg.GitHubAppPrivateKeyPath)
		if err != nil {
			return fmt.Errorf("initialize worker GitHub App: %w", err)
		}
		in.github = app
	}
	if cfg.RoutingConfigured() && in.github != nil {
		docker, err := dockerx.New(ctx)
		if err != nil {
			return fmt.Errorf("builds need Docker (unset PREVIEW_BASE_DOMAIN to run without builds): %w", err)
		}
		defer func(c *client.Client) { _ = c.Close() }(docker)
		in.docker = docker
	}
	p, err := buildPipeline(ctx, in)
	if err != nil {
		return fmt.Errorf("compose worker: %w", err)
	}
	for _, step := range p.startup {
		if err := step(ctx); err != nil {
			return fmt.Errorf("prepare worker: %w", err)
		}
	}
	riverClient, err := queue.NewWorker(pool, cfg, logger, p.register...)
	if err != nil {
		return err
	}
	// Keep job contexts alive while draining after the process signal is received.
	if err := riverClient.Start(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("start River worker (run make migrate-up first): %w", err)
	}
	heartbeat, err := workers.Start(ctx, pool, p.capabilities, logger)
	if err != nil {
		return errors.Join(err, stopRiver(riverClient, logger))
	}
	for _, reason := range p.unavailable {
		logger.Warn("capability unavailable", "reason", reason)
	}
	checks := append([]httpx.Check{
		{Name: "database", Run: func(ctx context.Context) error { return database.HealthCheck(ctx, pool) }},
		{Name: "schema", Run: func(ctx context.Context) error { return database.CheckSchema(ctx, pool) }},
		{Name: "river", Run: func(context.Context) error {
			select {
			case <-riverClient.Stopped():
				return errors.New("stopped")
			default:
				return nil
			}
		}},
		{Name: "heartbeat", Run: func(context.Context) error { return heartbeat.Fresh() }},
	}, p.checks...)
	stopInternal, err := httpx.ServeInternal(cfg.InternalAddr, httpx.Internal(checks, func() map[string]any {
		return map[string]any{"capabilities": heartbeat.Capabilities(), "version": version.Version}
	}), logger)
	if err != nil {
		return errors.Join(err, heartbeat.Stop(context.Background()), stopRiver(riverClient, logger))
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = stopInternal(ctx)
	}()
	logger.Info("River worker started", "execution", p.capabilities.Execution, "paused", p.capabilities.Paused, "reporting", p.capabilities.Reporting,
		"housekeeping", p.capabilities.Housekeeping, "discovery", p.capabilities.Discovery, "build_workers", cfg.MaxConcurrentBuilds)
	var stopped error
	select {
	case <-ctx.Done():
	case <-riverClient.Stopped():
		stopped = errors.New("river worker stopped unexpectedly")
	}
	// Tell the server first, so no new request counts on this worker.
	if err := heartbeat.Stop(context.Background()); err != nil {
		logger.Warn("remove worker capabilities", "error", err)
	}
	if stopped != nil {
		return stopped
	}
	return stopRiver(riverClient, logger)
}

// stopRiver lets running jobs finish for drainTimeout, then cancels them.
func stopRiver(c *river.Client[pgx.Tx], logger *slog.Logger) error {
	stopCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	if err := c.Stop(stopCtx); err != nil {
		cancelCtx, cancelJobs := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelJobs()
		if cancelErr := c.StopAndCancel(cancelCtx); cancelErr != nil {
			return fmt.Errorf("stop River worker: %w", errors.Join(err, cancelErr))
		}
		logger.Warn("running jobs were cancelled after the drain period; they resume on the next start")
		return nil
	}
	logger.Info("River worker stopped")
	return nil
}
