package database

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Run after make migrate-up. Each test transaction is rolled back.
func TestMigrationConstraintsIntegration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set TEST_DATABASE_URL to test the migrated PostgreSQL schema")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, err := NewPool(ctx, config.SharedConfig{DatabaseURL: databaseURL})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		rollbackCtx, cancelRollback := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelRollback()
		if err := tx.Rollback(rollbackCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("rollback migration test: %v", err)
		}
	}()

	insertID := func(query string, args ...any) int64 {
		t.Helper()
		var id int64
		if err := tx.QueryRow(ctx, query, args...).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	externalID := time.Now().UnixNano()
	suffix := strconv.FormatInt(externalID, 10)
	subdomain := "migration-test-" + suffix
	userID := insertID("INSERT INTO users (github_id, username) VALUES ($1, 'migration-test') RETURNING id", externalID)
	installationID := insertID("INSERT INTO installations (github_installation_id, owner_user_id) VALUES ($1, $2) RETURNING id", externalID, userID)
	repoID := insertID("INSERT INTO repos (installation_id, full_name) VALUES ($1, $2) RETURNING id", installationID, "migration-test/"+suffix)
	prID := insertID("INSERT INTO pull_requests (repo_id, number, head_sha) VALUES ($1, 1, 'test-sha') RETURNING id", repoID)
	otherPRID := insertID("INSERT INTO pull_requests (repo_id, number, head_sha) VALUES ($1, 2, 'test-sha') RETURNING id", repoID)
	envID := insertID("INSERT INTO environments (pull_request_id, subdomain, expires_at) VALUES ($1, $2, now() + interval '1 day') RETURNING id", prID, subdomain)

	for _, tt := range []struct {
		name, query, code string
		args              []any
	}{
		{"duplicate PR", "INSERT INTO pull_requests (repo_id, number, head_sha) VALUES ($1, 1, 'other-sha')", "23505", []any{repoID}},
		{"active environment", "INSERT INTO environments (pull_request_id, subdomain, expires_at) VALUES ($1, $2, now() + interval '1 day')", "23505", []any{prID, "other-" + suffix}},
		{"global subdomain", "INSERT INTO environments (pull_request_id, subdomain, expires_at) VALUES ($1, $2, now() + interval '1 day')", "23505", []any{otherPRID, subdomain}},
		{"unknown status", "UPDATE environments SET status = 'ready' WHERE id = $1", "23514", []any{envID}},
		{"null status", "UPDATE environments SET status = NULL WHERE id = $1", "23502", []any{envID}},
		{"invalid expiry", "UPDATE environments SET expires_at = created_at WHERE id = $1", "23514", []any{envID}},
		{"missing parent", "INSERT INTO build_logs (environment_id, sequence, line) VALUES (-1, 0, 'test')", "23503", nil},
		{"duplicate log sequence", "INSERT INTO build_logs (environment_id, sequence, line) VALUES ($1, 0, 'first'), ($1, 0, 'duplicate')", "23505", []any{envID}},
		{"duplicate secret", "INSERT INTO env_secrets (repo_id, key, encrypted_value) VALUES ($1, 'TOKEN', decode('0102', 'hex')), ($1, 'TOKEN', decode('0304', 'hex'))", "23505", []any{repoID}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tx.Exec(ctx, "SAVEPOINT constraint_test"); err != nil {
				t.Fatal(err)
			}
			_, err := tx.Exec(ctx, tt.query, tt.args...)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != tt.code {
				t.Errorf("expected PostgreSQL error %s, got %v", tt.code, err)
			}
			if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT constraint_test"); err != nil {
				t.Fatal(err)
			}
		})
	}

	for _, status := range []string{"queued", "building", "running", "failed", "tearing_down", "torn_down"} {
		t.Run(status, func(t *testing.T) {
			if _, err := tx.Exec(ctx, "UPDATE environments SET status = $1 WHERE id = $2", status, envID); err != nil {
				t.Fatalf("valid status rejected: %v", err)
			}
			if _, err := tx.Exec(ctx, "SAVEPOINT active_test"); err != nil {
				t.Fatal(err)
			}
			_, err := tx.Exec(ctx, "INSERT INTO environments (pull_request_id, subdomain, expires_at) VALUES ($1, $2, now() + interval '1 day')", prID, "replacement-"+suffix)
			if status == "torn_down" {
				if err != nil {
					t.Errorf("completed teardown must release the active slot: %v", err)
				}
			} else {
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
					t.Errorf("status %s must retain the active slot, got %v", status, err)
				}
			}
			if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT active_test"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
