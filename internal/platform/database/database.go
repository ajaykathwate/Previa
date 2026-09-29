package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool constructs a pool from configuration and checks that it can connect.
// The caller owns the returned pool and must close it when shutting down.
func NewPool(ctx context.Context, cfg config.Config) (*pgxpool.Pool, error) {
	if cfg.DatabaseURL == "" {
		return nil, errors.New("create database pool: DATABASE_URL must not be empty")
	}
	poolConfig, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database pool configuration: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}
	if err := HealthCheck(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect database pool: %w", err)
	}
	return pool, nil
}

// HealthCheck verifies database connectivity, with a maximum wait of five seconds.
func HealthCheck(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("database health check: pool is nil")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return nil
}
