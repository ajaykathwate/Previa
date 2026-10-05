package housekeeping

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/ajaykathwatee/previa/internal/webhooks"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// fixture is a migrated throwaway schema with the environments stack
// (repository, broker, teardown sweeper) over a fake runtime.
type fixture struct {
	pool      *pgxpool.Pool
	logger    *slog.Logger
	repo      *environments.Repository
	broker    *environments.Broker
	events    *webhooks.Service
	sweeper   *environments.TeardownSweeper
	inventory *fakeLeftovers
	repoID    int64
	settings  Settings
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := migratedPool(t)
	f := &fixture{pool: pool, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), inventory: newFakeLeftovers()}
	ctx := t.Context()
	var user, installation int64
	f.scan(t, "INSERT INTO users(github_id, username) VALUES (1, 'owner') RETURNING id", []any{&user})
	f.scan(t, "INSERT INTO installations(github_installation_id, owner_user_id, status) VALUES (900, $1, 'active') RETURNING id", []any{&installation}, user)
	f.scan(t, "INSERT INTO repos(installation_id, full_name, github_repo_id, build_mode) VALUES ($1, 'octo/app', 100, 'automatic') RETURNING id", []any{&f.repoID}, installation)
	producer, err := queue.NewProducer(pool, f.logger)
	if err != nil {
		t.Fatal(err)
	}
	operations, err := environments.NewOperationBroker(producer)
	if err != nil {
		t.Fatal(err)
	}
	f.repo = environments.NewRepository(pool, operations).WithReports()
	service, err := environments.NewService(f.repo, idleRuntime{}, f.logger, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := environments.NewOperationWorker(f.repo, noopExecutor{}, f.logger, time.Minute, map[environments.OperationKind]time.Duration{
		environments.ActivateRoute: time.Second, environments.RemoveRoute: time.Second, environments.ReportStatus: time.Second, environments.CleanupArtifact: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.broker, err = environments.NewExecutionBroker(pool, producer, service, worker, nil); err != nil {
		t.Fatal(err)
	}
	f.events = webhooks.NewService(pool, f.broker)
	if f.sweeper, err = environments.NewTeardownSweeper(f.repo, f.inventory, f.logger, 10); err != nil {
		t.Fatal(err)
	}
	f.settings = Settings{Grace: 15 * time.Minute, CacheBudget: 10 << 30, WorkspaceMaxAge: time.Hour, BatchSize: 10, PullRequestBatch: 2}
	_ = ctx
	return f
}

func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	connection := os.Getenv("TEST_DATABASE_URL")
	if connection == "" {
		t.Skip("set TEST_DATABASE_URL for housekeeping integration tests")
	}
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, connection)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("housekeeping_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
	})
	cfg, err := pgxpool.ParseConfig(connection)
	if err != nil {
		t.Fatal("invalid test connection")
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
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(data), "-- +goose Down")
		if _, err := pool.Exec(ctx, up); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
	}
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		t.Fatal(err)
	}
	return pool
}

func (f *fixture) scan(t *testing.T, sql string, dest []any, args ...any) {
	t.Helper()
	if err := f.pool.QueryRow(t.Context(), sql, args...).Scan(dest...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func (f *fixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func (f *fixture) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	f.scan(t, sql, []any{&n}, args...)
	return n
}

// open records PR number opened at sha through the webhook path (Automatic:
// a pending build request).
func (f *fixture) open(t *testing.T, number int32, sha string) environments.Request {
	t.Helper()
	r, err := f.events.ProcessPR(t.Context(), webhooks.PREvent{RepoID: f.repoID, Number: number, HeadSHA: sha, Action: "opened", MergeableState: "clean"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// publish makes the PR look published: its request succeeded and its
// environment runs a preview of the current commit.
func (f *fixture) publish(t *testing.T, r environments.Request, expiresIn time.Duration) {
	t.Helper()
	f.exec(t, "UPDATE build_intents SET state='succeeded', started_at=now(), completed_at=now() WHERE pull_request_id=$1", r.PullRequestID)
	f.exec(t, `INSERT INTO environments(pull_request_id, subdomain, status, container_id, image_tag, head_sha, applied_generation, applied_revision, expires_at, created_at)
		SELECT id, 'preview-'||id::text, 'running', 'container-'||id::text, 'image-'||id::text, head_sha, generation, 1, now() + $2::bigint * interval '1 second', now() - interval '3 days'
		FROM pull_requests WHERE id = $1::bigint`, r.PullRequestID, int64(expiresIn/time.Second))
}

type idleRuntime struct{}

func (idleRuntime) Build(context.Context, environments.Request, environments.Attempt) (environments.Artifact, error) {
	return environments.Artifact{}, fmt.Errorf("no builds in housekeeping tests")
}
func (idleRuntime) Resources(context.Context, int64) ([]environments.Resource, error) {
	return nil, nil
}

type noopExecutor struct{}

func (noopExecutor) Execute(context.Context, environments.Operation) error { return nil }

// fakeLeftovers is Docker as housekeeping sees it.
type fakeLeftovers struct {
	mu        sync.Mutex
	resources map[int64][]environments.Resource
	removed   []environments.Resource
}

func newFakeLeftovers() *fakeLeftovers {
	return &fakeLeftovers{resources: map[int64][]environments.Resource{}}
}

func (l *fakeLeftovers) add(r environments.Resource) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.resources[r.PullRequestID] = append(l.resources[r.PullRequestID], r)
}

func (l *fakeLeftovers) Resources(_ context.Context, pr int64) ([]environments.Resource, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]environments.Resource(nil), l.resources[pr]...), nil
}

func (l *fakeLeftovers) PullRequests(context.Context) (map[int64]time.Time, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	found := map[int64]time.Time{}
	for pr, list := range l.resources {
		for _, r := range list {
			if r.CreatedAt.After(found[pr]) {
				found[pr] = r.CreatedAt
			}
		}
	}
	return found, nil
}

func (l *fakeLeftovers) RemoveResource(_ context.Context, r environments.Resource) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.removed = append(l.removed, r)
	list := l.resources[r.PullRequestID]
	for i := range list {
		if list[i].Artifact == r.Artifact && list[i].Network == r.Network {
			l.resources[r.PullRequestID] = append(list[:i], list[i+1:]...)
			break
		}
	}
	return nil
}
