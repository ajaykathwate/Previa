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
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
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

func TestAdmissionLockSerializesAndSurvivesCancellation(t *testing.T) {
	pool := isolatedPool(t)
	lock := NewAdmissionLock(pool)
	unlock, err := lock.Lock(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// A second admission waits; giving up while waiting leaks nothing.
	waitCtx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	if _, err := lock.Lock(waitCtx); err == nil {
		t.Fatal("a second admission did not wait")
	}
	cancel()
	acquired := make(chan func(), 1)
	go func() {
		next, err := lock.Lock(t.Context())
		if err != nil {
			t.Error(err)
			close(acquired)
			return
		}
		acquired <- next
	}()
	select {
	case <-acquired:
		t.Fatal("admission granted while held")
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	select {
	case next := <-acquired:
		if next != nil {
			next()
		}
	case <-time.After(5 * time.Second):
		t.Fatal("admission not granted after release")
	}
	// Every connection went back unlocked: the lock is free again.
	final, err := lock.Lock(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	final()
}

func TestRecordComponentsKeepsOnlyServedRows(t *testing.T) {
	pool := isolatedPool(t)
	ctx := t.Context()
	store := NewTargetStore(pool)
	var user, installation, repo, pr int64
	for _, q := range []struct {
		sql  string
		dest *int64
		args []any
	}{
		{"INSERT INTO users(github_id, username) VALUES (91, 'c') RETURNING id", &user, nil},
		{"INSERT INTO installations(github_installation_id, owner_user_id) VALUES (91, $1) RETURNING id", &installation, []any{&user}},
		{"INSERT INTO repos(installation_id, full_name, github_repo_id) VALUES ($1, 'c/r', 91) RETURNING id", &repo, []any{&installation}},
		{"INSERT INTO pull_requests(repo_id, number, head_sha, state, generation) VALUES ($1, 1, 'sha', 'open', 3) RETURNING id", &pr, []any{&repo}},
	} {
		args := make([]any, len(q.args))
		for i, a := range q.args {
			args[i] = *(a.(*int64))
		}
		if err := pool.QueryRow(ctx, q.sql, args...).Scan(q.dest); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, "INSERT INTO environments(pull_request_id, subdomain, status, applied_generation, applied_revision) VALUES ($1, 'pr-1-c-1', 'running', 1, 1)", pr); err != nil {
		t.Fatal(err)
	}
	record := func(generation, revision int64) {
		t.Helper()
		if err := store.RecordComponents(ctx, Components{PullRequestID: pr, Generation: generation, Revision: revision, Services: []ServiceComponent{{Kind: "postgres", Version: "17"}}, Setup: true, Processes: []string{"worker"}, DocsPath: "/docs", Outbound: 1}); err != nil {
			t.Fatal(err)
		}
	}
	record(1, 1) // deployed
	record(2, 1) // superseded, never deployed
	record(3, 1) // the newest
	var rows []string
	r, err := pool.Query(ctx, "SELECT generation || '.' || revision FROM preview_components WHERE pull_request_id = $1 ORDER BY 1", pr)
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
		var row string
		_ = r.Scan(&row)
		rows = append(rows, row)
	}
	if strings.Join(rows, ",") != "1.1,3.1" {
		t.Fatalf("rows kept: %v", rows)
	}
}

func TestLegacyContainerAccountLabelResolvesStableWorkspaceAfterHandover(t *testing.T) {
	pool := isolatedPool(t)
	ctx := t.Context()
	for _, sql := range []string{
		"INSERT INTO users(github_id,username) VALUES(11,'next'),(12,'installer')",
		"INSERT INTO installations(github_installation_id,owner_user_id) VALUES(99,2)",
		"INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES(1,100,'team/app')",
		"INSERT INTO pull_requests(repo_id,number,head_sha) VALUES(1,1,'sha')",
		"INSERT INTO workspace_memberships(workspace_id,user_id,role) VALUES(1,1,'admin')",
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	rt := &Runtime{targets: NewTargetStore(pool)}
	labels := map[string]string{dockerx.LabelAccount: "2", dockerx.LabelPR: "1"}
	before, err := rt.containerWorkspace(ctx, labels, map[string]int64{})
	if err != nil || before != 1 {
		t.Fatalf("legacy user counted as workspace: %d %v", before, err)
	}
	if _, err := pool.Exec(ctx, "BEGIN; UPDATE workspace_memberships SET role='admin' WHERE workspace_id=1 AND user_id=2; UPDATE workspace_memberships SET role='owner' WHERE workspace_id=1 AND user_id=1; UPDATE workspaces SET owner_user_id=1 WHERE id=1; COMMIT;"); err != nil {
		t.Fatal(err)
	}
	after, err := rt.containerWorkspace(ctx, labels, map[string]int64{})
	if err != nil || after != before {
		t.Fatalf("handover moved consumption: %d %v", after, err)
	}
}
