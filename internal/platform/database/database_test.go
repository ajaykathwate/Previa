package database

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/config"
)

func TestNewPoolRejectsInvalidConfiguration(t *testing.T) {
	for _, databaseURL := range []string{"", "postgres://localhost:invalid/db"} {
		pool, err := NewPool(t.Context(), config.Config{DatabaseURL: databaseURL})
		if pool != nil {
			pool.Close()
			t.Fatal("unexpected pool for invalid configuration")
		}
		if err == nil {
			t.Fatal("expected invalid configuration error")
		}
	}
}

func TestNewPoolHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	pool, err := NewPool(ctx, config.Config{DatabaseURL: "postgres://localhost/previa?sslmode=disable"})
	if pool != nil {
		pool.Close()
		t.Fatal("unexpected pool with canceled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
}

func TestHealthCheckRejectsNilPool(t *testing.T) {
	if err := HealthCheck(t.Context(), nil); err == nil {
		t.Fatal("expected error for nil pool")
	}
}

func TestPoolIntegration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set TEST_DATABASE_URL to test against local PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	pool, err := NewPool(ctx, config.Config{DatabaseURL: databaseURL})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := HealthCheck(ctx, pool); err != nil {
		t.Fatal(err)
	}
	canceled, cancelPing := context.WithCancel(ctx)
	cancelPing()
	if err := HealthCheck(canceled, pool); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected health check cancellation, got %v", err)
	}
	pool.Close()
	if err := HealthCheck(ctx, pool); err == nil {
		t.Fatal("health check unexpectedly passed for closed pool")
	}
}
