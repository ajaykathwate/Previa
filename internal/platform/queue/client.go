package queue

import (
	"fmt"
	"log/slog"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// NewProducer creates an insert-only client; the HTTP server never executes jobs.
func NewProducer(pool *pgxpool.Pool, logger *slog.Logger) (*river.Client[pgx.Tx], error) {
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Logger: logger})
	if err != nil {
		return nil, fmt.Errorf("create River producer: %w", err)
	}
	return client, nil
}

// NewWorker registers ping and accepts feature registrations from the composition root.
func NewWorker(pool *pgxpool.Pool, cfg config.WorkerConfig, logger *slog.Logger, registrations ...func(*river.Workers, map[string]river.QueueConfig)) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, &PingWorker{Logger: logger})
	workerCfg := workerConfig(cfg, logger, workers)
	for _, register := range registrations {
		register(workers, workerCfg.Queues)
	}
	client, err := river.NewClient(riverpgxv5.New(pool), workerCfg)
	if err != nil {
		return nil, fmt.Errorf("create River worker: %w", err)
	}
	return client, nil
}

func workerConfig(cfg config.WorkerConfig, logger *slog.Logger, workers *river.Workers) *river.Config {
	return &river.Config{
		Logger: logger,
		Queues: map[string]river.QueueConfig{
			PingQueue: {MaxWorkers: cfg.MaxConcurrentBuilds},
		},
		Workers: workers,
		// The scan floor is not a blanket rescue deadline: River also consults
		// each registered worker's Timeout before rescuing that job kind.
		JobTimeout:           PingTimeout,
		RescueStuckJobsAfter: RescueScanFloor,
		MaxAttempts:          5,
	}
}
