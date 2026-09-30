package app

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRiverMigrationRejectsArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"down"}, {"up", "extra"}} {
		if err := RunRiverMigration(t.Context(), args, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted invalid arguments %v", args)
		}
	}
}

func TestRiverMigrationFromEnvironmentIntegration(t *testing.T) {
	connection := os.Getenv("TEST_DATABASE_URL")
	if connection == "" {
		t.Skip("set TEST_DATABASE_URL to verify migrations in an isolated schema")
	}
	pool, err := pgxpool.New(t.Context(), connection)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	schema := fmt.Sprintf("river_migration_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := pool.Exec(t.Context(), "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("clean migration schema: %v", err)
		}
	}()
	u, err := url.Parse(connection)
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	t.Setenv("DATABASE_URL", u.String())
	for _, key := range []string{"GITHUB_OAUTH_CLIENT_ID", "GITHUB_OAUTH_CLIENT_SECRET", "SESSION_SECRET", "MAX_CONCURRENT_BUILDS"} {
		t.Setenv(key, "")
	}
	var out bytes.Buffer
	if err := RunRiverMigration(t.Context(), []string{"status"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "001 pending") {
		t.Fatalf("fresh schema status: %s", &out)
	}
	out.Reset()
	if err := RunRiverMigration(t.Context(), []string{"up"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "007 applied") {
		t.Fatalf("incomplete migration: %s", &out)
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if fields := strings.Fields(line); len(fields) > 1 && fields[1] == "pending" {
			t.Fatalf("migration still pending: %s", line)
		}
	}
	out.Reset()
	if err := RunRiverMigration(t.Context(), []string{"up"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "applied 0 migrations") {
		t.Fatalf("rerun was not a no-op: %s", &out)
	}
}
