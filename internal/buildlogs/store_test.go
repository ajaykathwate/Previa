package buildlogs_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/buildlogs"
	"github.com/ajaykathwatee/previa/internal/secrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fixture struct {
	pool          *pgxpool.Pool
	store         *buildlogs.Store
	dsn, schema   string
	owner, other  int64
	repoID, prID  int64
	environmentID int64
}

func setup(t *testing.T) *fixture {
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
	schema := fmt.Sprintf("buildlogs_test_%d", time.Now().UnixNano())
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
	f := &fixture{pool: pool, store: buildlogs.NewStore(pool), dsn: dsn, schema: schema}
	scan := func(dst *int64, sql string, args ...any) {
		t.Helper()
		if err := pool.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	var install int64
	scan(&f.owner, `INSERT INTO users(github_id,username) VALUES (42,'owner') RETURNING id`)
	scan(&f.other, `INSERT INTO users(github_id,username) VALUES (43,'other') RETURNING id`)
	scan(&install, `INSERT INTO installations(github_installation_id,owner_user_id) VALUES (99,$1) RETURNING id`, f.owner)
	scan(&f.repoID, `INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES ($1,100,'owner/app') RETURNING id`, install)
	scan(&f.prID, `INSERT INTO pull_requests(repo_id,number,head_sha,title) VALUES ($1,7,'sha-1','A change') RETURNING id`, f.repoID)
	scan(&f.environmentID, `INSERT INTO environments(pull_request_id,subdomain,expires_at) VALUES ($1,'preview-7',now()+interval '1 day') RETURNING id`, f.prID)
	return f
}

// admit mirrors environments' admission transaction: open the attempt's stream
// (bumping epoch under the row lock), then delete earlier lines.
func (f *fixture) admit(t *testing.T, generation, revision int64) int64 {
	t.Helper()
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var epoch int64
	if err := tx.QueryRow(t.Context(), `INSERT INTO build_log_streams (environment_id, epoch, generation, revision, head_sha)
		VALUES ($1, 1, $2, $3, 'sha-1')
		ON CONFLICT (environment_id) DO UPDATE SET epoch = build_log_streams.epoch + 1,
		  generation = EXCLUDED.generation, revision = EXCLUDED.revision, head_sha = EXCLUDED.head_sha,
		  state = 'open', next_sequence = 0, stored_bytes = 0, truncated = false, omitted_lines = 0,
		  lost_lines = 0, opened_at = clock_timestamp(), finished_at = NULL, pruned_at = NULL, deleted_at = NULL
		RETURNING epoch`, f.environmentID, generation, revision).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "DELETE FROM build_logs WHERE environment_id=$1", f.environmentID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return epoch
}

func redactor(t *testing.T, values ...string) *secrets.Redactor {
	t.Helper()
	r, err := secrets.NewRedactor(values...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *fixture) open(t *testing.T, epoch int64, limits buildlogs.Limits) *buildlogs.Writer {
	t.Helper()
	w, err := f.store.Open(t.Context(), buildlogs.Binding{EnvironmentID: f.environmentID, Epoch: epoch}, redactor(t), limits)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func (f *fixture) texts(t *testing.T, epoch int64) []string {
	t.Helper()
	lines, err := f.store.LinesFrom(t.Context(), f.environmentID, epoch, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		if l.Sequence != int64(i) {
			t.Fatalf("sequence gap at %d: %d", i, l.Sequence)
		}
		out[i] = l.Text
	}
	return out
}

func TestStoreAppendsContinuesAndFencesEarlierAttempts(t *testing.T) {
	f := setup(t)
	epoch := f.admit(t, 1, 1)
	w := f.open(t, epoch, buildlogs.Limits{})
	if _, err := fmt.Fprint(w, "build step 1\nbuild step 2\n"); err != nil {
		t.Fatal(err)
	}
	w.System("Image built")
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Startup output for the same attempt continues the sequence.
	startup := f.open(t, epoch, buildlogs.Limits{})
	if _, err := fmt.Fprint(startup, "listening\n"); err != nil {
		t.Fatal(err)
	}
	if err := startup.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.texts(t, epoch), "|"); got != "build step 1|build step 2|Image built|listening" {
		t.Fatalf("stored: %s", got)
	}

	late := f.open(t, epoch, buildlogs.Limits{}) // Opened before a retry is admitted.
	retry := f.admit(t, 1, 1)                    // Same revision, new execution.
	if retry != epoch+1 {
		t.Fatalf("retry epoch %d", retry)
	}
	if _, err := fmt.Fprint(late, "output from the superseded execution\n"); err != nil {
		t.Fatal(err)
	}
	if err := late.Close(t.Context()); !errors.Is(err, buildlogs.ErrSuperseded) {
		t.Fatalf("late writer close: %v", err)
	}
	if _, err := f.store.Open(t.Context(), buildlogs.Binding{EnvironmentID: f.environmentID, Epoch: epoch}, redactor(t), buildlogs.Limits{}); !errors.Is(err, buildlogs.ErrSuperseded) {
		t.Fatalf("opening an earlier attempt: %v", err)
	}
	var stale int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM build_logs WHERE epoch<>$1", retry).Scan(&stale); err != nil || stale != 0 {
		t.Fatalf("earlier attempt lines survived: %d %v", stale, err)
	}
	if got := f.texts(t, retry); len(got) != 0 {
		t.Fatalf("retry inherited lines: %q", got)
	}
}

func TestStoreConcurrentAdmissionNeverMixesAttempts(t *testing.T) {
	f := setup(t)
	first := f.admit(t, 1, 1)
	w := f.open(t, first, buildlogs.Limits{})
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := fmt.Fprintf(w, "old attempt line %d\n", i); err != nil {
				return // Fenced.
			}
		}
	}()
	time.Sleep(300 * time.Millisecond) // Let some batches commit.
	second := f.admit(t, 2, 2)
	time.Sleep(300 * time.Millisecond) // Let the old writer try to append again.
	close(stop)
	wg.Wait()
	if err := w.Close(t.Context()); !errors.Is(err, buildlogs.ErrSuperseded) {
		t.Fatalf("old writer close: %v", err)
	}
	var rows int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM build_logs").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("lines after admission of epoch %d: %d %v", second, rows, err)
	}
}

func TestStoreCapsCountersAndFinish(t *testing.T) {
	f := setup(t)
	epoch := f.admit(t, 1, 1)
	w := f.open(t, epoch, buildlogs.Limits{Lines: 10})
	for i := 0; i < 50; i++ {
		if _, err := fmt.Fprintf(w, "line %d\n", i); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	s, err := f.store.Stream(t.Context(), f.environmentID)
	if err != nil {
		t.Fatal(err)
	}
	if s.Lines != 10 || !s.Truncated || s.OmittedLines != 41 || s.Finished || s.Epoch != epoch {
		t.Fatalf("stream: %+v", s)
	}
	if err := f.store.Finish(t.Context(), f.environmentID, epoch); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Finish(t.Context(), f.environmentID, epoch); err != nil {
		t.Fatal("finish must be idempotent:", err)
	}
	if s, _ := f.store.Stream(t.Context(), f.environmentID); !s.Finished || s.FinishedAt.IsZero() {
		t.Fatalf("not finished: %+v", s)
	}
	if _, err := f.store.Open(t.Context(), buildlogs.Binding{EnvironmentID: f.environmentID, Epoch: epoch}, redactor(t), buildlogs.Limits{}); !errors.Is(err, buildlogs.ErrSuperseded) {
		t.Fatalf("a finished attempt accepted a writer: %v", err)
	}
	if _, err := f.store.Open(t.Context(), buildlogs.Binding{}, redactor(t), buildlogs.Limits{}); !errors.Is(err, buildlogs.ErrNoStream) {
		t.Fatalf("attempt without environment: %v", err)
	}
	if _, err := f.store.Open(t.Context(), buildlogs.Binding{EnvironmentID: f.environmentID, Epoch: epoch}, nil, buildlogs.Limits{}); err == nil {
		t.Fatal("a writer without redaction was opened")
	}
}

func TestStorePagesAndOwnerScope(t *testing.T) {
	f := setup(t)
	epoch := f.admit(t, 1, 1)
	w := f.open(t, epoch, buildlogs.Limits{})
	for i := 0; i < 30; i++ {
		if _, err := fmt.Fprintf(w, "line %d\n", i); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	before, err := f.store.LinesBefore(t.Context(), f.environmentID, epoch, 30, 5)
	if err != nil || len(before) != 5 || before[0].Sequence != 25 || before[4].Text != "line 29" {
		t.Fatalf("tail page: %+v %v", before, err)
	}
	from, err := f.store.LinesFrom(t.Context(), f.environmentID, epoch, 10, 3)
	if err != nil || len(from) != 3 || from[0].Text != "line 10" {
		t.Fatalf("later page: %+v %v", from, err)
	}
	// The PR page quotes the end of the attempt that belongs to its request,
	// and nothing from another generation or revision.
	tail, err := f.store.Tail(t.Context(), f.environmentID, 1, 1, 3)
	if err != nil || len(tail) != 3 || tail[0] != "line 27" || tail[2] != "line 29" {
		t.Fatalf("tail: %q %v", tail, err)
	}
	for _, fence := range [][2]int64{{1, 2}, {2, 1}} {
		if other, err := f.store.Tail(t.Context(), f.environmentID, fence[0], fence[1], 3); err != nil || other != nil {
			t.Fatalf("tail for generation %d revision %d: %q %v", fence[0], fence[1], other, err)
		}
	}
	if none, err := f.store.Tail(t.Context(), f.environmentID+1000, 1, 1, 3); err != nil || none != nil {
		t.Fatalf("tail without a stream: %q %v", none, err)
	}
	if _, err := f.store.Target(t.Context(), f.other, f.repoID, 7); !errors.Is(err, buildlogs.ErrUnavailable) {
		t.Fatalf("another user's PR: %v", err)
	}
	if _, err := f.store.Target(t.Context(), f.owner, f.repoID, 8); !errors.Is(err, buildlogs.ErrUnavailable) {
		t.Fatalf("missing PR: %v", err)
	}
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO build_intents(pull_request_id,revision,base_revision,generation,head_sha,source,request_action,requested_by,idempotency_key,state,available_at,started_at)
		VALUES ($1,1,0,1,'sha-1','manual','build',$2,'fixture-key-0000001','running',now(),now())`, f.prID, f.owner); err != nil {
		t.Fatal(err)
	}
	target, err := f.store.Target(t.Context(), f.owner, f.repoID, 7)
	if err != nil {
		t.Fatal(err)
	}
	if target.EnvironmentID != f.environmentID || target.Intent == nil || target.Intent.State != "running" || target.Repository != "owner/app" || !target.Available {
		t.Fatalf("target: %+v", target)
	}
}

func TestPruneDeletesOnlyExpiredAttempts(t *testing.T) {
	f := setup(t)
	epoch := f.admit(t, 1, 1)
	w := f.open(t, epoch, buildlogs.Limits{})
	if _, err := fmt.Fprint(w, "old output\n"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Finish(t.Context(), f.environmentID, epoch); err != nil {
		t.Fatal(err)
	}
	report, err := f.store.Prune(t.Context(), time.Now().Add(-f.store.Retention()))
	if err != nil || report.Streams != 0 || len(f.texts(t, epoch)) != 1 {
		t.Fatalf("recent logs pruned: %+v %v", report, err)
	}
	if _, err := f.pool.Exec(t.Context(), "UPDATE build_log_streams SET finished_at = now() - interval '15 days'"); err != nil {
		t.Fatal(err)
	}
	report, err = f.store.Prune(t.Context(), time.Now().Add(-f.store.Retention()))
	if err != nil || report.Streams != 1 || report.Lines != 1 || len(f.texts(t, epoch)) != 0 {
		t.Fatalf("expired logs kept: %+v %v", report, err)
	}
	if s, _ := f.store.Stream(t.Context(), f.environmentID); s.PrunedAt.IsZero() {
		t.Fatal("pruned stream not marked")
	}
}

func TestStreamChangesWakeLiveViewers(t *testing.T) {
	f := setup(t)
	cfg, err := pgx.ParseConfig(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = f.schema
	listener, err := pgx.ConnectConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close(context.Background()) }()
	if _, err := listener.Exec(t.Context(), "LISTEN previa_events"); err != nil {
		t.Fatal(err)
	}
	epoch := f.admit(t, 1, 1)
	want := fmt.Sprintf(`{"topic" : "buildlog:%d", "source" : "build_logs"}`, f.environmentID)
	waitFor := func(what string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		for {
			n, err := listener.WaitForNotification(ctx)
			if err != nil {
				t.Fatalf("no notification for %s: %v", what, err)
			}
			if n.Payload == want {
				return
			}
		}
	}
	waitFor("admission")
	w := f.open(t, epoch, buildlogs.Limits{})
	if _, err := fmt.Fprint(w, "hello\n"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitFor("append")
}

// Lines keep the time they were received; a running attempt's logs cannot be
// deleted, a finished one's can (once), and a late writer stores nothing after.
func TestDeleteRemovesAFinishedAttemptsLinesAndFencesWriters(t *testing.T) {
	f := setup(t)
	epoch := f.admit(t, 1, 1)
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO build_intents(pull_request_id,revision,base_revision,generation,head_sha,source,request_action,requested_by,idempotency_key,state,available_at,started_at)
		VALUES ($1,1,0,1,'sha-1','manual','build',$2,'fixture-key-0000001','running',now(),now())`, f.prID, f.owner); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	w := f.open(t, epoch, buildlogs.Limits{})
	if _, err := fmt.Fprint(w, "first\nsecond\n"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	lines, err := f.store.LinesFrom(t.Context(), f.environmentID, epoch, 0, 10)
	if err != nil || len(lines) != 2 {
		t.Fatalf("lines: %v %v", lines, err)
	}
	for _, l := range lines {
		if l.At.Before(before.Add(-time.Second)) || l.At.After(time.Now().Add(time.Second)) {
			t.Fatalf("line %d was stored with time %v", l.Sequence, l.At)
		}
	}
	late := f.open(t, epoch, buildlogs.Limits{})
	if err := f.store.Delete(t.Context(), f.environmentID, epoch); !errors.Is(err, buildlogs.ErrNotDeletable) {
		t.Fatalf("the running attempt's logs were deletable: %v", err)
	}
	if _, err := f.pool.Exec(t.Context(), "UPDATE build_intents SET state='failed', completed_at=now() WHERE pull_request_id=(SELECT pull_request_id FROM environments WHERE id=$1)", f.environmentID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Delete(t.Context(), f.environmentID, epoch+1); !errors.Is(err, buildlogs.ErrNotDeletable) {
		t.Fatalf("another attempt's logs: %v", err)
	}
	if err := f.store.Delete(t.Context(), f.environmentID, epoch); err != nil {
		t.Fatalf("a crashed attempt's logs (stream still open) can be deleted: %v", err)
	}
	if len(f.texts(t, epoch)) != 0 {
		t.Fatal("lines kept")
	}
	s, err := f.store.Stream(t.Context(), f.environmentID)
	if err != nil || s.DeletedAt.IsZero() || s.PrunedAt.IsZero() || !s.Finished {
		t.Fatalf("stream after deletion: %+v %v", s, err)
	}
	if err := f.store.Delete(t.Context(), f.environmentID, epoch); !errors.Is(err, buildlogs.ErrNotDeletable) {
		t.Fatalf("deleting twice: %v", err)
	}
	_, _ = fmt.Fprint(late, "too late\n")
	_ = late.Close(t.Context())
	if len(f.texts(t, epoch)) != 0 {
		t.Fatal("a late writer stored lines after deletion")
	}
	// The next attempt starts with a clean stream.
	next := f.admit(t, 1, 2)
	if s, _ := f.store.Stream(t.Context(), f.environmentID); s.Epoch != next || !s.DeletedAt.IsZero() {
		t.Fatalf("a new attempt still reads as deleted: %+v", s)
	}
}

// A live preview's crash adds a section to the finished log of the attempt
// that built it, redacted and after its lines; never to another attempt's
// or to deleted logs.
func TestOpenFinishedAddsASectionToTheBuiltAttemptsLog(t *testing.T) {
	f := setup(t)
	epoch := f.admit(t, 1, 1)
	w := f.open(t, epoch, buildlogs.Limits{})
	_, _ = fmt.Fprint(w, "built\n")
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.OpenFinished(t.Context(), f.environmentID, 1, 1, redactor(t)); !errors.Is(err, buildlogs.ErrNoStream) {
		t.Fatalf("an open stream takes no section: %v", err)
	}
	if err := f.store.Finish(t.Context(), f.environmentID, epoch); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.OpenFinished(t.Context(), f.environmentID, 1, 2, redactor(t)); !errors.Is(err, buildlogs.ErrNoStream) {
		t.Fatalf("another revision's stream: %v", err)
	}
	section, err := f.store.OpenFinished(t.Context(), f.environmentID, 1, 1, redactor(t, "crash-secret-1"))
	if err != nil {
		t.Fatal(err)
	}
	section.System("The app stopped.")
	_, _ = fmt.Fprint(section, "panic: crash-secret-1\n")
	if err := section.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.texts(t, epoch), "|"); got != "built|The app stopped.|panic: [redacted]" {
		t.Fatalf("lines: %q", got)
	}
	if err := f.store.Delete(t.Context(), f.environmentID, epoch); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.OpenFinished(t.Context(), f.environmentID, 1, 1, redactor(t)); !errors.Is(err, buildlogs.ErrNoStream) {
		t.Fatalf("deleted logs take no section: %v", err)
	}
}

func TestStoreSearchFiltersLiteralTextSourceEpochAndSparseSequences(t *testing.T) {
	f := setup(t)
	epoch := f.admit(t, 1, 1)
	w := f.open(t, epoch, buildlogs.Limits{})
	for i := range 1100 {
		if i%2 == 0 {
			w.System(fmt.Sprintf("Previa %d 50%%_done", i))
		} else {
			if _, err := fmt.Fprintf(w, "Application %d\n", i); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	filter := buildlogs.Filter{Source: "previa", Query: "50%_DONE"}
	lines, err := f.store.SearchBefore(t.Context(), f.environmentID, epoch, 1100, 300, filter)
	if err != nil || len(lines) != 300 || lines[0].Sequence != 500 || lines[299].Sequence != 1098 {
		t.Fatalf("matching tail: %d %v", len(lines), err)
	}
	next, err := f.store.SearchFrom(t.Context(), f.environmentID, epoch, 500, 300, filter)
	if err != nil || len(next) != 300 || next[0].Sequence != 500 || next[299].Sequence != 1098 {
		t.Fatalf("matching forward page: %d %v", len(next), err)
	}
	app, err := f.store.SearchFrom(t.Context(), f.environmentID, epoch, 0, 1000, buildlogs.Filter{Source: "application", Query: "aPpLiCaTiOn"})
	if err != nil || len(app) != 550 || app[0].System {
		t.Fatalf("application source: %d %v", len(app), err)
	}
	missing, err := f.store.SearchBefore(t.Context(), f.environmentID, epoch+1, 1100, 300, filter)
	if err != nil || len(missing) != 0 {
		t.Fatal("search leaked another epoch")
	}
	missing, err = f.store.SearchFrom(t.Context(), f.environmentID+1, epoch, 0, 300, filter)
	if err != nil || len(missing) != 0 {
		t.Fatal("search leaked another environment")
	}
	missing, err = f.store.SearchFrom(t.Context(), f.environmentID, epoch, 0, 300, buildlogs.Filter{Query: "%_wildcard"})
	if err != nil || len(missing) != 0 {
		t.Fatal("search interpreted SQL wildcards")
	}
}
