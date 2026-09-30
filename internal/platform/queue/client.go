package queue

import (
	"fmt"
	"log/slog"
	"time"

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

// NewWorker registers only the ping worker and its isolated queue.
func NewWorker(pool *pgxpool.Pool, cfg config.Config, logger *slog.Logger) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, &PingWorker{Logger: logger})
	client, err := river.NewClient(riverpgxv5.New(pool), workerConfig(cfg, logger, workers))
	if err != nil {
		return nil, fmt.Errorf("create River worker: %w", err)
	}
	return client, nil
}

func workerConfig(cfg config.Config, logger *slog.Logger, workers *river.Workers) *river.Config {
	return &river.Config{
		Logger: logger,
		Queues: map[string]river.QueueConfig{
			PingQueue: {MaxWorkers: cfg.MaxConcurrentBuilds},
		},
		Workers:              workers,
		JobTimeout:           10 * time.Second,
		RescueStuckJobsAfter: time.Minute,
		MaxAttempts:          5,
	}
}
