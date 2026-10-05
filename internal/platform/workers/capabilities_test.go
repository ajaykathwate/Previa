package workers

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required")
	}
	admin, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("workers_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	p, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	files, err := filepath.Glob("../../../db/migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(data), "-- +goose Down")
		if _, err := p.Exec(t.Context(), up); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
	}
	return p
}

func TestCapabilitiesComeFromLiveWorkersOnly(t *testing.T) {
	p := pool(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reader := NewReader(p)
	if c, err := reader.Current(t.Context()); err != nil || c != (Capabilities{}) {
		t.Fatalf("no workers: %+v %v", c, err)
	}
	discovery, err := Start(t.Context(), p, Capabilities{Discovery: true}, logger)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := Start(t.Context(), p, Capabilities{Execution: true, Routing: true, Reporting: true, Housekeeping: true}, logger)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := reader.Current(t.Context()); err != nil || !c.Execution || !c.Discovery || !c.Reporting {
		t.Fatalf("two workers: %+v %v", c, err)
	}
	// A worker that stopped answering no longer counts.
	if _, err := p.Exec(t.Context(), "UPDATE worker_capabilities SET seen_at = now() - interval '10 minutes' WHERE worker_id = $1", executor.id); err != nil {
		t.Fatal(err)
	}
	if c, _ := reader.Current(t.Context()); c.Execution || !c.Discovery {
		t.Fatalf("a silent worker still counts: %+v", c)
	}
	if err := executor.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := discovery.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if c, _ := reader.Current(t.Context()); c != (Capabilities{}) {
		t.Fatalf("stopped workers still count: %+v", c)
	}
}

func TestPausedWorkersSayWhyNothingBuilds(t *testing.T) {
	p := pool(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reader := NewReader(p)
	paused, err := Start(t.Context(), p, Capabilities{Routing: true, Reporting: true, Housekeeping: true, Discovery: true, Paused: true}, logger)
	if err != nil {
		t.Fatal(err)
	}
	c, live, err := reader.Live(t.Context())
	if err != nil || live != 1 || c.Execution || !c.Paused || !c.Housekeeping {
		t.Fatalf("paused worker: %+v live=%d %v", c, live, err)
	}
	// Another worker that can build means builds are not paused overall.
	executor, err := Start(t.Context(), p, Capabilities{Execution: true}, logger)
	if err != nil {
		t.Fatal(err)
	}
	if c, live, _ := reader.Live(t.Context()); !c.Execution || c.Paused || live != 2 {
		t.Fatalf("with a building worker: %+v live=%d", c, live)
	}
	if err := executor.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := paused.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
}
