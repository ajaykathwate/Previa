package reporting

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
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type reportFixture struct {
	pool     *pgxpool.Pool
	github   *fakeGitHub
	reporter *Reporter
	prID     int64
	repoID   int64
	headSHA  string
}

const fixtureRepoID = 55

func newReportFixture(t *testing.T) *reportFixture {
	t.Helper()
	f := &reportFixture{pool: migratedPool(t), github: newFakeGitHub(t, fixtureRepoID), headSHA: headB}
	f.seed(t, 900, fixtureRepoID, 7)
	reporter, err := NewReporter(f.pool, f.github.api(t), settings, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	reporter.now = func() time.Time { return now }
	f.reporter = reporter
	return f
}

// migratedPool returns a pool on a throwaway schema with every migration.
func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	connection := os.Getenv("TEST_DATABASE_URL")
	if connection == "" {
		t.Skip("set TEST_DATABASE_URL for reporting integration tests")
	}
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, connection)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("reporting_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
	return pool
}

// seed stores one open PR (generation 5) at f.headSHA.
func (f *reportFixture) seed(t *testing.T, installationID, repoID int64, number int32) {
	t.Helper()
	ctx := t.Context()
	var user, installation int64
	mustScan(t, f.pool.QueryRow(ctx, "INSERT INTO users(github_id, username) VALUES (1, 'owner') RETURNING id"), &user)
	mustScan(t, f.pool.QueryRow(ctx, "INSERT INTO installations(github_installation_id, owner_user_id, status) VALUES ($1, $2, 'active') RETURNING id", installationID, user), &installation)
	mustScan(t, f.pool.QueryRow(ctx, "INSERT INTO repos(installation_id, full_name, github_repo_id) VALUES ($1, 'octo/app', $2) RETURNING id", installation, repoID), &f.repoID)
	mustScan(t, f.pool.QueryRow(ctx, "INSERT INTO pull_requests(repo_id, number, head_sha, generation, build_status) VALUES ($1, $2, $3, 5, 'open') RETURNING id", f.repoID, number, f.headSHA), &f.prID)
}

func mustScan(t *testing.T, row pgx.Row, dest ...any) {
	t.Helper()
	if err := row.Scan(dest...); err != nil {
		t.Fatal(err)
	}
}

func (f *reportFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// intent sets the PR's build request, as admission and the job handoff do.
func (f *reportFixture) intent(t *testing.T, state string, revision int64) {
	t.Helper()
	f.exec(t, `INSERT INTO build_intents(pull_request_id, revision, base_revision, generation, head_sha, source, request_action, idempotency_key, state, available_at, started_at, completed_at)
		SELECT id, $2::bigint, $2::bigint - 1, generation, head_sha, 'automatic', 'automatic', 'key-'||$2::bigint::text, $3::text, now() - interval '1 minute',
		       CASE WHEN $3::text IN ('running','succeeded','failed') THEN now() END,
		       CASE WHEN $3::text IN ('succeeded','failed','cancelled','superseded') THEN now() END
		FROM pull_requests WHERE id = $1
		ON CONFLICT (pull_request_id) DO UPDATE SET revision = EXCLUDED.revision, base_revision = EXCLUDED.base_revision,
		  generation = EXCLUDED.generation, head_sha = EXCLUDED.head_sha, state = EXCLUDED.state,
		  started_at = EXCLUDED.started_at, completed_at = EXCLUDED.completed_at`, f.prID, revision, state)
}

// environment sets the lifetime environment's state.
func (f *reportFixture) environment(t *testing.T, status string, deployed string, revision int64, code string) {
	t.Helper()
	var containerID, deployedSHA, errorCode any
	if deployed != "" {
		containerID, deployedSHA = "container-"+deployed[:4], deployed
	}
	if code != "" {
		errorCode = code
	}
	f.exec(t, `INSERT INTO environments(pull_request_id, subdomain, status, container_id, image_tag, head_sha, applied_generation, applied_revision, last_error_code)
		VALUES ($1, 'pr-7-1j-abcdefgh', $2::text, $3::text, $3::text, $4::text, 5, $5::bigint, $6::text)
		ON CONFLICT (pull_request_id) DO UPDATE SET status = EXCLUDED.status, container_id = EXCLUDED.container_id, image_tag = EXCLUDED.image_tag,
		  head_sha = EXCLUDED.head_sha, applied_generation = EXCLUDED.applied_generation, applied_revision = EXCLUDED.applied_revision,
		  last_error_code = EXCLUDED.last_error_code`, f.prID, status, containerID, deployedSHA, revision, errorCode)
}

// want records a reportable change, as environments' reportTx does.
func (f *reportFixture) want(t *testing.T) {
	t.Helper()
	f.exec(t, `INSERT INTO pull_request_reports(pull_request_id, wanted_version) VALUES ($1, 1)
		ON CONFLICT (pull_request_id) DO UPDATE SET wanted_version = pull_request_reports.wanted_version + 1`, f.prID)
}

func (f *reportFixture) sync(t *testing.T) {
	t.Helper()
	if err := f.reporter.Sync(t.Context(), f.prID, "test-reporter"); err != nil {
		t.Fatal(err)
	}
}

func (f *reportFixture) comment(t *testing.T) *fakeComment {
	t.Helper()
	comments := f.github.appComments(7)
	if len(comments) != 1 {
		t.Fatalf("PR has %d Previa comments", len(comments))
	}
	return comments[0]
}

func (f *reportFixture) versions(t *testing.T) (wanted, reported int64, code string) {
	t.Helper()
	var c *string
	mustScan(t, f.pool.QueryRow(t.Context(), "SELECT wanted_version, reported_version, last_error_code FROM pull_request_reports WHERE pull_request_id=$1", f.prID), &wanted, &reported, &c)
	if c != nil {
		code = *c
	}
	return wanted, reported, code
}

func TestReporterReportsOnceAndSkipsUnchangedState(t *testing.T) {
	f := newReportFixture(t)
	f.intent(t, "queued", 1)
	f.want(t)
	f.sync(t)
	c := f.comment(t)
	if !strings.Contains(c.Body, "Previa preview: Queued") || CommentVersion(c.Body) != 1 {
		t.Fatalf("comment %q", c.Body)
	}
	checks := f.github.checksFor(headB)
	if len(checks) != 1 || checks[0].Status != "queued" || checks[0].ExternalID != fmt.Sprintf("previa:pr:%d:%s:r1", f.prID, headB) {
		t.Fatalf("checks %+v", checks)
	}
	var stored int64
	mustScan(t, f.pool.QueryRow(t.Context(), "SELECT github_comment_id FROM pull_requests WHERE id=$1", f.prID), &stored)
	if stored != c.ID {
		t.Fatal("comment ID not stored")
	}
	// A duplicate report for the same version does nothing at all.
	calls := len(f.github.calls)
	f.sync(t)
	if len(f.github.calls) != calls {
		t.Fatal("a reported version was written again")
	}
	// A new version with an unchanged rendering costs no write.
	f.want(t)
	f.sync(t)
	if f.github.count("PATCH") != 0 || f.github.count("POST") != 2 {
		t.Fatalf("unchanged state written: %v", f.github.calls)
	}
	if wanted, reported, _ := f.versions(t); wanted != 2 || reported != 2 {
		t.Fatalf("versions %d/%d", wanted, reported)
	}
}

func TestReporterAlwaysRendersTheLatestState(t *testing.T) {
	f := newReportFixture(t)
	f.intent(t, "queued", 1)
	f.want(t) // the queued report is still pending ...
	f.intent(t, "running", 1)
	f.want(t) // ... when the build starts
	f.environment(t, "running", headB, 1, "")
	f.intent(t, "succeeded", 1)
	f.want(t)
	// Whichever operation runs first reports the current state; later ones
	// find nothing left to report.
	f.sync(t)
	f.sync(t)
	f.sync(t)
	c := f.comment(t)
	if !strings.Contains(c.Body, "Previa preview: Ready") || !strings.Contains(c.Body, "https://pr-7-1j-abcdefgh.preview.example.com") {
		t.Fatalf("comment %q", c.Body)
	}
	checks := f.github.checksFor(headB)
	if len(checks) != 1 || checks[0].Conclusion != "success" {
		t.Fatalf("checks %+v", checks)
	}
	// A late failure report of the same revision cannot replace the success:
	// the renderer reads the environment, which still says running.
	f.want(t)
	f.sync(t)
	if !strings.Contains(f.comment(t).Body, "Ready") || f.github.checksFor(headB)[0].Conclusion != "success" {
		t.Fatal("a stale report replaced the current state")
	}
}

func TestReporterRecoversDeletedAndLostComments(t *testing.T) {
	f := newReportFixture(t)
	f.intent(t, "running", 1)
	f.want(t)
	f.sync(t)
	first := f.comment(t)
	// A person deletes the comment; the next change recreates it.
	f.github.mu.Lock()
	delete(f.github.comments, first.ID)
	f.github.mu.Unlock()
	f.intent(t, "failed", 1)
	f.environment(t, "failed", "", 1, "build_failed")
	f.want(t)
	f.sync(t)
	recreated := f.comment(t)
	if recreated.ID == first.ID || !strings.Contains(recreated.Body, "The image build failed.") {
		t.Fatalf("recreated %+v", recreated)
	}
	// GitHub accepted a comment but saving its ID failed: discovery finds it
	// instead of creating a second one.
	f.exec(t, "UPDATE pull_requests SET github_comment_id = NULL WHERE id=$1", f.prID)
	f.intent(t, "running", 2)
	f.want(t)
	f.sync(t)
	again := f.comment(t)
	var stored int64
	mustScan(t, f.pool.QueryRow(t.Context(), "SELECT github_comment_id FROM pull_requests WHERE id=$1", f.prID), &stored)
	if again.ID != recreated.ID || stored != recreated.ID || !strings.Contains(again.Body, "Building") {
		t.Fatalf("ambiguous create: %+v stored %d", again, stored)
	}
}

func TestReporterGivesEachRevisionItsOwnCheck(t *testing.T) {
	f := newReportFixture(t)
	f.intent(t, "running", 1)
	f.want(t)
	f.sync(t)
	// A same-commit Rebuild: the open run is stopped, a new one tracks it.
	f.intent(t, "running", 2)
	f.want(t)
	f.sync(t)
	f.want(t)
	f.sync(t)
	checks := f.github.checksFor(headB)
	sort.Slice(checks, func(i, j int) bool { return checks[i].ID < checks[j].ID })
	if len(checks) != 2 || checks[0].Conclusion != "cancelled" || checks[1].Status != "in_progress" || !strings.HasSuffix(checks[1].ExternalID, ":r2") {
		t.Fatalf("checks %+v", checks)
	}
	// Losing the stored run's ID still finds it by its external ID.
	f.exec(t, "UPDATE pull_request_reports SET check_run_id = NULL, check_head_sha = NULL, check_revision = NULL, check_open = false WHERE pull_request_id=$1", f.prID)
	f.environment(t, "running", headB, 2, "")
	f.intent(t, "succeeded", 2)
	f.want(t)
	f.sync(t)
	if checks := f.github.checksFor(headB); len(checks) != 2 {
		t.Fatalf("duplicate check run: %d", len(checks))
	}
}

func TestReporterNewCommitAndRemoval(t *testing.T) {
	f := newReportFixture(t)
	f.environment(t, "running", headB, 1, "")
	f.intent(t, "succeeded", 1)
	f.want(t)
	f.sync(t)
	// A new commit in Manual mode: nothing requested for it; the comment shows
	// the deployed commit and its check result stays.
	headC := strings.Repeat("c", 40)
	f.exec(t, "UPDATE pull_requests SET head_sha=$2, generation=6 WHERE id=$1", f.prID, headC)
	f.want(t)
	f.sync(t)
	body := f.comment(t).Body
	if !strings.Contains(body, "earlier commit") || !strings.Contains(body, "https://pr-7-1j-abcdefgh") {
		t.Fatalf("comment %q", body)
	}
	if checks := f.github.checksFor(headB); len(checks) != 1 || checks[0].Conclusion != "success" || len(f.github.checksFor(headC)) != 0 {
		t.Fatal("checks changed for an unbuilt commit")
	}
	// Close: the preview goes, the completed check stays as it was.
	f.exec(t, "UPDATE pull_requests SET state='closed', generation=7 WHERE id=$1", f.prID)
	f.environment(t, "torn_down", "", 1, "")
	f.want(t)
	f.sync(t)
	body = f.comment(t).Body
	if !strings.Contains(body, "Preview removed") || strings.Contains(body, "https://pr-7") {
		t.Fatalf("comment %q", body)
	}
	if checks := f.github.checksFor(headB); checks[0].Conclusion != "success" {
		t.Fatal("removal rewrote a completed check")
	}
}

func TestReporterRemovalStopsAnUnfinishedCheck(t *testing.T) {
	f := newReportFixture(t)
	f.intent(t, "running", 1)
	f.environment(t, "queued", "", 0, "")
	f.want(t)
	f.sync(t)
	f.exec(t, "UPDATE pull_requests SET state='closed', generation=6 WHERE id=$1", f.prID)
	f.intent(t, "superseded", 1)
	f.environment(t, "torn_down", "", 0, "")
	f.want(t)
	f.sync(t)
	if checks := f.github.checksFor(headB); len(checks) != 1 || checks[0].Conclusion != "cancelled" {
		t.Fatalf("checks %+v", checks)
	}
}

func TestReporterRetriesRateLimitsAndOutages(t *testing.T) {
	f := newReportFixture(t)
	f.intent(t, "queued", 1)
	f.want(t)
	f.github.failOnce("POST comments", 499)
	err := f.reporter.Sync(t.Context(), f.prID, "test-reporter")
	var failure *environments.OperationFailure
	if !errors.As(err, &failure) || failure.Code != "github_rate_limited" || failure.Permanent || failure.RetryAfter < time.Minute {
		t.Fatalf("rate limit: %v", err)
	}
	f.github.failOnce("POST check-runs", 500)
	err = f.reporter.Sync(t.Context(), f.prID, "test-reporter")
	if !errors.As(err, &failure) || failure.Code != "github_unavailable" || failure.Permanent {
		t.Fatalf("outage: %v", err)
	}
	// The comment written before the outage is kept, not duplicated.
	f.sync(t)
	if f.comment(t) == nil || len(f.github.checksFor(headB)) != 1 {
		t.Fatal("retry did not converge")
	}
	if _, reported, _ := f.versions(t); reported != 1 {
		t.Fatalf("reported %d", reported)
	}
}

func TestReporterRespectsRevokedAccessAndPermissions(t *testing.T) {
	f := newReportFixture(t)
	f.intent(t, "queued", 1)
	f.exec(t, "UPDATE installations SET status='suspended'")
	f.want(t)
	f.sync(t)
	if len(f.github.calls) != 0 {
		t.Fatal("reported to GitHub without access")
	}
	if wanted, reported, code := f.versions(t); reported != wanted || code != "inaccessible" {
		t.Fatalf("revoked: %d/%d %q", wanted, reported, code)
	}
	// The App lost Checks permission: the comment still updates.
	f.exec(t, "UPDATE installations SET status='active'")
	f.want(t)
	f.github.failOnce("POST check-runs", 403)
	f.sync(t)
	if f.comment(t) == nil {
		t.Fatal("comment not written")
	}
	if _, _, code := f.versions(t); code != "check_permission_denied" {
		t.Fatalf("code %q", code)
	}
	// Neither part allowed: a permanent failure, not an endless retry.
	f.intent(t, "running", 1)
	f.want(t)
	f.github.failOnce("GET comment", 403)
	f.github.failOnce("POST check-runs", 403)
	err := f.reporter.Sync(t.Context(), f.prID, "test-reporter")
	var failure *environments.OperationFailure
	if !errors.As(err, &failure) || !failure.Permanent {
		t.Fatalf("permissions removed: %v", err)
	}
}

func TestReporterSerializesPerPR(t *testing.T) {
	f := newReportFixture(t)
	f.intent(t, "queued", 1)
	f.want(t)
	f.exec(t, "UPDATE pull_request_reports SET lease_owner='other', lease_expires_at=now()+interval '1 minute' WHERE pull_request_id=$1", f.prID)
	err := f.reporter.Sync(t.Context(), f.prID, "test-reporter")
	var failure *environments.OperationFailure
	if !errors.As(err, &failure) || failure.Code != "report_in_progress" || failure.RetryAfter <= 0 {
		t.Fatalf("held lease: %v", err)
	}
	if len(f.github.calls) != 0 {
		t.Fatal("wrote while another reporter held the PR")
	}
	// A crashed reporter's lease expires.
	f.exec(t, "UPDATE pull_request_reports SET lease_expires_at=now()-interval '1 second' WHERE pull_request_id=$1", f.prID)
	f.sync(t)
	var owner *string
	mustScan(t, f.pool.QueryRow(t.Context(), "SELECT lease_owner FROM pull_request_reports WHERE pull_request_id=$1", f.prID), &owner)
	if owner != nil || f.comment(t) == nil {
		t.Fatal("lease not taken over and released")
	}
	// A PR nobody ever reported on needs no work.
	if err := f.reporter.Sync(t.Context(), f.prID+1000, "test-reporter"); err != nil {
		t.Fatal(err)
	}
}

func TestReporterNeverOverwritesANewerComment(t *testing.T) {
	f := newReportFixture(t)
	f.intent(t, "queued", 1)
	f.want(t)
	f.sync(t)
	c := f.comment(t)
	// A newer reporter (version 9) wrote last; this report is for version 2.
	f.github.mu.Lock()
	c.Body = Marker + "\n<!-- previa:version 9 -->\nnewer"
	f.github.mu.Unlock()
	f.intent(t, "running", 1)
	f.want(t)
	f.sync(t)
	if !strings.HasSuffix(f.comment(t).Body, "newer") {
		t.Fatal("an older report overwrote a newer comment")
	}
}

func TestReporterExecutesOnlyReports(t *testing.T) {
	f := newReportFixture(t)
	err := f.reporter.Execute(t.Context(), environments.Operation{Kind: environments.CleanupArtifact, PullRequestID: f.prID})
	var failure *environments.OperationFailure
	if !errors.As(err, &failure) || !failure.Permanent {
		t.Fatalf("got %v", err)
	}
}
