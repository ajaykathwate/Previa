package onboarding

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	pages "github.com/ajaykathwatee/previa/web/pages/onboarding"
	"github.com/jackc/pgx/v5"
)

func TestSetupIsForPeopleWhoCanBuildInTheRepository(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	for name, user := range map[string]int64{"owner": f.owner, "admin": f.admin, "granted developer": f.dev} {
		if p, err := f.service.Page(ctx, user, f.repo, 0); err != nil || p.Name != "acme/api" || p.Selected == nil || p.Selected.Number != 42 {
			t.Errorf("%s: %v %+v", name, err, p.Selected)
		}
	}
	for name, attempt := range map[string][2]int64{
		"reviewer": {f.reviewer, f.repo}, "outsider": {f.outsider, f.repo}, "developer without a grant": {f.dev, f.otherRepo},
		"another workspace's repository": {f.owner, f.outsideRepo},
	} {
		if _, err := f.service.Page(ctx, attempt[0], attempt[1], 0); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := f.service.RequestCheck(ctx, f.reviewer, f.repo, 42, testSHA); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a Reviewer cannot request checks: %v", err)
	}
}

func TestSetupResumesOnTheRightPullRequest(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	newer := strings.Repeat("a", 40)
	f.addPR(t, 43, newer, -time.Minute)
	if p, _ := f.service.Page(ctx, f.owner, f.repo, 0); p.Selected.Number != 43 || len(p.Options) != 2 || p.OpenCount != 2 {
		t.Fatalf("without history the newest pull request is chosen: %+v", p.Selected)
	}
	if _, err := f.service.RequestCheck(ctx, f.owner, f.repo, 42, testSHA); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.service.Page(ctx, f.owner, f.repo, 0); p.Selected.Number != 42 {
		t.Fatalf("setup continues where it was checked last: %+v", p.Selected)
	}
	if p, _ := f.service.Page(ctx, f.owner, f.repo, 43); p.Selected.Number != 43 {
		t.Fatalf("an explicit choice wins: %+v", p.Selected)
	}
	if _, err := f.pool.Exec(ctx, "UPDATE pull_requests SET state='closed' WHERE number=43 AND repo_id=$1", f.repo); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Page(ctx, f.owner, f.repo, 43); !errors.Is(err, ErrClosed) {
		t.Fatalf("a closed pull request: %v", err)
	}
	if _, err := f.service.Page(ctx, f.owner, f.repo, 99); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("an unknown pull request: %v", err)
	}
}

func TestSecretNamesAreShownOnlyToManagers(t *testing.T) {
	f := newFixture(t)
	f.secrets.names = []string{"API_KEY"}
	p, err := f.service.Page(t.Context(), f.owner, f.repo, 0)
	if err != nil || !p.Facts.Secrets.Manager || !slices.Equal(p.Facts.Secrets.Names, []string{"API_KEY"}) {
		t.Fatalf("owner: %+v %v", p.Facts.Secrets, err)
	}
	p, err = f.service.Page(t.Context(), f.dev, f.repo, 0)
	if err != nil || p.Facts.Secrets.Manager || len(p.Facts.Secrets.Names) != 0 || len(p.Facts.Secrets.Managers) != 2 ||
		p.Facts.Secrets.Managers[0].Name != "Olive Owner" || p.Facts.Secrets.Managers[0].Role != "owner" {
		t.Fatalf("a Developer sees who manages secrets, not their names: %+v %v", p.Facts.Secrets, err)
	}
}

func TestACheckIsRequestedOncePerCommit(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	outcome, err := f.service.RequestCheck(ctx, f.dev, f.repo, 42, testSHA)
	if err != nil || outcome != CheckRequested || f.jobs.last() == (CheckArgs{}) {
		t.Fatalf("first request: %s %v", outcome, err)
	}
	row := f.check(t, f.pr)
	if row.State != "queued" || row.Revision != 1 || row.Head != testSHA || f.jobs.last().Revision != 1 {
		t.Fatalf("queued: %+v %+v", row, f.jobs.last())
	}
	if outcome, _ = f.service.RequestCheck(ctx, f.dev, f.repo, 42, testSHA); outcome != CheckRunning || len(f.jobs.args) != 1 {
		t.Fatalf("a check on its way is not asked for twice: %s", outcome)
	}
	if _, err := f.pool.Exec(ctx, "UPDATE repository_setup_checks SET requested_at=requested_at-interval '3 minutes'"); err != nil {
		t.Fatal(err)
	}
	if outcome, _ = f.service.RequestCheck(ctx, f.dev, f.repo, 42, testSHA); outcome != CheckRequested || f.check(t, f.pr).Revision != 2 || f.jobs.last().Revision != 2 {
		t.Fatalf("a check that never finished can be asked for again: %s", outcome)
	}
	if _, err := f.pool.Exec(ctx, "UPDATE repository_setup_checks SET state='complete'"); err != nil {
		t.Fatal(err)
	}
	if outcome, _ = f.service.RequestCheck(ctx, f.dev, f.repo, 42, testSHA); outcome != CheckRecent {
		t.Fatalf("a double click is one request: %s", outcome)
	}
	if _, err := f.service.RequestCheck(ctx, f.dev, f.repo, 42, strings.Repeat("b", 40)); !errors.Is(err, ErrChanged) {
		t.Fatalf("the page showed another commit: %v", err)
	}
	var intents int
	var mode string
	if err := f.pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM build_intents), (SELECT build_mode FROM repos WHERE id=$1)", f.repo).Scan(&intents, &mode); err != nil || intents != 0 || mode != "manual" {
		t.Fatalf("checks never request a build or turn on Automatic builds: %d %q %v", intents, mode, err)
	}
	if _, err := f.pool.Exec(ctx, "UPDATE repos SET accessible=false WHERE id=$1", f.repo); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.RequestCheck(ctx, f.owner, f.repo, 42, testSHA); !errors.Is(err, ErrAccess) {
		t.Fatalf("lost access: %v", err)
	}
	if _, err := f.pool.Exec(ctx, "UPDATE repos SET accessible=true WHERE id=$1", f.repo); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, "UPDATE pull_requests SET state='closed' WHERE id=$1", f.pr); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.RequestCheck(ctx, f.owner, f.repo, 42, testSHA); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed: %v", err)
	}
	noApp, _ := NewService(f.pool, f.jobs, f.platform, f.caps, f.secrets, Options{})
	if _, err := noApp.RequestCheck(ctx, f.owner, f.repo, 42, testSHA); !errors.Is(err, ErrNoGitHub) {
		t.Fatalf("without the App: %v", err)
	}
}

func TestAWorkspaceHasAnHourlyCheckAllowance(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	limited, _ := NewService(f.pool, f.jobs, f.platform, f.caps, f.secrets, Options{Discover: true, Allowance: 1})
	if _, err := limited.RequestCheck(ctx, f.owner, f.repo, 42, testSHA); err != nil {
		t.Fatal(err)
	}
	other := strings.Repeat("d", 40)
	f.addPR(t, 44, other, 0)
	_, err := limited.RequestCheck(ctx, f.owner, f.repo, 44, other)
	var limit *LimitError
	if !errors.As(err, &limit) || limit.ResetsAt.Before(time.Now().Add(50*time.Minute)) {
		t.Fatalf("allowance spent: %v", err)
	}
	if _, err := f.pool.Exec(ctx, "UPDATE setup_check_budgets SET window_started_at=window_started_at-interval '61 minutes'"); err != nil {
		t.Fatal(err)
	}
	if outcome, err := limited.RequestCheck(ctx, f.owner, f.repo, 44, other); err != nil || outcome != CheckRequested {
		t.Fatalf("a new hour: %s %v", outcome, err)
	}
}

// fakeFiles serves one tree for every commit and can change the database
// while "GitHub" answers.
type fakeFiles struct {
	tree   Tree
	err    error
	during func()
}

func (f *fakeFiles) Commit(context.Context, int64, int64, string) (Tree, error) {
	if f.during != nil {
		f.during()
	}
	return f.tree, f.err
}

func readyTree() *fakeTree {
	return &fakeTree{files: map[string]string{".previewrc.yaml": "port: 8080\nhealth_check_path: /healthz\n", "Dockerfile": "FROM scratch"}}
}

func runner(t *testing.T, f *fixture, files *fakeFiles) *Runner {
	t.Helper()
	r, err := NewRunner(f.pool, files, f.platform, slogDiscard())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestTheWorkerRecordsAResultOnlyWhileItStillApplies(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	files := &fakeFiles{tree: readyTree()}
	run := runner(t, f, files)
	if _, err := f.service.RequestCheck(ctx, f.owner, f.repo, 42, testSHA); err != nil {
		t.Fatal(err)
	}
	args := f.jobs.last()
	if err := run.Run(ctx, args, false); err != nil {
		t.Fatal(err)
	}
	row := f.check(t, f.pr)
	result, ok := decodeResult(row.Result)
	if row.State != "complete" || row.ResultSHA != testSHA || row.Platform != "fp" || !ok || !result.Passed() || result.Port != 8080 {
		t.Fatalf("completed: %+v %+v", row, result)
	}
	if p, _ := f.service.Page(ctx, f.owner, f.repo, 42); p.Steps[1].State != pages.StateDone || p.Steps[2].Form != "build" {
		t.Fatalf("the checklist moves on to building: %+v", p.Steps)
	}

	// A newer request replaces an older job: the older one records nothing.
	if _, err := f.pool.Exec(ctx, "UPDATE repository_setup_checks SET requested_at=requested_at-interval '1 minute'"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.RequestCheck(ctx, f.owner, f.repo, 42, testSHA); err != nil {
		t.Fatal(err)
	}
	if err := run.Run(ctx, args, false); err != nil || f.check(t, f.pr).State != "queued" {
		t.Fatalf("a replaced job: %v %+v", err, f.check(t, f.pr))
	}

	// A commit pushed while checking makes the result stale, not current.
	args = f.jobs.last()
	files.during = func() {
		if _, err := f.pool.Exec(ctx, "UPDATE pull_requests SET head_sha=$2,generation=generation+1 WHERE id=$1", f.pr, strings.Repeat("e", 40)); err != nil {
			t.Error(err)
		}
	}
	if err := run.Run(ctx, args, false); err != nil {
		t.Fatal(err)
	}
	if row = f.check(t, f.pr); row.State != "failed" || row.Failure != FailStaleCommit || row.ResultSHA != testSHA {
		t.Fatalf("pushed while checking: %+v", row)
	}
}

func TestTheWorkerKeepsTheLastResultWhenGitHubFails(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	files := &fakeFiles{tree: readyTree()}
	run := runner(t, f, files)
	request := func() CheckArgs {
		t.Helper()
		if _, err := f.pool.Exec(ctx, "UPDATE repository_setup_checks SET requested_at=requested_at-interval '1 minute'"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.RequestCheck(ctx, f.owner, f.repo, 42, testSHA); err != nil {
			t.Fatal(err)
		}
		return f.jobs.last()
	}
	if err := run.Run(ctx, request(), false); err != nil {
		t.Fatal(err)
	}
	files.tree = &fakeTree{err: &AttemptError{Code: FailRateLimited}}
	if err := run.Run(ctx, request(), false); err != nil {
		t.Fatal(err)
	}
	row := f.check(t, f.pr)
	if row.State != "failed" || row.Failure != FailRateLimited || row.ResultSHA != testSHA || row.Checked.IsZero() {
		t.Fatalf("rate limited: %+v", row)
	}
	p, _ := f.service.Page(ctx, f.owner, f.repo, 42)
	if p.Steps[1].State != pages.StateProblem || p.Build != nil {
		t.Fatalf("a failed attempt is shown as one, with no build offered: %+v", p.Steps[1])
	}

	// An unexpected error is retried by the queue, then recorded.
	files.tree, files.err = nil, errors.New("worker bug")
	args := request()
	if err := run.Run(ctx, args, false); err == nil || f.check(t, f.pr).State != "running" {
		t.Fatalf("retried: %v %+v", err, f.check(t, f.pr))
	}
	if err := run.Run(ctx, args, true); err != nil || f.check(t, f.pr).Failure != FailInternal {
		t.Fatalf("the last attempt records the failure: %v %+v", err, f.check(t, f.pr))
	}
}

func TestTheWorkerNoticesLostAccessAndClosedPullRequests(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	files := &fakeFiles{tree: readyTree()}
	run := runner(t, f, files)
	if _, err := f.service.RequestCheck(ctx, f.owner, f.repo, 42, testSHA); err != nil {
		t.Fatal(err)
	}
	files.during = func() {
		if _, err := f.pool.Exec(ctx, "UPDATE installations SET status='suspended' WHERE id=$1", f.installation); err != nil {
			t.Error(err)
		}
	}
	if err := run.Run(ctx, f.jobs.last(), false); err != nil {
		t.Fatal(err)
	}
	if row := f.check(t, f.pr); row.State != "failed" || row.Failure != FailAccess || len(row.Result) != 0 {
		t.Fatalf("revoked while checking: no result may be recorded: %+v", row)
	}
	if _, err := f.pool.Exec(ctx, "UPDATE installations SET status='active' WHERE id=$1", f.installation); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, "UPDATE repository_setup_checks SET requested_at=requested_at-interval '1 minute'"); err != nil {
		t.Fatal(err)
	}
	files.during = nil
	if _, err := f.service.RequestCheck(ctx, f.owner, f.repo, 42, testSHA); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, "UPDATE pull_requests SET state='closed' WHERE id=$1", f.pr); err != nil {
		t.Fatal(err)
	}
	if err := run.Run(ctx, f.jobs.last(), false); err != nil || f.check(t, f.pr).Failure != FailClosed {
		t.Fatalf("closed before the worker started: %v %+v", err, f.check(t, f.pr))
	}
}

func TestOpenRecordsOnlyOpensFromPrevia(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	if _, err := f.service.Open(ctx, f.owner, f.repo, 42, true); !errors.Is(err, ErrNotDeployed) {
		t.Fatalf("nothing deployed: %v", err)
	}
	f.deploy(t, f.pr)
	address, err := f.service.Open(ctx, f.dev, f.repo, 42, false)
	if err != nil || address != "https://pr-42-fixture.preview.test" {
		t.Fatalf("open: %q %v", address, err)
	}
	var opens int
	_ = f.pool.QueryRow(ctx, "SELECT count(*) FROM preview_opens").Scan(&opens)
	if opens != 0 {
		t.Fatal("an open from elsewhere was recorded")
	}
	if _, err := f.service.Open(ctx, f.dev, f.repo, 42, true); err != nil {
		t.Fatal(err)
	}
	p, _ := f.service.Page(ctx, f.dev, f.repo, 42)
	if last := p.Steps[len(p.Steps)-1]; last.State != pages.StateDone || !strings.Contains(last.Summary, "by you") {
		t.Fatalf("opened: %+v", last)
	}
	if _, err := f.service.Open(ctx, f.outsider, f.repo, 42, true); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("an outsider: %v", err)
	}
}

func TestTheOverviewJourneyFollowsTheWorkspace(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	j, err := f.service.Journey(ctx, f.workspace, f.owner)
	if err != nil || j.Repositories != 2 || j.Checked || j.Published || j.Opened || j.FocusID != f.repo {
		t.Fatalf("a new workspace: %+v %v", j, err)
	}
	j, _ = f.service.Journey(ctx, f.workspace, f.newdev)
	if j.Repositories != 0 || j.FocusID != 0 || len(j.Managers) != 2 {
		t.Fatalf("a Developer without access learns whom to ask: %+v", j)
	}
	run := runner(t, f, &fakeFiles{tree: readyTree()})
	if _, err := f.service.RequestCheck(ctx, f.owner, f.repo, 42, testSHA); err != nil {
		t.Fatal(err)
	}
	if err := run.Run(ctx, f.jobs.last(), false); err != nil {
		t.Fatal(err)
	}
	if j, _ = f.service.Journey(ctx, f.workspace, f.dev); !j.Checked || j.Published || j.Repositories != 1 {
		t.Fatalf("checked: %+v", j)
	}
	f.deploy(t, f.pr)
	if _, err := f.service.Open(ctx, f.owner, f.repo, 42, true); err != nil {
		t.Fatal(err)
	}
	if j, _ = f.service.Journey(ctx, f.workspace, f.admin); !j.Checked || !j.Published || !j.Opened {
		t.Fatalf("a team that already has a usable preview needs no wizard: %+v", j)
	}
}

func TestSetupChangesWakeTheRepositoryPage(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, f.pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, "LISTEN previa_events"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.RequestCheck(ctx, f.owner, f.repo, 42, testSHA); err != nil {
		t.Fatal(err)
	}
	// Notifications are database-wide: tests of other packages, in their own
	// schemas, may announce changes meanwhile. Wait for this repository's.
	var event struct{ Topic, Source string }
	for event.Source != "repository_setup_checks" || event.Topic != "repository:"+strconv.FormatInt(f.repo, 10) {
		notification, err := conn.WaitForNotification(ctx)
		if err != nil {
			t.Fatalf("no setup notification for the repository: %v (last %+v)", err, event)
		}
		if err := json.Unmarshal([]byte(notification.Payload), &event); err != nil {
			t.Fatalf("payload %q: %v", notification.Payload, err)
		}
	}
}
