package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func isolatedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for isolated database tests")
	}
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("runtime_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		defer admin.Close()
		if _, err := admin.Exec(c, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
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
	files, err := filepath.Glob("../../db/migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
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

func TestTargetStoreReadsTheCurrentAuthorizedTarget(t *testing.T) {
	pool := isolatedPool(t)
	ctx := t.Context()
	var owner, install, repo, pr int64
	scan := func(dst *int64, sql string, args ...any) {
		t.Helper()
		if err := pool.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	scan(&owner, `INSERT INTO users(github_id,username) VALUES (42,'owner') RETURNING id`)
	scan(&install, `INSERT INTO installations(github_installation_id,owner_user_id) VALUES (99,$1) RETURNING id`, owner)
	scan(&repo, `INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES ($1,100,'owner/app') RETURNING id`, install)
	scan(&pr, `INSERT INTO pull_requests(repo_id,number,head_sha,generation,head_repo_id) VALUES ($1,7,'sha-1',4,100) RETURNING id`, repo)
	if _, err := pool.Exec(ctx, `INSERT INTO build_intents(pull_request_id,revision,base_revision,generation,head_sha,source,request_action,requested_by,idempotency_key,state,available_at,started_at)
		VALUES ($1,2,1,4,'sha-1','manual','build',$2,'fixture-key-0000001','running',now(),now())`, pr, owner); err != nil {
		t.Fatal(err)
	}
	store := NewTargetStore(pool)
	request := environments.Request{PullRequestID: pr, Generation: 4, Revision: 2, HeadSHA: "sha-1", State: "open"}
	target, err := store.Current(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if target.RepoID != repo || target.InstallationID != 99 || target.BaseRepoID != 100 || target.HeadRepoID != 100 || target.Fork || target.Number != 7 {
		t.Fatalf("target: %+v", target)
	}
	if _, err := pool.Exec(ctx, "UPDATE pull_requests SET head_repo_id = 555, is_fork = true WHERE id = $1", pr); err != nil {
		t.Fatal(err)
	}
	if target, err := store.Current(ctx, request); err != nil || !target.Fork || target.secretSource().Trusted() {
		t.Fatalf("fork target: %+v %v", target, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE repos SET accessible = false WHERE id = $1", repo); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Current(ctx, request); CodeOf(err) != CodeInaccessible {
		t.Fatalf("revoked access: %v", err)
	}
	stale := request
	stale.Revision = 1
	if _, err := store.Current(ctx, stale); CodeOf(err) != CodeNotCurrent {
		t.Fatalf("stale revision: %v", err)
	}
	if _, err := store.Current(ctx, environments.Request{PullRequestID: pr + 1000, Generation: 4, Revision: 2, HeadSHA: "sha-1", State: "open"}); CodeOf(err) != CodeNotCurrent {
		t.Fatalf("missing PR: %v", err)
	}
}
