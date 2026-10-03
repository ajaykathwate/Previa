package installations

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func installationDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	connection := os.Getenv("TEST_DATABASE_URL")
	if connection == "" {
		t.Skip("set TEST_DATABASE_URL for installation persistence checks")
	}
	admin, err := pgxpool.New(t.Context(), connection)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("installation_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+quoted); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		defer admin.Close()
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(connection)
	if err != nil {
		t.Fatal("invalid test connection")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, name := range []string{"0001_init.sql", "0005_installation_linking.sql"} {
		data, err := os.ReadFile("../../db/migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(data), "-- +goose Down")
		if _, err := pool.Exec(t.Context(), up); err != nil {
			t.Fatal(err)
		}
	}
	return pool
}
func TestPostgresInstallationOwnershipLifecycleAndRevisionFence(t *testing.T) {
	pool := installationDatabase(t)
	repo := NewRepository(pool)
	ctx := t.Context()
	var user, other int64
	if err := pool.QueryRow(ctx, "INSERT INTO users(github_id,username) VALUES(42,'owner') RETURNING id").Scan(&user); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "INSERT INTO users(github_id,username) VALUES(43,'other') RETURNING id").Scan(&other); err != nil {
		t.Fatal(err)
	}
	revision, err := repo.Prepare(ctx, 99)
	if err != nil || revision != 0 {
		t.Fatalf("initial revision: %v", err)
	}
	repos := []RepositoryInfo{{ID: 456, FullName: "owner/repo", Accessible: true}}
	// Delivery before redirect stores an unlinked row, hidden from both users.
	if err := repo.ApplyEvent(ctx, 99, "active", "owner", repos, nil, true); err != nil {
		t.Fatal(err)
	}
	if list, err := repo.List(ctx, user); err != nil || len(list) != 0 {
		t.Fatal("unlinked installation leaked")
	}
	if err := repo.Link(ctx, 99, user, revision, "owner", repos); !errors.Is(err, ErrSuperseded) {
		t.Fatal("stale callback overwrote lifecycle event")
	}
	revision, err = repo.Prepare(ctx, 99)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Link(ctx, 99, user, revision, "owner", repos); err != nil {
		t.Fatal(err)
	}
	revision, err = repo.Prepare(ctx, 99)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Link(ctx, 99, other, revision, "owner", repos); !errors.Is(err, ErrForbidden) {
		t.Fatal("installation ownership changed")
	}
	if list, err := repo.List(ctx, other); err != nil || len(list) != 0 {
		t.Fatal("other user's installations leaked")
	}
	// Stable repository identity preserves PR history across renames/removal.
	var localRepo int64
	if err := pool.QueryRow(ctx, "SELECT id FROM repos WHERE github_repo_id=456").Scan(&localRepo); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO pull_requests(repo_id,number,head_sha) VALUES($1,1,'sha')", localRepo); err != nil {
		t.Fatal(err)
	}
	if err := repo.ApplyEvent(ctx, 99, "", "owner", []RepositoryInfo{{ID: 456, FullName: "owner/renamed"}}, nil, false); err != nil {
		t.Fatal(err)
	}
	var renamedID int64
	if err := pool.QueryRow(ctx, "SELECT id FROM repos WHERE full_name='owner/renamed'").Scan(&renamedID); err != nil || renamedID != localRepo {
		t.Fatal("rename lost repository identity")
	}
	if err := repo.ApplyEvent(ctx, 99, "suspended", "owner", nil, nil, false); err != nil {
		t.Fatal(err)
	}
	list, err := repo.List(ctx, user)
	if err != nil || list[0].Repositories[0].Accessible {
		t.Fatal("suspended installation still accessible")
	}
	if err := repo.ApplyEvent(ctx, 99, "active", "owner", nil, nil, false); err != nil {
		t.Fatal(err)
	}
	list, err = repo.List(ctx, user)
	if err != nil || !list[0].Repositories[0].Accessible {
		t.Fatal("unsuspend did not restore repository selection")
	}
	if err := repo.ApplyEvent(ctx, 99, "", "owner", nil, []RepositoryInfo{{ID: 456, FullName: "owner/renamed"}}, false); err != nil {
		t.Fatal(err)
	}
	list, err = repo.List(ctx, user)
	if err != nil || list[0].Repositories[0].Accessible {
		t.Fatal("removed repository access remained")
	}
	revision, err = repo.Prepare(ctx, 99)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ApplyEvent(ctx, 99, "inactive", "owner", nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := repo.Link(ctx, 99, user, revision, "owner", repos); !errors.Is(err, ErrSuperseded) {
		t.Fatal("close during callback was overwritten")
	}
	if err := repo.ApplyEvent(ctx, 99, "active", "owner", repos, nil, true); err != nil {
		t.Fatal(err)
	}
	list, err = repo.List(ctx, user)
	if err != nil || list[0].Status != "inactive" || list[0].Repositories[0].Accessible {
		t.Fatal("late created event revived deleted installation")
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM pull_requests WHERE repo_id=$1", localRepo).Scan(&count); err != nil || count != 1 {
		t.Fatal("lifecycle processing removed PR history")
	}
	// Reinstalling the same repository can preserve history for the same owner.
	if err := repo.ApplyEvent(ctx, 100, "active", "owner", repos, nil, true); err != nil {
		t.Fatal(err)
	}
	revision, err = repo.Prepare(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Link(ctx, 100, other, revision, "owner", repos); !errors.Is(err, ErrForbidden) {
		t.Fatal("reinstall reassigned another owner's repository")
	}
	if err := repo.Link(ctx, 100, user, revision, "owner", repos); err != nil {
		t.Fatal(err)
	}
	var reboundID int64
	if err := pool.QueryRow(ctx, "SELECT id FROM repos WHERE github_repo_id=456").Scan(&reboundID); err != nil || reboundID != localRepo {
		t.Fatal("reinstall lost repository history")
	}

}
