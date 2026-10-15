package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/platform/githubapp"
	"github.com/ajaykathwatee/previa/internal/platform/httpx"
	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/ajaykathwatee/previa/internal/platform/version"
	"github.com/ajaykathwatee/previa/internal/platform/workers"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
	pool, err := openDatabase(ctx, cfg.SharedConfig, "worker", cfg.AppEnv, logger)
	if err != nil {
		return err
	}
	defer pool.Close()
	producer, err := queue.NewProducer(pool, logger)
	if err != nil {
		return err
	}
	in, closeDocker, err := workerInput(ctx, cfg, pool, producer, logger)
	if err != nil {
		return err
	}
	defer closeDocker()
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
	for _, loop := range p.background {
		go loop(ctx) // Ends with the process context.
	}
	checks := append(databaseChecks(pool),
		httpx.Check{Name: "river", Run: func(context.Context) error {
			select {
			case <-riverClient.Stopped():
				return errors.New("stopped")
			default:
				return nil
			}
		}},
		httpx.Check{Name: "heartbeat", Run: func(context.Context) error { return heartbeat.Fresh() }})
	stopInternal, err := httpx.ServeInternal(cfg.InternalAddr, httpx.Internal(append(checks, p.checks...), func() map[string]any {
		return map[string]any{"capabilities": heartbeat.Capabilities(), "version": version.Version}
	}), logger)
	if err != nil {
		return errors.Join(err, heartbeat.Stop(context.Background()), stopRiver(riverClient, logger))
	}
	defer stopWithin(stopInternal, 5*time.Second)
	logger.Info("River worker started", "execution", p.capabilities.Execution, "paused", p.capabilities.Paused, "reporting", p.capabilities.Reporting,
		"housekeeping", p.capabilities.Housekeeping, "discovery", p.capabilities.Discovery, "build_workers", cfg.MaxConcurrentBuilds)
	return runUntilStopped(ctx, riverClient, heartbeat, logger)
}

// workerInput gathers the worker's external dependencies: the GitHub App,
// and Docker when the worker can build (routing configured and the App
// present). The returned close releases Docker.
func workerInput(ctx context.Context, cfg config.WorkerConfig, pool *pgxpool.Pool, producer *river.Client[pgx.Tx], logger *slog.Logger) (pipelineInput, func(), error) {
	in := pipelineInput{cfg: cfg, pool: pool, producer: producer, logger: logger}
	if cfg.GitHubAppID > 0 {
		app, err := githubapp.NewApp(cfg.GitHubAppID, cfg.GitHubAppPrivateKeyPath)
		if err != nil {
			return pipelineInput{}, nil, fmt.Errorf("initialize worker GitHub App: %w", err)
		}
		in.github = app
	}
	if !cfg.RoutingConfigured() || in.github == nil {
		return in, func() {}, nil
	}
	docker, err := dockerx.New(ctx)
	if err != nil {
		return pipelineInput{}, nil, fmt.Errorf("builds need Docker (unset PREVIEW_BASE_DOMAIN to run without builds): %w", err)
	}
	in.docker = docker
	return in, func() { _ = docker.Close() }, nil
}

// runUntilStopped runs until the process is told to stop or River stops on
// its own, then withdraws the worker's capabilities and drains River.
func runUntilStopped(ctx context.Context, riverClient *river.Client[pgx.Tx], heartbeat *workers.Heartbeat, logger *slog.Logger) error {
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
