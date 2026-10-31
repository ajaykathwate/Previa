package workspaces

import (
	"os"
	"strings"
	"testing"
)

func workspaceMigration(t *testing.T) (string, string) {
	t.Helper()
	b, err := os.ReadFile("../../db/migrations/0030_workspaces.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(b), "-- +goose Down")
	if !ok {
		t.Fatal("missing rollback")
	}
	return up, down
}
func TestMigrationBackfillsConnectionsPreviewsAndUsageWithoutChangingIDs(t *testing.T) {
	pool := isolatedPoolBefore(t, true)
	ctx := t.Context()
	up, down := workspaceMigration(t)
	for _, sql := range []string{
		"INSERT INTO users(github_id,username) VALUES(10,'legacy')",
		"INSERT INTO installations(github_installation_id,owner_user_id) VALUES(20,1),(21,1)",
		"INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES(1,30,'acme/api')",
		"INSERT INTO pull_requests(repo_id,number,head_sha) VALUES(1,1,'sha')",
		"INSERT INTO environments(pull_request_id,subdomain,status,expires_at) VALUES(1,'stable-host','running',now()+interval '1 day')",
		"INSERT INTO account_build_usage(user_id,day,build_seconds,builds) VALUES(1,current_date,60,1)",
		up,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	var count, workspaces, seconds int64
	var hostname string
	var allowed bool
	if err := pool.QueryRow(ctx, "SELECT count(*),count(DISTINCT workspace_id) FROM installations WHERE owner_user_id=1 AND workspace_id IS NOT NULL").Scan(&count, &workspaces); err != nil || count != 2 || workspaces != 1 {
		t.Fatalf("backfill: %d %d %v", count, workspaces, err)
	}
	if err := pool.QueryRow(ctx, "SELECT subdomain FROM environments WHERE id=1 AND pull_request_id=1").Scan(&hostname); err != nil || hostname != "stable-host" {
		t.Fatal(hostname, err)
	}
	if err := pool.QueryRow(ctx, "SELECT workspace_repository_access(1,1,'operate')").Scan(&allowed); err != nil || !allowed {
		t.Fatal(allowed, err)
	}
	if err := pool.QueryRow(ctx, "SELECT build_seconds FROM workspace_build_usage").Scan(&seconds); err != nil || seconds != 60 {
		t.Fatal(seconds, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE workspace_build_usage SET build_seconds=90,builds=2"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatal("safe solo rollback", err)
	}
	if err := pool.QueryRow(ctx, "SELECT build_seconds FROM account_build_usage WHERE user_id=1").Scan(&seconds); err != nil || seconds != 90 {
		t.Fatal("rollback lost new consumption", seconds, err)
	}
	if _, err := pool.Exec(ctx, up); err != nil {
		t.Fatal("reapply", err)
	}
}
func TestMigrationRejectsLossyTeamRollback(t *testing.T) {
	f := newFixture(t)
	f.invite(t, "reviewer", "reviewer", f.reviewer, 104, []int64{f.repo})
	_, down := workspaceMigration(t)
	if _, err := f.pool.Exec(t.Context(), down); err == nil || !strings.Contains(err.Error(), "rollback would discard team data") {
		t.Fatal("lossy rollback allowed", err)
	}
	if _, err := f.service.Read(t.Context(), f.reviewer, f.wid); err != nil {
		t.Fatal("failed rollback damaged team", err)
	}
}
