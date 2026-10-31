package sampledata

import (
	"context"
	"errors"
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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// isolatedPool migrates a fresh schema of TEST_DATABASE_URL.
func isolatedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for test data database tests")
	}
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("sampledata_test_%d", time.Now().UnixNano())
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
	cfg.MaxConns = 6
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

// recordedJobs records refresh job inserts. A job inserted while another of
// the same repository is unfinished is a duplicate, as River's unique
// options make it; scheduled says until when the existing one waits.
type recordedJobs struct {
	mu        sync.Mutex
	inserted  []RefreshArgs
	scheduled map[int64]time.Time // repository → when its unfinished job runs
	retried   []int64
}

func newRecordedJobs() *recordedJobs { return &recordedJobs{scheduled: map[int64]time.Time{}} }

func (j *recordedJobs) InsertTx(_ context.Context, _ pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	a, ok := args.(RefreshArgs)
	if !ok {
		return nil, errors.New("unexpected job")
	}
	if at, exists := j.scheduled[a.RepoID]; exists {
		return &rivertype.JobInsertResult{UniqueSkippedAsDuplicate: true, Job: &rivertype.JobRow{ID: a.RepoID, State: rivertype.JobStateScheduled, ScheduledAt: at}}, nil
	}
	j.inserted = append(j.inserted, a)
	j.scheduled[a.RepoID] = opts.ScheduledAt
	return &rivertype.JobInsertResult{Job: &rivertype.JobRow{ID: a.RepoID, State: rivertype.JobStateScheduled, ScheduledAt: opts.ScheduledAt}}, nil
}

func (j *recordedJobs) JobRetryTx(_ context.Context, _ pgx.Tx, id int64) (*rivertype.JobRow, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.retried = append(j.retried, id)
	j.scheduled[id] = time.Now()
	return &rivertype.JobRow{ID: id, State: rivertype.JobStateAvailable}, nil
}

// finish forgets a repository's job, as if it ran to the end.
func (j *recordedJobs) finish(repoID int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.scheduled, repoID)
}

func (j *recordedJobs) count() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.inserted)
}

// fakeBuilder returns what the test set, and records the jobs it got.
type fakeBuilder struct {
	mu     sync.Mutex
	jobs   []BuildJob
	result BuildResult
	err    error
	during func(BuildJob) // runs while "building"
}

func (b *fakeBuilder) BuildBaseline(ctx context.Context, job BuildJob) (BuildResult, error) {
	b.mu.Lock()
	b.jobs = append(b.jobs, job)
	during, result, err := b.during, b.result, b.err
	b.mu.Unlock()
	if during != nil {
		during(job)
	}
	if ctx.Err() != nil {
		return BuildResult{}, ctx.Err()
	}
	return result, err
}

func (b *fakeBuilder) built() []BuildJob {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]BuildJob(nil), b.jobs...)
}

// fakeBranches resolves branches from a map; "" is the default branch.
type fakeBranches struct {
	mu            sync.Mutex
	defaultBranch string
	heads         map[string]string
	err           error
	calls         int
}

func (b *fakeBranches) BranchHead(_ context.Context, _, _ int64, branch string) (string, string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	if b.err != nil {
		return "", "", b.err
	}
	if branch == "" {
		branch = b.defaultBranch
	}
	sha, ok := b.heads[branch]
	if !ok {
		return branch, "", ErrBranchNotFound
	}
	return branch, sha, nil
}

func (b *fakeBranches) set(branch, sha string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.heads[branch] = sha
}

// fakeAdmission admits everything unless told to wait, and records usage.
type fakeAdmission struct {
	mu     sync.Mutex
	reason string
	until  time.Time
	usage  []int64 // seconds per recorded build
}

func (a *fakeAdmission) AdmitBaselineTx(context.Context, pgx.Tx, int64, int64, time.Time) (string, time.Time, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reason, a.until, nil
}

func (a *fakeAdmission) RecordBaselineUsage(_ context.Context, _ int64, _ time.Time, seconds int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.usage = append(a.usage, seconds)
	return nil
}

// recordedActivity keeps events in memory.
type recordedActivity struct {
	mu     sync.Mutex
	events []Event
}

func (a *recordedActivity) RecordTx(_ context.Context, _ pgx.Tx, e Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, e)
	return nil
}

func (a *recordedActivity) kinds() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var kinds []string
	for _, e := range a.events {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

// removedFiles records removed storage keys.
type removedFiles struct {
	mu   sync.Mutex
	keys []string
}

func (f *removedFiles) Remove(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, key)
	return nil
}

func (f *removedFiles) removed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
}

// fixedCatalog offers what the test says.
type fixedCatalog struct{ offer Offer }

func (c fixedCatalog) TestData(context.Context) (Offer, error) { return c.offer, nil }

const (
	shaOne   = "1111111111111111111111111111111111111111"
	shaTwo   = "2222222222222222222222222222222222222222"
	shaThree = "3333333333333333333333333333333333333333"
)

// fixture is a workspace with an Owner, an Admin, a Developer granted
// acme/api and a Reviewer, the repository acme/api (GitHub 100) on
// installation 7, and the feature's parts wired to fakes.
type fixture struct {
	pool                        *pgxpool.Pool
	jobs                        *recordedJobs
	store                       *Store
	service                     *Service
	runner                      *Runner
	builder                     *fakeBuilder
	branches                    *fakeBranches
	admission                   *fakeAdmission
	activity                    *recordedActivity
	files                       *removedFiles
	owner, admin, dev, reviewer int64
	workspace, repo             int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := isolatedPool(t)
	ctx := t.Context()
	f := &fixture{pool: pool, jobs: newRecordedJobs(), builder: &fakeBuilder{}, branches: &fakeBranches{defaultBranch: "main", heads: map[string]string{"main": shaOne}},
		admission: &fakeAdmission{}, activity: &recordedActivity{}, files: &removedFiles{}}
	scan := func(dst *int64, query string, args ...any) {
		t.Helper()
		if err := pool.QueryRow(ctx, query, args...).Scan(dst); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	for login, dst := range map[string]*int64{"owner": &f.owner, "admin": &f.admin, "dev": &f.dev, "reviewer": &f.reviewer} {
		scan(dst, "INSERT INTO users(github_id,username) VALUES($1,$2) RETURNING id", int64(len(login))*1000+int64(login[0]), login)
	}
	var installation int64
	scan(&installation, "INSERT INTO installations(github_installation_id,owner_user_id,account_login) VALUES(7,$1,'acme') RETURNING id", f.owner)
	scan(&f.workspace, "SELECT workspace_id FROM installations WHERE id=$1", installation)
	f.exec(t, "INSERT INTO workspace_memberships(workspace_id,user_id,role) VALUES($1,$2,'admin'),($1,$3,'developer'),($1,$4,'reviewer')", f.workspace, f.admin, f.dev, f.reviewer)
	scan(&f.repo, "INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES($1,100,'acme/api') RETURNING id", installation)
	f.exec(t, "INSERT INTO repository_memberships(repo_id,workspace_id,user_id) VALUES($1,$2,$3),($1,$2,$4)", f.repo, f.workspace, f.dev, f.reviewer)
	var err error
	if f.store, err = NewStore(pool, f.jobs, "build"); err != nil {
		t.Fatal(err)
	}
	if f.service, err = NewService(f.store, f.activity, fixedCatalog{offer: Offer{Databases: true, Baselines: true}}); err != nil {
		t.Fatal(err)
	}
	if f.runner, err = NewRunner(f.store, RunnerParts{Builder: f.builder, Branches: f.branches, Admission: f.admission, Activity: f.activity,
		Files: f.files, Timeout: time.Minute, MinInterval: 10 * time.Minute}, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// enable turns Test data on as the Owner.
func (f *fixture) enable(t *testing.T) Settings {
	t.Helper()
	settings, err := f.service.Save(t.Context(), f.owner, f.repo, Form{Enabled: true, KeepData: true, UseBaseline: true, AutoRefresh: true})
	if err != nil {
		t.Fatal(err)
	}
	return settings
}

// dumps is one captured PostgreSQL 17 dump.
func dumps(key string) BuildResult {
	return BuildResult{ConfigHash: "hash", Dumps: []Dump{{Kind: "postgres", Version: "17",
		Stored: Stored{Key: key, Checksum: strings.Repeat("a", 64), Size: 4096, KeyVersion: 1}}}}
}

// refreshes are the repository's refreshes, oldest first.
func (f *fixture) refreshes(t *testing.T) []Refresh {
	t.Helper()
	rows, err := f.pool.Query(t.Context(), "SELECT sequence, trigger, state, coalesce(failure_code,''), coalesce(waiting_reason,''), attempts, available_at FROM baseline_refreshes WHERE repo_id=$1 ORDER BY sequence", f.repo)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []Refresh
	for rows.Next() {
		var r Refresh
		if err := rows.Scan(&r.Sequence, &r.Trigger, &r.State, &r.FailureCode, &r.WaitingReason, &r.Attempts, &r.AvailableAt); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// current is the repository's current baselines.
func (f *fixture) current(t *testing.T) []Baseline {
	t.Helper()
	baselines, err := f.store.Current(t.Context(), f.repo)
	if err != nil {
		t.Fatal(err)
	}
	return baselines
}

// fakeLeftovers reports Docker objects by refresh and records removals.
type fakeLeftovers struct {
	found   map[int64]time.Time
	removed []int64
}

func (l *fakeLeftovers) BaselineRefreshes(context.Context) (map[int64]time.Time, error) {
	return l.found, nil
}

func (l *fakeLeftovers) RemoveBaseline(_ context.Context, id int64) error {
	l.removed = append(l.removed, id)
	return nil
}

func (l *fakeLeftovers) RemoveBaselineImages(context.Context, int64) error { return nil }

type emptyDirectory struct{}

func (emptyDirectory) Files() ([]StoredFile, error) { return nil, nil }
func (emptyDirectory) Remove(string) error          { return nil }
func (emptyDirectory) RemoveTemporary(string) error { return nil }

func osChtimes(path string, t time.Time) error { return os.Chtimes(path, t, t) }
