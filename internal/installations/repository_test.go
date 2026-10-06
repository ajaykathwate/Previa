package installations

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	// The whole schema: the directory query reads pull requests, previews and
	// sync state.
	files, err := filepath.Glob("../../db/migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(data), "-- +goose Down")
		if _, err := pool.Exec(t.Context(), up); err != nil {
			t.Fatalf("%s: %v", file, err)
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

func TestPostgresRepositoryPaginationFiltersAndOwnership(t *testing.T) {
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
	var records []RepositoryInfo
	for i := 1; i <= 150; i++ {
		records = append(records, RepositoryInfo{ID: int64(i), FullName: fmt.Sprintf("owner/repo-%03d", i), Accessible: i%2 == 1})
	}
	if err := repo.Link(ctx, 99, user, 0, "owner", records); err != nil {
		t.Fatal(err)
	}
	var removed []RepositoryInfo
	for _, record := range records {
		if !record.Accessible {
			removed = append(removed, record)
		}
	}
	if err := repo.ApplyEvent(ctx, 99, "active", "owner", nil, removed, false); err != nil {
		t.Fatal(err)
	}
	if err := repo.Link(ctx, 100, other, 0, "other", []RepositoryInfo{{ID: 200, FullName: "other/private", Accessible: true}}); err != nil {
		t.Fatal(err)
	}
	for number := int64(1); number <= 15; number++ {
		page, err := repo.Page(ctx, user, RepositoryFilter{Page: number, Access: "all"})
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != 150 || page.Pages != 15 || len(page.Rows) != 10 || page.Page != number {
			t.Fatalf("incorrect page: %+v", page)
		}
		if page.Rows[0].FullName != fmt.Sprintf("owner/repo-%03d", (number-1)*10+1) {
			t.Fatal("unstable ordering or wrong offset")
		}
	}
	for _, filter := range []RepositoryFilter{{Page: 1, Access: "available"}, {Page: 1, Access: "unavailable"}} {
		page, err := repo.Page(ctx, user, filter)
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != 75 || len(page.Rows) != 10 {
			t.Fatal("access filter did not limit count and rows")
		}
		for _, row := range page.Rows {
			if row.Accessible != (filter.Access == "available") {
				t.Fatal("incorrect access result")
			}
		}
	}
	page, err := repo.Page(ctx, user, RepositoryFilter{Page: 9223372036854775807, Access: "all"})
	if err != nil || page.Page != 15 {
		t.Fatalf("page was not safely clamped: %v", err)
	}
	// Each card counts open PRs and live previews, with build mode and last sync.
	var repoID int64
	if err := pool.QueryRow(ctx, "SELECT id FROM repos WHERE full_name='owner/repo-001'").Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`UPDATE repos SET build_mode='automatic' WHERE id=$1`,
		`INSERT INTO pull_requests(repo_id,number,head_sha,state) VALUES ($1,1,'a','open'),($1,2,'b','open'),($1,3,'c','closed')`,
		`INSERT INTO environments(pull_request_id,subdomain,status,head_sha) SELECT id,'pr-'||number||'-x','running','a' FROM pull_requests WHERE repo_id=$1 AND number=1`,
		`INSERT INTO environments(pull_request_id,subdomain,status) SELECT id,'pr-'||number||'-x','building' FROM pull_requests WHERE repo_id=$1 AND number=2`,
		`INSERT INTO pr_syncs(repo_id,state,last_success_at) VALUES ($1,'complete','2026-10-04T10:00:00Z')`,
	} {
		if _, err := pool.Exec(ctx, sql, repoID); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	first, err := repo.Page(ctx, user, RepositoryFilter{Page: 1, Access: "all"})
	if err != nil {
		t.Fatal(err)
	}
	card := first.Rows[0]
	if card.OpenPRs != 2 || card.LivePreviews != 1 || card.BuildMode != "automatic" || card.LastSynced.IsZero() {
		t.Fatalf("card facts: %+v", card)
	}
	if next := first.Rows[1]; next.OpenPRs != 0 || next.LivePreviews != 0 || next.BuildMode != "manual" || !next.LastSynced.IsZero() {
		t.Fatalf("an untouched repository: %+v", next)
	}
	for _, search := range []string{"REPO-150", "%", "other/private"} {
		page, err := repo.Page(ctx, user, RepositoryFilter{Page: 1, Access: "all", Search: search})
		if err != nil {
			t.Fatal(err)
		}
		expected := int64(0)
		if search == "REPO-150" {
			expected = 1
		}
		if page.Total != expected {
			t.Fatalf("search %q returned %d", search, page.Total)
		}
	}
	summary, err := repo.Summary(ctx, user)
	if err != nil || summary.Installations != 1 || summary.Repositories != 150 || summary.Available != 75 {
		t.Fatalf("wrong scoped summary: %+v %v", summary, err)
	}
	if err := repo.ApplyEvent(ctx, 99, "suspended", "owner", nil, nil, false); err != nil {
		t.Fatal(err)
	}
	page, err = repo.Page(ctx, user, RepositoryFilter{Page: 1, Access: "available"})
	if err != nil || page.Total != 0 {
		t.Fatalf("suspended installation still available: %v", err)
	}
}
