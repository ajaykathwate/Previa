package activity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// isolatedPool migrates a fresh schema of TEST_DATABASE_URL.
func isolatedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for activity database tests")
	}
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("activity_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		defer admin.Close()
		_, _ = admin.Exec(c, "DROP SCHEMA "+quoted+" CASCADE")
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	files, _ := filepath.Glob("../../db/migrations/*.sql")
	sort.Strings(files)
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(b), "-- +goose Down")
		if _, err := pool.Exec(ctx, up); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
	}
	return pool
}

// twoPullRequests is one workspace's repository with two pull requests, and
// a pull request of an installation that belongs to no workspace.
func twoPullRequests(t *testing.T, pool *pgxpool.Pool) (first, second, orphan int64) {
	t.Helper()
	ctx := t.Context()
	var user, installation, repo, loose, looseRepo int64
	for _, step := range []struct {
		dst   *int64
		query string
		args  []any
	}{
		{&user, "INSERT INTO users(github_id,username) VALUES(1,'owner') RETURNING id", nil},
		{&installation, "INSERT INTO installations(github_installation_id,owner_user_id) VALUES(1,$1) RETURNING id", []any{&user}},
		{&repo, "INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES($1,10,'acme/api') RETURNING id", []any{&installation}},
		{&first, "INSERT INTO pull_requests(repo_id,number,head_sha) VALUES($1,1,'a') RETURNING id", []any{&repo}},
		{&second, "INSERT INTO pull_requests(repo_id,number,head_sha) VALUES($1,2,'a') RETURNING id", []any{&repo}},
		{&loose, "INSERT INTO installations(github_installation_id) VALUES(2) RETURNING id", nil},
		{&looseRepo, "INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES($1,11,'loose/app') RETURNING id", []any{&loose}},
		{&orphan, "INSERT INTO pull_requests(repo_id,number,head_sha) VALUES($1,3,'a') RETURNING id", []any{&looseRepo}},
	} {
		args := make([]any, len(step.args))
		for i, a := range step.args {
			args[i] = *(a.(*int64))
		}
		if err := pool.QueryRow(ctx, step.query, args...).Scan(step.dst); err != nil {
			t.Fatalf("%s: %v", step.query, err)
		}
	}
	return first, second, orphan
}

func record(t *testing.T, pool *pgxpool.Pool, e Event) {
	t.Helper()
	if err := database.InTransaction(t.Context(), pool, func(tx pgx.Tx) error { return NewRecorder().RecordTx(t.Context(), tx, e) }); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, pool *pgxpool.Pool, where string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM activity_events WHERE "+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEventsAreRecordedOnceAndStaySmall(t *testing.T) {
	pool := isolatedPool(t)
	first, _, orphan := twoPullRequests(t, pool)
	e := Event{PullRequestID: first, Kind: "feedback.thread_created", Metadata: map[string]any{"finding": 1}, DedupKey: "finding:1:created"}
	record(t, pool, e)
	record(t, pool, e)
	if n := count(t, pool, "pull_request_id=$1", first); n != 1 {
		t.Fatalf("a retried write records one event, got %d", n)
	}
	var workspace, repo int64
	var actor *int64
	if err := pool.QueryRow(t.Context(), "SELECT workspace_id, repo_id, actor_user_id FROM activity_events WHERE pull_request_id=$1", first).Scan(&workspace, &repo, &actor); err != nil || workspace == 0 || repo == 0 || actor != nil {
		t.Fatalf("scope derives from the pull request and no actor means the system: %d %d %v %v", workspace, repo, actor, err)
	}
	record(t, pool, Event{PullRequestID: orphan, Kind: "feedback.thread_created"})
	if n := count(t, pool, "pull_request_id=$1", orphan); n != 0 {
		t.Fatalf("a pull request outside every workspace has no timeline, got %d", n)
	}
	for name, bad := range map[string]Event{
		"a malformed kind":   {PullRequestID: first, Kind: "Created"},
		"no pull request":    {Kind: "feedback.thread_created"},
		"oversized metadata": {PullRequestID: first, Kind: "feedback.thread_created", Metadata: map[string]any{"text": strings.Repeat("x", MaxMetadata)}},
	} {
		err := database.InTransaction(t.Context(), pool, func(tx pgx.Tx) error { return NewRecorder().RecordTx(t.Context(), tx, bad) })
		if err == nil {
			t.Errorf("%s was recorded", name)
		}
	}
}

func TestPruningKeepsNinetyDaysAndACapPerPullRequest(t *testing.T) {
	pool := isolatedPool(t)
	first, second, _ := twoPullRequests(t, pool)
	for i := range 12 {
		record(t, pool, Event{PullRequestID: first, Kind: "feedback.reply_added", DedupKey: fmt.Sprintf("first:%d", i)})
	}
	for i := range 3 {
		record(t, pool, Event{PullRequestID: second, Kind: "feedback.reply_added", DedupKey: fmt.Sprintf("second:%d", i)})
	}
	// Two of the second pull request's events are past retention.
	if _, err := pool.Exec(t.Context(), "UPDATE activity_events SET occurred_at=now()-interval '91 days' WHERE dedup_key IN ('second:0','second:1')"); err != nil {
		t.Fatal(err)
	}
	p := NewPruner(pool)
	p.perPullRequest, p.batch = 5, 2
	deleted, err := p.Prune(t.Context())
	if err != nil || deleted != 2+7 {
		t.Fatalf("deleted %d: %v", deleted, err)
	}
	if n := count(t, pool, "pull_request_id=$1", first); n != 5 {
		t.Fatalf("the cap keeps the newest five, kept %d", n)
	}
	if n := count(t, pool, "pull_request_id=$1 AND dedup_key IN ('first:11','first:7')", first); n != 2 {
		t.Fatalf("the newest survive, got %d", n)
	}
	if n := count(t, pool, "pull_request_id=$1", second); n != 1 {
		t.Fatalf("old events go, kept %d", n)
	}
	var marks int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM activity_pruned WHERE pull_request_id IN ($1,$2)", first, second).Scan(&marks); err != nil || marks != 2 {
		t.Fatalf("each pruned pull request says older history was removed: %d %v", marks, err)
	}
	if again, err := p.Prune(t.Context()); err != nil || again != 0 {
		t.Fatalf("a second pass has nothing to do: %d %v", again, err)
	}
}
