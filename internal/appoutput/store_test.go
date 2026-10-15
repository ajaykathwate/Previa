package appoutput

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/secrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

func isolatedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for app output database tests")
	}
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("appoutput_test_%d", time.Now().UnixNano())
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

type fixture struct {
	pool                 *pgxpool.Pool
	store                *Store
	owner, repoID, envID int64
	number               int32
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{pool: isolatedPool(t), number: 7}
	f.store = NewStore(f.pool)
	var install, pr int64
	for _, step := range []struct {
		sql  string
		args []any
		into *int64
	}{
		{"INSERT INTO users(github_id,username) VALUES (42,'owner') RETURNING id", nil, &f.owner},
		{"INSERT INTO installations(github_installation_id,owner_user_id) VALUES (99,$1) RETURNING id", []any{&f.owner}, &install},
		{"INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES ($1,100,'owner/app') RETURNING id", []any{&install}, &f.repoID},
		{"INSERT INTO pull_requests(repo_id,number,head_sha,title) VALUES ($1,7,'sha-1','A change') RETURNING id", []any{&f.repoID}, &pr},
		{"INSERT INTO environments(pull_request_id,subdomain,status,container_id,head_sha,expires_at) VALUES ($1,'preview-7','running','" + strings.Repeat("c", 64) + "','sha-1',now()+interval '1 day') RETURNING id", []any{&pr}, &f.envID},
	} {
		args := make([]any, len(step.args))
		for i, a := range step.args {
			args[i] = *(a.(*int64))
		}
		if err := f.pool.QueryRow(t.Context(), step.sql, args...).Scan(step.into); err != nil {
			t.Fatalf("%s: %v", step.sql, err)
		}
	}
	return f
}

func (f *fixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

// A viewer asks; one follower takes the watch; lines keep the newest
// keepLines; a new follower fences the old one; stopping can forget.
func TestStoreWatchLifecycle(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	if p, err := f.store.Preview(ctx, f.owner, f.repoID, 7); err != nil || !p.Live || p.EnvironmentID != f.envID {
		t.Fatalf("preview: %+v %v", p, err)
	}
	if _, err := f.store.Preview(ctx, f.owner+1, f.repoID, 7); err != ErrUnavailable {
		t.Fatalf("another user's preview: %v", err)
	}
	if needed, err := f.store.Request(ctx, f.envID); err != nil || !needed {
		t.Fatalf("first request: %v %v", needed, err)
	}
	epoch, began, err := f.store.Begin(ctx, f.envID)
	if err != nil || !began || epoch != 1 {
		t.Fatalf("begin: %d %v %v", epoch, began, err)
	}
	if _, began, _ := f.store.Begin(ctx, f.envID); began {
		t.Fatal("a second follower took a watch already followed")
	}
	if needed, err := f.store.Request(ctx, f.envID); err != nil || needed {
		t.Fatalf("a live follower needs no other: %v %v", needed, err)
	}
	lines := make([]Line, keepLines+5)
	for i := range lines {
		lines[i] = Line{At: time.Now(), Text: fmt.Sprintf("line %d", i)}
	}
	if err := f.store.Append(ctx, f.envID, epoch, lines); err != nil {
		t.Fatal(err)
	}
	w, err := f.store.Watch(ctx, f.envID)
	if err != nil || w.State != StateFollowing || w.NextSequence != keepLines+5 || w.First != 5 {
		t.Fatalf("watch: %+v %v", w, err)
	}
	got, err := f.store.Lines(ctx, f.envID, epoch, 0, 2)
	if err != nil || len(got) != 2 || got[0].Text != "line 5" {
		t.Fatalf("oldest kept lines: %+v %v", got, err)
	}
	if watched, err := f.store.Heartbeat(ctx, f.envID, epoch); err != nil || !watched {
		t.Fatalf("heartbeat: %v %v", watched, err)
	}
	f.exec(t, "UPDATE app_output_watches SET seen_at = now() - interval '2 minutes'")
	if watched, err := f.store.Heartbeat(ctx, f.envID, epoch); err != nil || watched {
		t.Fatalf("an unwatched follower must stop: %v %v", watched, err)
	}
	// The follower's worker stopped answering: a viewer's touch replaces it.
	f.exec(t, "UPDATE app_output_watches SET updated_at = now() - interval '1 minute'")
	if needed, err := f.store.Touch(ctx, f.envID); err != nil || !needed {
		t.Fatalf("a stale follower is replaced: %v %v", needed, err)
	}
	next, began, err := f.store.Begin(ctx, f.envID)
	if err != nil || !began || next != 2 {
		t.Fatalf("second follower: %d %v %v", next, began, err)
	}
	if err := f.store.Append(ctx, f.envID, epoch, lines[:1]); err != errFenced {
		t.Fatalf("the replaced follower is fenced: %v", err)
	}
	if err := f.store.Stop(ctx, f.envID, next, "", true); err != nil {
		t.Fatal(err)
	}
	var kept int
	if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM app_output_lines").Scan(&kept); err != nil || kept != 0 {
		t.Fatalf("a forgotten watch keeps no lines: %d %v", kept, err)
	}
	if needed, err := f.store.Touch(ctx, f.envID); err != nil || needed {
		t.Fatalf("a stopped watch restarts only when a viewer opens it again: %v %v", needed, err)
	}
}

type fakeSource struct {
	output   string
	redact   []string
	follows  chan string
	keepOpen bool
}

func (s *fakeSource) FollowApp(_ context.Context, _ int64, containerID, _ string) (io.ReadCloser, *secrets.Redactor, error) {
	s.follows <- containerID
	r, err := secrets.NewRedactor(s.redact...)
	if err != nil {
		return nil, nil, err
	}
	reader, writer := io.Pipe()
	go func() {
		_, _ = io.WriteString(writer, s.output)
		if !s.keepOpen {
			_ = writer.Close()
		}
	}()
	return reader, r, nil
}

// The follower stores sanitized, redacted lines with Docker's times, says
// when the output ends, and keeps nothing once nobody watches.
func TestFollowerStoresRedactedLinesWhileWatched(t *testing.T) {
	flushEvery, heartbeatEvery = 20*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { flushEvery, heartbeatEvery = 500*time.Millisecond, 10*time.Second })
	f := newFixture(t)
	ctx := t.Context()
	source := &fakeSource{
		output:  "2026-10-12T09:30:01.5Z listening on :3000\n2026-10-12T09:30:02Z \x1b[31mtoken=super-secret-1\x1b[0m\nno timestamp\n",
		redact:  []string{"super-secret-1"},
		follows: make(chan string, 2),
	}
	worker := &FollowWorker{Store: f.store, Source: source, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if _, err := f.store.Request(ctx, f.envID); err != nil {
		t.Fatal(err)
	}
	if err := worker.Work(ctx, &river.Job[FollowArgs]{Args: FollowArgs{EnvironmentID: f.envID}}); err != nil {
		t.Fatal(err)
	}
	if id := <-source.follows; id != strings.Repeat("c", 64) {
		t.Fatalf("followed %q", id)
	}
	w, err := f.store.Watch(ctx, f.envID)
	if err != nil || w.State != StateStopped || w.Note != noteEnded {
		t.Fatalf("after the output ended: %+v %v", w, err)
	}
	lines, err := f.store.Lines(ctx, f.envID, w.Epoch, 0, 10)
	if err != nil || len(lines) != 3 {
		t.Fatalf("lines: %+v %v", lines, err)
	}
	if lines[0].Text != "listening on :3000" || !lines[0].At.Equal(time.Date(2026, 10, 12, 9, 30, 1, 5e8, time.UTC)) ||
		lines[1].Text != "token=[redacted]" || lines[2].Text != "no timestamp" {
		t.Fatalf("stored: %+v", lines)
	}

	// Nobody watching: the follower stops and keeps nothing.
	source.keepOpen = true
	if _, err := f.store.Request(ctx, f.envID); err != nil {
		t.Fatal(err)
	}
	f.exec(t, "UPDATE app_output_watches SET seen_at = now() - interval '2 minutes'")
	if err := worker.Work(ctx, &river.Job[FollowArgs]{Args: FollowArgs{EnvironmentID: f.envID}}); err != nil {
		t.Fatal(err)
	}
	var kept int
	if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM app_output_lines").Scan(&kept); err != nil || kept != 0 {
		t.Fatalf("an unwatched follower keeps no lines: %d %v", kept, err)
	}

	// A preview that is not running has nothing to follow.
	f.exec(t, "UPDATE environments SET status='torn_down', container_id=NULL")
	if _, err := f.store.Request(ctx, f.envID); err != nil {
		t.Fatal(err)
	}
	if err := worker.Work(ctx, &river.Job[FollowArgs]{Args: FollowArgs{EnvironmentID: f.envID}}); err != nil {
		t.Fatal(err)
	}
	if w, _ := f.store.Watch(ctx, f.envID); w.Note != noteNotRunning {
		t.Fatalf("not running: %+v", w)
	}
}
