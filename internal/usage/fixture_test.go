package usage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fixture struct {
	pool                   *pgxpool.Pool
	store                  *Store
	workspace, owner, repo int64
	now                    time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL for isolated usage integration tests")
	}
	admin, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "usage_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(t.Context(), "CREATE SCHEMA "+quoted); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		defer admin.Close()
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 12
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	files, err := filepath.Glob("../../db/migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(b), "-- +goose Down")
		if _, err = pool.Exec(t.Context(), up); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
	}
	f := &fixture{pool: pool, now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	// These are deliberately small test allowances, not product defaults.
	f.exec(t, `UPDATE free_plan_policy SET period_kind='month',included_seconds=600,concurrent_builds=2,preview_slots=2`)
	f.owner = f.id(t, `INSERT INTO users(github_id,username) VALUES(101,'owner') RETURNING id`)
	installation := f.id(t, `INSERT INTO installations(github_installation_id,owner_user_id,account_login) VALUES(7,$1,'acme') RETURNING id`, f.owner)
	f.workspace = f.id(t, `SELECT workspace_id FROM installations WHERE id=$1`, installation)
	f.repo = f.id(t, `INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES($1,100,'acme/api') RETURNING id`, installation)
	f.store = NewStore(pool, 0, 0)
	f.store.now = func() time.Time { return f.now }
	return f
}
func (f *fixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}
func (f *fixture) id(t *testing.T, sql string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := f.pool.QueryRow(t.Context(), sql, args...).Scan(&id); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return id
}
func (f *fixture) pr(t *testing.T, number int) int64 {
	t.Helper()
	pr := f.id(t, `INSERT INTO pull_requests(repo_id,number,head_sha) VALUES($1,$2,'sha') RETURNING id`, f.repo, number)
	f.exec(t, `INSERT INTO build_intents(pull_request_id,workspace_id,revision,base_revision,generation,head_sha,source,request_action,idempotency_key,state,available_at)
        SELECT id,$2,1,0,generation,head_sha,'automatic','automatic','usage-test-'||id,'queued',now() FROM pull_requests WHERE id=$1`, pr, f.workspace)
	return pr
}
func (f *fixture) admit(t *testing.T, pr int64, key string) (string, time.Time) {
	t.Helper()
	var reason string
	var until time.Time
	err := f.store.transaction(t.Context(), func(tx pgx.Tx) error {
		var err error
		reason, until, err = f.store.AdmitPRTx(t.Context(), tx, pr, key, f.now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return reason, until
}
func (f *fixture) reserve(t *testing.T, pr int64) string {
	t.Helper()
	key := uuid.NewString()
	if r, _ := f.admit(t, pr, key); r != "" {
		t.Fatal("unexpected admission wait", r)
	}
	return key
}
func (f *fixture) start(t *testing.T, key string) {
	t.Helper()
	if _, err := f.store.start(t.Context(), key); err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) finish(t *testing.T, key, outcome string) {
	t.Helper()
	if err := f.store.finish(t.Context(), key, outcome); err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) consumed(t *testing.T) int64 {
	t.Helper()
	var us int64
	if err := f.pool.QueryRow(t.Context(), `SELECT coalesce(sum(consumed_us),0)::bigint FROM workspace_usage_periods WHERE workspace_id=$1`, f.workspace).Scan(&us); err != nil {
		t.Fatal(err)
	}
	return us
}
func (f *fixture) member(t *testing.T, role string, grant bool) int64 {
	t.Helper()
	user := f.id(t, `INSERT INTO users(github_id,username) VALUES($1,$2) RETURNING id`, time.Now().UnixNano(), role)
	f.exec(t, `INSERT INTO workspace_memberships(workspace_id,user_id,role) VALUES($1,$2,$3)`, f.workspace, user, role)
	if grant {
		f.exec(t, `INSERT INTO repository_memberships(repo_id,workspace_id,user_id) VALUES($1,$2,$3)`, f.repo, f.workspace, user)
	}
	return user
}

func (f *fixture) assertReconciles(t *testing.T) {
	t.Helper()
	var mismatch int
	err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM workspace_usage_periods p WHERE p.consumed_us<>(SELECT coalesce(sum(s.consumed_us),0)::bigint FROM workspace_usage_segments s WHERE s.workspace_id=p.workspace_id AND s.period_start=p.starts_at)`).Scan(&mismatch)
	if err != nil || mismatch != 0 {
		t.Fatalf("periods must reconcile with authoritative segments: %d %v", mismatch, err)
	}
}

func TestUTCPeriod(t *testing.T) {
	at := time.Date(2026, 12, 31, 23, 59, 59, 0, time.FixedZone("offset", -3600))
	start, end := period(at, "month")
	if start.Format(time.DateOnly) != "2027-01-01" || end.Format(time.DateOnly) != "2027-02-01" {
		t.Fatal(start, end)
	}
	if fmt.Sprint(end.Sub(start)) != "744h0m0s" {
		t.Fatal(start, end)
	}
}
