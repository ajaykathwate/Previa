package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/platform/httpx"
	"github.com/ajaykathwatee/previa/internal/platform/version"
	"github.com/jackc/pgx/v5/pgxpool"
)

// openDatabase connects a process to PostgreSQL and refuses to go on with an
// older schema than this build needs ("run migrations first").
func openDatabase(ctx context.Context, cfg config.SharedConfig, process, environment string, logger *slog.Logger) (*pgxpool.Pool, error) {
	pool, err := database.NewPool(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("initialize %s database: %w", process, err)
	}
	if err := database.CheckSchema(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	logger.Info("database health check passed", "version", version.Version, "schema", database.SchemaVersion, "environment", environment)
	return pool, nil
}

// databaseChecks are the readiness checks every process with a database runs.
func databaseChecks(pool *pgxpool.Pool) []httpx.Check {
	return []httpx.Check{
		{Name: "database", Run: func(ctx context.Context) error { return database.HealthCheck(ctx, pool) }},
		{Name: "schema", Run: func(ctx context.Context) error { return database.CheckSchema(ctx, pool) }},
	}
}

// stopWithin calls stop with a deadline, for deferred shutdown of a listener.
func stopWithin(stop func(context.Context) error, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_ = stop(ctx)
}
