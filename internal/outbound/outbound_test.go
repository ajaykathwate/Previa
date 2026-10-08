package outbound

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/egress"
	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

func TestParseAcceptsOnlyPublicNamesOnAllowedPorts(t *testing.T) {
	s, err := NewService(&memoryStore{}, []int{5432, 6379})
	if err != nil {
		t.Fatal(err)
	}
	if d, err := s.Parse(" DB.Example.com. ", "5432"); err != nil || d != (egress.Destination{Host: "db.example.com", Port: 5432}) {
		t.Fatalf("%v %v", d, err)
	}
	for _, tc := range []struct{ host, port, field string }{
		{"10.0.0.5", "5432", "host"},
		{"[::1]", "5432", "host"},
		{"localhost", "5432", "host"},
		{"db.internal", "5432", "host"},
		{"printer.local", "5432", "host"},
		{"db_example.com", "5432", "host"},
		{"db.example.com", "22", "port"},
		{"db.example.com", "abc", "port"},
	} {
		_, err := s.Parse(tc.host, tc.port)
		var invalid *InvalidError
		if !errors.As(err, &invalid) || invalid.Field != tc.field {
			t.Errorf("%s:%s: %v", tc.host, tc.port, err)
		}
	}
	off, _ := NewService(&memoryStore{}, nil)
	if _, err := off.Add(context.Background(), 1, 1, "db.example.com", "5432"); !errors.Is(err, ErrDisabled) || off.Enabled() {
		t.Fatalf("turned off: %v", err)
	}
}

// memoryStore satisfies Store for tests that never reach it.
type memoryStore struct{}

func (*memoryStore) Repository(context.Context, int64, int64) (RepositoryView, error) {
	return RepositoryView{}, ErrUnavailable
}
func (*memoryStore) List(context.Context, int64, int64) ([]Approved, error) { return nil, nil }
func (*memoryStore) Add(context.Context, int64, int64, egress.Destination, int) error {
	return nil
}
func (*memoryStore) Remove(context.Context, int64, int64, int64) (egress.Destination, error) {
	return egress.Destination{}, ErrUnavailable
}

func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	connection := os.Getenv("TEST_DATABASE_URL")
	if connection == "" {
		t.Skip("set TEST_DATABASE_URL for outbound connection checks")
	}
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, connection)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("outbound_test_%d", time.Now().UnixNano())
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

// fixture: an owner with an available repository (and a revoked one), and a
// stranger.
type fixture struct {
	pool                     *pgxpool.Pool
	service                  *Service
	owner, stranger          int64
	repo, revoked, otherRepo int64
}

func newFixture(t *testing.T) *fixture {
	pool := migratedPool(t)
	producer, err := queue.NewProducer(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{pool: pool}
	scan := func(sql string, dest *int64, args ...any) {
		t.Helper()
		if err := pool.QueryRow(t.Context(), sql, args...).Scan(dest); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	scan("INSERT INTO users(github_id, username) VALUES (1, 'owner') RETURNING id", &f.owner)
	scan("INSERT INTO users(github_id, username) VALUES (2, 'stranger') RETURNING id", &f.stranger)
	var mine, theirs int64
	scan("INSERT INTO installations(github_installation_id, owner_user_id, status) VALUES (1, $1, 'active') RETURNING id", &mine, f.owner)
	scan("INSERT INTO installations(github_installation_id, owner_user_id, status) VALUES (2, $1, 'active') RETURNING id", &theirs, f.stranger)
	scan("INSERT INTO repos(installation_id, full_name, github_repo_id) VALUES ($1, 'octo/api', 10) RETURNING id", &f.repo, mine)
	scan("INSERT INTO repos(installation_id, full_name, github_repo_id, accessible) VALUES ($1, 'octo/gone', 11, false) RETURNING id", &f.revoked, mine)
	scan("INSERT INTO repos(installation_id, full_name, github_repo_id) VALUES ($1, 'other/app', 12) RETURNING id", &f.otherRepo, theirs)
	if f.service, err = NewService(NewRepository(pool, producer), []int{5432, 6379, 6380}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) wakeups(t *testing.T) int {
	var n int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM river_job WHERE kind = 'outbound_refresh'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestApprovalsAreOwnerScopedBoundedAndWakeTheWorker(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	if _, err := f.service.Add(ctx, f.owner, f.repo, "db.example.com", "5432"); err != nil {
		t.Fatal(err)
	}
	if f.wakeups(t) != 1 {
		t.Fatal("an approval did not wake the outbound refresh")
	}
	// One destination per port; another user's repository is invisible.
	if _, err := f.service.Add(ctx, f.owner, f.repo, "other.example.com", "5432"); !errors.Is(err, ErrPortTaken) {
		t.Fatalf("second destination on a port: %v", err)
	}
	if _, err := f.service.Add(ctx, f.stranger, f.repo, "db.example.com", "6379"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a stranger approved: %v", err)
	}
	if list, err := f.service.List(ctx, f.stranger, f.repo); err != nil || len(list) != 0 {
		t.Fatalf("a stranger listed approvals: %v %v", list, err)
	}
	if _, err := f.service.Add(ctx, f.owner, f.revoked, "db.example.com", "5432"); !errors.Is(err, ErrAccessRemoved) {
		t.Fatalf("approval after access was removed: %v", err)
	}
	// The limit holds.
	if _, err := f.pool.Exec(ctx, "INSERT INTO repository_destinations(repo_id, host, port) SELECT $1, 'h'||n||'.example.com', 20000+n FROM generate_series(1, $2::int) n", f.repo, MaxDestinations-1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Add(ctx, f.owner, f.repo, "cache.example.com", "6379"); !errors.Is(err, ErrLimit) {
		t.Fatalf("over the limit: %v", err)
	}
	if _, err := f.pool.Exec(ctx, "DELETE FROM repository_destinations WHERE port > 20000"); err != nil {
		t.Fatal(err)
	}
	// The worker reads every repository's approvals without an owner.
	if _, err := f.service.Add(ctx, f.owner, f.repo, "cache.example.com", "6380"); err != nil {
		t.Fatal(err)
	}
	approved, err := NewRepository(f.pool, nil).Destinations(ctx, f.repo, f.otherRepo)
	if err != nil || len(approved[f.repo]) != 2 || approved[f.repo][0] != (egress.Destination{Host: "db.example.com", Port: 5432}) || len(approved[f.otherRepo]) != 0 {
		t.Fatalf("worker view %v %v", approved, err)
	}
	// Removal: only by the owner, works after access is lost, wakes the worker.
	list, _ := f.service.List(ctx, f.owner, f.repo)
	if _, err := f.service.Remove(ctx, f.stranger, f.repo, list[0].ID); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a stranger removed: %v", err)
	}
	if _, err := f.pool.Exec(ctx, "UPDATE repos SET accessible = false WHERE id = $1", f.repo); err != nil {
		t.Fatal(err)
	}
	before := f.wakeups(t)
	if removed, err := f.service.Remove(ctx, f.owner, f.repo, list[0].ID); err != nil || removed.Host != "db.example.com" || f.wakeups(t) != before+1 {
		t.Fatalf("removal %v %v", removed, err)
	}
	// The database refuses what the service would.
	if _, err := f.pool.Exec(ctx, "INSERT INTO repository_destinations(repo_id, host, port) VALUES ($1, '10.0.0.1', 5432)", f.repo); err == nil {
		t.Fatal("the database accepted an IP address")
	}
}

type session struct{ user int64 }

func (s session) UserIDs(context.Context) (int64, int64, bool) { return s.user, s.user, s.user > 0 }
func (s session) Username(context.Context) (string, bool)      { return "owner", true }
func (s session) CSRFToken(context.Context) string             { return "token" }

func TestOutboundTabIsOwnerOnly(t *testing.T) {
	f := newFixture(t)
	serve := func(user int64, method, path string, form url.Values) *httptest.ResponseRecorder {
		r := chi.NewRouter()
		NewHandler(f.service, session{user}, slog.New(slog.NewTextHandler(io.Discard, nil))).Mount(r, func(next http.Handler) http.Handler { return next })
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req := httptest.NewRequest(method, path, body)
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	tab := fmt.Sprintf("/repositories/%d/outbound", f.repo)
	if rec := serve(f.owner, http.MethodGet, tab, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Approve a destination") {
		t.Fatalf("owner page %d", rec.Code)
	}
	if rec := serve(f.stranger, http.MethodGet, tab, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("stranger page %d", rec.Code)
	}
	rec := serve(f.owner, http.MethodPost, tab, url.Values{"host": {"db.example.com"}, "port": {"5432"}})
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "approved=db.example.com") {
		t.Fatalf("approve %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if rec := serve(f.owner, http.MethodPost, tab, url.Values{"host": {"10.0.0.1"}, "port": {"5432"}}); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "not an IP address") {
		t.Fatalf("rejected input %d", rec.Code)
	}
	if rec := serve(f.stranger, http.MethodPost, tab, url.Values{"host": {"x.example.com"}, "port": {"6379"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("stranger approve %d", rec.Code)
	}
	list, _ := f.service.List(t.Context(), f.owner, f.repo)
	remove := fmt.Sprintf("%s/%d/remove", tab, list[0].ID)
	if rec := serve(f.stranger, http.MethodPost, remove, url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("stranger remove %d", rec.Code)
	}
	if after, _ := f.service.List(t.Context(), f.owner, f.repo); len(after) != 1 {
		t.Fatal("a stranger's removal took effect")
	}
	if rec := serve(f.owner, http.MethodPost, remove, url.Values{}); rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "removed=db.example.com") {
		t.Fatalf("owner remove %d", rec.Code)
	}
}
