package accounts

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/auth"
	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/installations"
	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/ajaykathwatee/previa/internal/support"
	"github.com/ajaykathwatee/previa/internal/workspaces"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

type fixture struct {
	pool                                         *pgxpool.Pool
	s                                            *Service
	owner, member, workspace, installation, repo int64
	inventory                                    *inventory
}
type inventory struct {
	resources []environments.Resource
	fail      error
}

func (i *inventory) Resources(context.Context, int64) ([]environments.Resource, error) {
	return i.resources, i.fail
}
func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for lifecycle integration tests")
	}
	admin, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("accounts_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(t.Context(), "CREATE SCHEMA "+quoted); err != nil {
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
		t.Fatal(err)
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
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = migrator.Migrate(t.Context(), rivermigrate.DirectionUp, nil); err != nil {
		t.Fatal(err)
	}
	f := &fixture{pool: pool, inventory: &inventory{}}
	f.owner = f.id(t, `INSERT INTO users(github_id,username) VALUES(101,'owner') RETURNING id`)
	f.member = f.id(t, `INSERT INTO users(github_id,username,email,first_name) VALUES(102,'member','private@example.test','Personal') RETURNING id`)
	f.installation = f.id(t, `INSERT INTO installations(github_installation_id,owner_user_id,account_login) VALUES(201,$1,'acme') RETURNING id`, f.owner)
	f.workspace = f.id(t, `SELECT workspace_id FROM installations WHERE id=$1`, f.installation)
	f.repo = f.id(t, `INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES($1,301,'acme/api') RETURNING id`, f.installation)
	f.exec(t, `INSERT INTO workspace_memberships(workspace_id,user_id,role) VALUES($1,$2,'admin')`, f.workspace, f.member)
	producer, err := queue.NewProducer(pool, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	ops, err := environments.NewOperationBroker(producer)
	if err != nil {
		t.Fatal(err)
	}
	ws := workspaces.NewService(workspaces.NewRepository(pool), nil, nil, nil)
	help := support.NewService(pool, support.Settings{})
	f.s = NewService(pool).WithPreviews(environments.NewLifecycle(environments.NewRepository(pool, ops), environments.NewRequestBroker(pool, producer), f.inventory)).WithRepositoryErasers(installations.NewRepository(pool)).WithInstallations(installations.NewRepository(pool)).WithWorkspaces(ws, help).WithAccountFences(ws).WithAccountErasers(help, ws, auth.NewRepository(pool))
	return f
}
func (f *fixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}
func (f *fixture) id(t *testing.T, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := f.pool.QueryRow(t.Context(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}
func (f *fixture) text(t *testing.T, q string, args ...any) string {
	t.Helper()
	var s string
	if err := f.pool.QueryRow(t.Context(), q, args...).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}
func (f *fixture) journal(t *testing.T) Journal {
	t.Helper()
	raw, err := f.s.ExportJournal(t.Context(), 90, 1048576)
	if err != nil {
		t.Fatal(err)
	}
	j, err := DecodeJournal(raw, 1048576)
	if err != nil {
		t.Fatal(err)
	}
	return j
}
