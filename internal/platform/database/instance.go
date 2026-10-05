package database

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Instance returns this database's identity for the Docker objects it creates
// (migration 0016). It is stable for the database's lifetime.
func Instance(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	if pool == nil {
		return "", errors.New("read platform instance: pool is nil")
	}
	var instance string
	if err := pool.QueryRow(ctx, "SELECT instance FROM platform_instance").Scan(&instance); err != nil {
		return "", fmt.Errorf("read platform instance (run make migrate-up): %w", err)
	}
	return instance, nil
}
