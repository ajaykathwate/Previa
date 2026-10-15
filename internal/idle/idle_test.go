package idle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

func isolatedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for idle preview database tests")
	}
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("idle_test_%d", time.Now().UnixNano())
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

type fakeContainers struct {
	mu           sync.Mutex
	slept, woken []int64
	wakeErr      error
}

func (c *fakeContainers) SleepPreview(_ context.Context, pr, _, _ int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slept = append(c.slept, pr)
	return nil
}

func (c *fakeContainers) WakePreview(_ context.Context, pr, _, _ int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.woken = append(c.woken, pr)
	return c.wakeErr
}

type fakeRoutes struct{ syncs int }

func (r *fakeRoutes) Sync(context.Context) error { r.syncs++; return nil }

type freshness bool

func (f freshness) Fresh(time.Duration) bool { return bool(f) }

type fixture struct {
	pool       *pgxpool.Pool
	containers *fakeContainers
	routes     *fakeRoutes
	idle       *Idle
	envs       map[string]int64 // by subdomain
	prs        map[string]int64
}

// newFixture has live previews: "idle" (last visited 5 hours ago), "busy"
// (visited a minute ago) and "never" (published 5 hours ago, never visited).
func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{pool: isolatedPool(t), containers: &fakeContainers{}, routes: &fakeRoutes{}, envs: map[string]int64{}, prs: map[string]int64{}}
	ctx := t.Context()
	var owner, install, repo int64
	scan := func(dst *int64, sql string, args ...any) {
		t.Helper()
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	scan(&owner, `INSERT INTO users(github_id,username) VALUES (42,'owner') RETURNING id`)
	scan(&install, `INSERT INTO installations(github_installation_id,owner_user_id) VALUES (99,$1) RETURNING id`, owner)
	scan(&repo, `INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES ($1,100,'owner/app') RETURNING id`, install)
	for i, p := range []struct {
		name            string
		published, seen string
	}{
		{"idle", "6 hours", "5 hours"}, {"busy", "6 hours", "1 minute"}, {"never", "5 hours", ""},
	} {
		var pr, env int64
		scan(&pr, `INSERT INTO pull_requests(repo_id,number,head_sha,generation) VALUES ($1,$2,'sha',1) RETURNING id`, repo, i+1)
		seen := "NULL"
		if p.seen != "" {
			seen = "now() - interval '" + p.seen + "'"
		}
		scan(&env, `INSERT INTO environments(pull_request_id,subdomain,status,container_id,applied_generation,applied_revision,expires_at,last_active_at,last_visited_at)
			VALUES ($1,$2,'running','c-'||$2,1,1,now()+interval '1 day',now() - $3::interval,`+seen+`) RETURNING id`, pr, p.name, p.published)
		f.envs[p.name], f.prs[p.name] = env, pr
	}
	var err error
	f.idle, err = New(f.pool, f.containers, f.routes, freshness(true), 3*time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) asleep(t *testing.T, name string) (bool, string) {
	t.Helper()
	var asleep bool
	var note string
	if err := f.pool.QueryRow(t.Context(), "SELECT asleep_at IS NOT NULL, wake_error FROM environments WHERE id=$1", f.envs[name]).Scan(&asleep, &note); err != nil {
		t.Fatal(err)
	}
	return asleep, note
}

func (f *fixture) version(t *testing.T) int64 {
	t.Helper()
	var v int64
	if err := f.pool.QueryRow(t.Context(), "SELECT version FROM route_table").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Previews nobody visited for the idle time sleep (route first, then their
// containers); a visited one does not; nothing sleeps while visits are not
// being read; a wake starts it again and routes it; a failed wake leaves it
// asleep with a note.
func TestIdlePreviewsSleepAndWake(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.idle.visits = freshness(false)
	if n, err := f.idle.SleepIdle(ctx); err != nil || n != 0 {
		t.Fatalf("slept %d without fresh visit counts: %v", n, err)
	}
	f.idle.visits = freshness(true)
	before := f.version(t)
	n, err := f.idle.SleepIdle(ctx)
	if err != nil || n != 2 {
		t.Fatalf("slept %d: %v", n, err)
	}
	for name, want := range map[string]bool{"idle": true, "never": true, "busy": false} {
		if got, _ := f.asleep(t, name); got != want {
			t.Errorf("%s asleep = %v, want %v", name, got, want)
		}
	}
	if f.version(t) != before+2 || f.routes.syncs != 2 || len(f.containers.slept) != 2 {
		t.Fatalf("version %d→%d, %d syncs, slept %v", before, f.version(t), f.routes.syncs, f.containers.slept)
	}
	// Waking: the containers start, the route serves them again.
	if err := f.idle.Wake(ctx, f.envs["idle"]); err != nil {
		t.Fatal(err)
	}
	if asleep, _ := f.asleep(t, "idle"); asleep || len(f.containers.woken) != 1 || f.routes.syncs != 3 {
		t.Fatalf("after waking: asleep %v, woken %v, %d syncs", asleep, f.containers.woken, f.routes.syncs)
	}
	if n, _ := f.idle.SleepIdle(ctx); n != 0 {
		t.Fatal("a preview woken just now went back to sleep")
	}
	f.containers.wakeErr = &safeError{"The server has no room to wake this preview right now; try again in a few minutes."}
	if err := f.idle.Wake(ctx, f.envs["never"]); err != nil {
		t.Fatal(err)
	}
	if asleep, note := f.asleep(t, "never"); !asleep || !strings.Contains(note, "no room") {
		t.Fatalf("failed wake: asleep %v, note %q", asleep, note)
	}
	// A new build of a sleeping preview wakes it: accepting its route clears
	// the sleep (environments.WakeForNewRoute); a wake job then does nothing.
	if _, err := f.pool.Exec(ctx, "UPDATE environments SET asleep_at=NULL WHERE id=$1", f.envs["never"]); err != nil {
		t.Fatal(err)
	}
	woken := len(f.containers.woken)
	if err := f.idle.Wake(ctx, f.envs["never"]); err != nil || len(f.containers.woken) != woken {
		t.Fatalf("waking an awake preview: %v", err)
	}
}

type safeError struct{ msg string }

func (e *safeError) Error() string       { return e.msg }
func (e *safeError) SafeMessage() string { return e.msg }

type jobs struct{ inserted []int64 }

func (j *jobs) Insert(_ context.Context, args river.JobArgs, _ *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	j.inserted = append(j.inserted, args.(WakeArgs).EnvironmentID)
	return &rivertype.JobInsertResult{}, nil
}

// The wake page wakes a sleeping preview and sends its visitor back to the
// address they opened once it runs; anything else gets the unavailable page.
func TestWakePageWakesAndReturnsTheVisitor(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	queue := &jobs{}
	page := NewWakePage(f.pool, queue, "preview.example.com", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusNotFound)
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	r.Get("/previews/wake/*", page.ServeHTTP)
	r.Post("/previews/wake/*", page.ServeHTTP)
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	visit := func(method, path string) *http.Response {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, method, server.URL+path, nil)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		return res
	}
	for _, path := range []string{"/previews/wake/idle.evil.example/x", "/previews/wake/nope.preview.example.com/", "/previews/wake/a.b.preview.example.com/"} {
		if res := visit(http.MethodGet, path); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", path, res.StatusCode)
		}
	}
	// Awake: straight back to the preview.
	res := visit(http.MethodGet, "/previews/wake/busy.preview.example.com/a/b?c=1")
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "https://busy.preview.example.com/a/b?c=1" {
		t.Fatalf("awake: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	// Asleep: one wake job, and the page reloads until it runs.
	if _, err := f.idle.SleepIdle(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		res = visit(http.MethodGet, "/previews/wake/idle.preview.example.com/path")
		if res.StatusCode != http.StatusAccepted || res.Header.Get("Refresh") != "3" || res.Header.Get("X-Robots-Tag") != "noindex" {
			t.Fatalf("asleep: %d %v", res.StatusCode, res.Header)
		}
	}
	if len(queue.inserted) != 1 || queue.inserted[0] != f.envs["idle"] {
		t.Fatalf("wake jobs: %v", queue.inserted)
	}
	if err := f.idle.Wake(ctx, f.envs["idle"]); err != nil {
		t.Fatal(err)
	}
	if res = visit(http.MethodGet, "/previews/wake/idle.preview.example.com/path"); res.StatusCode != http.StatusAccepted || res.Header.Get("Refresh") != "2" {
		t.Fatalf("just woken waits for the router: %d %v", res.StatusCode, res.Header)
	}
	page.now = func() time.Time { return time.Now().Add(settle) }
	if res = visit(http.MethodGet, "/previews/wake/idle.preview.example.com/path"); res.StatusCode != http.StatusFound || res.Header.Get("Location") != "https://idle.preview.example.com/path" {
		t.Fatalf("woken: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	// A failed wake shows why, and Try again asks once more.
	f.containers.wakeErr = errors.New("docker unavailable")
	if err := f.idle.Wake(ctx, f.envs["never"]); err != nil {
		t.Fatal(err)
	}
	if res = visit(http.MethodGet, "/previews/wake/never.preview.example.com/"); res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("failed wake: %d", res.StatusCode)
	}
	if res = visit(http.MethodPost, "/previews/wake/never.preview.example.com/"); res.StatusCode != http.StatusSeeOther || len(queue.inserted) != 2 {
		t.Fatalf("retry: %d, jobs %v", res.StatusCode, queue.inserted)
	}
}
