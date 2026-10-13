package reporting

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/githubapp"
	"github.com/google/go-github/v91/github"
)

const liveCheckName = "Previa preview (live proof)"

// TestRealReportingOnADisposablePR drives the reporter against a real PR on
// GitHub: queued, building, ready, a failed same-commit rebuild, a deleted
// comment, a lost comment ID and removal. It writes one living comment and
// check runs named liveCheckName to that PR only, then deletes the comment and
// completes any check run it left open (GitHub cannot delete check runs).
// Only comment and check run IDs are logged.
func TestRealReportingOnADisposablePR(t *testing.T) {
	if os.Getenv("TEST_REPORTING") != "1" {
		t.Skip("set TEST_REPORTING=1 (make test-reporting) with an authorized disposable PR")
	}
	installationID, repoID, number := liveID(t, "TEST_GITHUB_INSTALLATION_ID"), liveID(t, "TEST_GITHUB_REPO_ID"), liveID(t, "TEST_GITHUB_PR_NUMBER")
	app, err := githubapp.NewApp(liveID(t, "GITHUB_APP_ID"), os.Getenv("GITHUB_APP_PRIVATE_KEY_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	identity, err := app.App(ctx)
	if err != nil {
		t.Fatal("read the authenticated App (details omitted)")
	}
	client, err := app.InstallationClient(installationID)
	if err != nil {
		t.Fatal("mint an installation client (details omitted)")
	}
	api, err := NewGitHubAPI(app, identity.GetID(), identity.GetSlug())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := api.Repository(ctx, installationID, repoID)
	if err != nil {
		t.Fatalf("read the repository: %v", err)
	}
	pr, response, err := client.PullRequests.Get(ctx, repo.Owner, repo.Name, int(number))
	if err != nil {
		t.Fatalf("read the PR: %v", classify(response, err))
	}
	if pr.GetState() != "open" || len(pr.GetHead().GetSHA()) != 40 {
		t.Fatal("the proof needs an open PR")
	}

	f := &reportFixture{pool: migratedPool(t), headSHA: pr.GetHead().GetSHA()}
	f.seed(t, installationID, repoID, int32(number))
	marker := fmt.Sprintf("<!-- previa:live-proof %d -->", time.Now().UnixNano())
	liveProof(t, &live{f: f, client: client, api: liveProofAPI{GitHub: api, marker: marker}, marker: marker,
		installationID: installationID, repo: repo, number: number, appID: identity.GetID()})
}

// TestLiveProofRehearsal runs the live proof's script against the fake
// GitHub, so a mistake in it shows up before it touches a real PR.
func TestLiveProofRehearsal(t *testing.T) {
	fake := newFakeGitHub(t, fixtureRepoID)
	f := &reportFixture{pool: migratedPool(t), github: fake, headSHA: headB}
	f.seed(t, 900, fixtureRepoID, 7)
	client, err := fake.InstallationClient(900)
	if err != nil {
		t.Fatal(err)
	}
	api := fake.api(t)
	originalBody := Marker + "\nExisting live preview"
	original, err := api.CreateComment(t.Context(), 900, Repo{Owner: "octo", Name: "app"}, 7, originalBody)
	if err != nil {
		t.Fatal(err)
	}
	marker := "<!-- previa:live-proof rehearsal -->"
	isolated := liveProofAPI{GitHub: api, marker: marker}
	if err := isolated.UpdateComment(t.Context(), 900, Repo{Owner: "octo", Name: "app"}, original, originalBody); err == nil {
		t.Fatal("live proof could update the real preview comment")
	}
	// Registered first, so it runs after the proof's own cleanup.
	t.Cleanup(func() {
		if comments := fake.appComments(7); len(comments) != 1 || comments[0].ID != original || comments[0].Body != originalBody {
			t.Errorf("proof cleanup changed the real preview comment or left temporary comments: %+v", comments)
		}
		for _, c := range fake.checksFor(headB) {
			if c.Status != "completed" {
				t.Errorf("cleanup left check run %d %s", c.ID, c.Status)
			}
		}
	})
	liveProof(t, &live{f: f, client: client, api: isolated, marker: marker,
		installationID: 900, repo: Repo{Owner: "octo", Name: "app"}, number: 7, appID: testAppID})
}

func liveProof(t *testing.T, l *live) {
	t.Helper()
	f := l.f
	if _, found, err := l.api.FindComment(t.Context(), l.installationID, l.repo, int32(l.number), l.commentMarker(), commentPages); err != nil || found {
		t.Fatalf("the proof marker must be unused (found %v, %v)", found, err)
	}
	var err error
	f.reporter, err = NewReporter(f.pool, l.api, Settings{CheckName: liveCheckName}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { liveCleanup(t, l) })

	f.intent(t, "queued", 1)
	l.report(t, "queued")
	first := l.commentID(t)
	t.Logf("created living comment %d", first)
	run1 := l.check(t, 1, "queued", "")
	t.Logf("created check run %d (revision 1)", run1)

	f.intent(t, "running", 1)
	l.report(t, "building")
	l.check(t, 1, "in_progress", "")

	f.intent(t, "succeeded", 1)
	f.environment(t, "running", f.headSHA, 1, "")
	l.report(t, "ready")
	l.body(t, "Previa preview: Ready")
	l.check(t, 1, "completed", "success")

	// A same-commit rebuild gets its own check run and leaves the first alone.
	f.intent(t, "queued", 2)
	l.report(t, "rebuild queued")
	run2 := l.check(t, 2, "queued", "")
	t.Logf("created check run %d (revision 2)", run2)
	if run2 == run1 {
		t.Fatal("a rebuild reused the earlier check run")
	}
	l.checkByID(t, run1, "completed", "success")
	f.intent(t, "failed", 2)
	f.environment(t, "failed", f.headSHA, 1, "unhealthy")
	l.report(t, "rebuild failed")
	l.check(t, 2, "completed", "failure")
	l.body(t, "keeps serving")

	// Someone deletes the living comment: the next change recreates it.
	if response, err := l.client.Issues.DeleteComment(t.Context(), l.repo.Owner, l.repo.Name, first); err != nil {
		t.Fatalf("delete the comment: %v", classify(response, err))
	}
	f.intent(t, "queued", 3)
	l.report(t, "after deletion")
	second := l.commentID(t)
	if second == first {
		t.Fatal("the deleted comment was not replaced")
	}
	t.Logf("recreated living comment %d after deletion of %d", second, first)
	run3 := l.check(t, 3, "queued", "")
	t.Logf("created check run %d (revision 3)", run3)

	// Create succeeded but its ID was never saved: discovery adopts it.
	f.exec(t, "UPDATE pull_requests SET github_comment_id = NULL WHERE id=$1", f.prID)
	f.exec(t, "UPDATE pull_request_reports SET comment_digest = NULL WHERE pull_request_id=$1", f.prID)
	f.intent(t, "running", 3)
	l.report(t, "after a lost comment ID")
	if adopted := l.commentID(t); adopted != second {
		t.Fatalf("discovery created comment %d instead of adopting %d", adopted, second)
	}
	l.check(t, 3, "in_progress", "")

	// Removal: the comment says so and the unfinished check run is stopped.
	f.exec(t, "UPDATE pull_requests SET state='closed', generation=6 WHERE id=$1", f.prID)
	f.intent(t, "superseded", 3)
	f.environment(t, "torn_down", "", 1, "")
	l.report(t, "removed")
	l.body(t, "Previa preview: Preview removed")
	l.checkByID(t, run3, "completed", "cancelled")
	l.checkByID(t, run2, "completed", "failure")
	if comments := l.markerComments(t); len(comments) != 1 {
		t.Fatalf("the PR has %d Previa comments", len(comments))
	}
	t.Log("live proof complete: one living comment, one check run per revision, nothing duplicated")
}

type live struct {
	f              *reportFixture
	client         *github.Client
	api            GitHub
	marker         string
	installationID int64
	repo           Repo
	number         int64
	appID          int64
}

func (l *live) commentMarker() string {
	if l.marker != "" {
		return l.marker
	}
	return Marker
}

func (l *live) report(t *testing.T, stage string) {
	t.Helper()
	l.f.want(t)
	if err := l.f.reporter.Sync(t.Context(), l.f.prID, "live-proof"); err != nil {
		t.Fatalf("%s: %v", stage, err)
	}
	if wanted, reported, code := l.f.versions(t); reported != wanted || code != "" {
		t.Fatalf("%s: reported %d of %d (%s)", stage, reported, wanted, code)
	}
}

func (l *live) commentID(t *testing.T) int64 {
	t.Helper()
	var id int64
	mustScan(t, l.f.pool.QueryRow(t.Context(), "SELECT github_comment_id FROM pull_requests WHERE id=$1", l.f.prID), &id)
	return id
}

func (l *live) body(t *testing.T, want string) {
	t.Helper()
	comment, response, err := l.client.Issues.GetComment(t.Context(), l.repo.Owner, l.repo.Name, l.commentID(t))
	if err != nil {
		t.Fatalf("read the comment: %v", classify(response, err))
	}
	if !strings.Contains(comment.GetBody(), want) || !strings.HasPrefix(comment.GetBody(), l.commentMarker()) {
		t.Fatalf("the comment lacks %q", want)
	}
}

// check verifies the stored check run is the one for revision and reads it
// back from GitHub.
func (l *live) check(t *testing.T, revision int64, status, conclusion string) int64 {
	t.Helper()
	var id, storedRevision int64
	var sha string
	mustScan(t, l.f.pool.QueryRow(t.Context(), "SELECT check_run_id, check_head_sha, check_revision FROM pull_request_reports WHERE pull_request_id=$1", l.f.prID), &id, &sha, &storedRevision)
	if sha != l.f.headSHA || storedRevision != revision {
		t.Fatalf("stored check identity is revision %d", storedRevision)
	}
	run := l.checkByID(t, id, status, conclusion)
	if run.GetExternalID() != ExternalID(l.f.prID, &Check{HeadSHA: l.f.headSHA, Revision: revision}) || run.GetName() != liveCheckName {
		t.Fatalf("check run %d has the wrong identity", id)
	}
	return id
}

func (l *live) checkByID(t *testing.T, id int64, status, conclusion string) *github.CheckRun {
	t.Helper()
	run, response, err := l.client.Checks.GetCheckRun(t.Context(), l.repo.Owner, l.repo.Name, id)
	if err != nil {
		t.Fatalf("read check run %d: %v", id, classify(response, err))
	}
	if run.GetStatus() != status || run.GetConclusion() != conclusion {
		t.Fatalf("check run %d is %s/%s, want %s/%s", id, run.GetStatus(), run.GetConclusion(), status, conclusion)
	}
	return run
}

func (l *live) markerComments(t *testing.T) []int64 {
	t.Helper()
	ids, err := liveMarkerComments(t.Context(), l.client, l.repo, l.number, l.appID, l.commentMarker())
	if err != nil {
		t.Fatalf("list comments: %v", err)
	}
	return ids
}

// liveMarkerComments lists this App's Previa comments on the PR.
func liveMarkerComments(ctx context.Context, client *github.Client, repo Repo, number int64, appID int64, marker string) ([]int64, error) {
	var ids []int64
	options := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100, Page: 1}}
	for range commentPages {
		comments, response, err := client.Issues.ListComments(ctx, repo.Owner, repo.Name, int(number), options)
		if err != nil {
			return nil, classify(response, err)
		}
		for _, c := range comments {
			if c.GetPerformedViaGithubApp().GetID() == appID && strings.HasPrefix(c.GetBody(), marker+"\n") {
				ids = append(ids, c.GetID())
			}
		}
		if response.NextPage == 0 {
			break
		}
		options.Page = response.NextPage
	}
	return ids, nil
}

// liveCleanup deletes the proof's comment and completes its open check runs,
// even when the proof failed part-way.
func liveCleanup(t *testing.T, l *live) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ids, err := liveMarkerComments(ctx, l.client, l.repo, l.number, l.appID, l.commentMarker())
	if err != nil {
		t.Errorf("cleanup: list comments: %v; delete the Previa comment on the PR by hand", err)
	}
	for _, id := range ids {
		if response, err := l.client.Issues.DeleteComment(ctx, l.repo.Owner, l.repo.Name, id); err != nil {
			t.Errorf("cleanup: delete comment %d: %v", id, classify(response, err))
		} else {
			t.Logf("cleanup: deleted comment %d", id)
		}
	}
	name, filter := liveCheckName, "all"
	result, response, err := l.client.Checks.ListCheckRunsForRef(ctx, l.repo.Owner, l.repo.Name, l.f.headSHA, &github.ListCheckRunsOptions{CheckName: &name, AppID: &l.appID, Filter: &filter, ListOptions: github.ListOptions{PerPage: 100}})
	if err != nil {
		t.Errorf("cleanup: list check runs: %v", classify(response, err))
		return
	}
	for _, run := range result.CheckRuns {
		if run.GetStatus() == "completed" {
			continue
		}
		stop := CheckRun{Name: liveCheckName, Status: "completed", Conclusion: "cancelled", Title: "Stopped", Summary: "The live proof ended.", ExternalID: run.GetExternalID()}
		if err := l.api.UpdateCheckRun(ctx, l.installationID, l.repo, run.GetID(), stop); err != nil {
			t.Errorf("cleanup: complete check run %d: %v", run.GetID(), err)
		} else {
			t.Logf("cleanup: completed check run %d", run.GetID())
		}
	}
	t.Logf("cleanup: %d completed check runs named %q remain on the commit (GitHub cannot delete check runs)", len(result.CheckRuns), liveCheckName)
}

func liveID(t *testing.T, name string) int64 {
	t.Helper()
	id, err := strconv.ParseInt(os.Getenv(name), 10, 64)
	if err != nil || id <= 0 {
		t.Fatalf("set %s to a positive integer", name)
	}
	return id
}
