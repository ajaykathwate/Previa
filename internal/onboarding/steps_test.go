package onboarding

import (
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/onboarding/dbgen"
	"github.com/ajaykathwatee/previa/internal/platform/workers"
	pages "github.com/ajaykathwatee/previa/web/pages/onboarding"
	"github.com/jackc/pgx/v5/pgtype"
)

var stepsNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func at(d time.Duration) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: stepsNow.Add(d), Valid: true}
}

func baseView() view {
	return view{now: stepsNow, viewer: 1, discover: true, secretsOn: true, previewDomain: "preview.test", fingerprint: "fp",
		repo: dbgen.SetupRepositoryRow{ID: 5, FullName: "acme/api", Accessible: true, InstallationStatus: "active", CanManage: true, WorkspaceID: 9, BuildMode: "manual"},
		caps: workers.Capabilities{Execution: true, Discovery: true}}
}

func withPR(v view, change func(*dbgen.SetupPullRequestRow)) view {
	row := dbgen.SetupPullRequestRow{ID: 11, Number: 42, Title: "Add discount codes", HeadBranch: "feature/codes", HeadSha: testSHA, Generation: 3, State: "open", BuildStatus: "open", RequestRevision: 4}
	if change != nil {
		change(&row)
	}
	v.pr = &row
	return v
}

// checked records a completed check of the head with result r.
func checked(t *testing.T, r Result) func(*dbgen.SetupPullRequestRow) {
	data, err := encodeResult(r.finish())
	if err != nil {
		t.Fatal(err)
	}
	return func(row *dbgen.SetupPullRequestRow) {
		row.CheckSha, row.CheckState, row.ResultSha, row.Result = row.HeadSha, "complete", row.HeadSha, data
		row.CheckRequestedAt, row.CheckedAt, row.ExpiresAt, row.CheckPlatform = at(-time.Minute), at(-time.Minute), at(14*time.Minute), "fp"
	}
}

var ready = Result{Config: ConfigFound, Context: ".", Dockerfile: "Dockerfile", ContextState: PathFound, DockerfileState: PathFound, Port: 3000, HealthCheckPath: "/healthz",
	ResourceTier: "standard", Services: []ManagedService{{Kind: "postgres", Version: "17"}}, Setup: []string{"bin/rails", "db:migrate"}, EnvNames: []string{"RAILS_ENV"}}

func derived(v view) pages.Page {
	var p pages.Page
	v.derive(&p)
	return p
}

func step(t *testing.T, p pages.Page, id string) pages.Step {
	t.Helper()
	for _, s := range p.Steps {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no %s step in %+v", id, p.Steps)
	return pages.Step{}
}

func states(p pages.Page) string {
	var out []string
	for _, s := range p.Steps {
		out = append(out, s.State)
	}
	return strings.Join(out, " ")
}

func TestWithoutAnOpenPullRequestOnlyTheConnectionCounts(t *testing.T) {
	p := derived(baseView())
	if states(p) != "done waiting waiting waiting waiting" || p.Selected != nil || p.Check != nil || p.Build != nil {
		t.Fatalf("no pull request: %s %+v", states(p), p)
	}
}

func TestConnectionProblemsNameTheFixForEachRole(t *testing.T) {
	v := baseView()
	v.repo.Accessible = false
	p := derived(withPR(v, nil))
	connection := step(t, p, stepConnection)
	if connection.State != pages.StateProblem || len(connection.Links) != 1 || connection.Links[0].Href != "/repositories?access=unavailable" {
		t.Fatalf("a manager restores access: %+v", connection)
	}
	if states(p) != "problem waiting waiting waiting waiting" || p.Check != nil {
		t.Fatalf("nothing else applies until access returns: %s", states(p))
	}
	v.repo.CanManage = false
	v.managers = []pages.Person{{Login: "alice", Name: "Alice", Role: "owner"}}
	connection = step(t, derived(withPR(v, nil)), stepConnection)
	if len(connection.Links) != 0 || !strings.Contains(connection.Items[0].Detail, "Alice (@alice)") {
		t.Fatalf("a Developer learns whom to ask: %+v", connection)
	}
	v = baseView()
	v.discover = false
	if connection = step(t, derived(v), stepConnection); connection.State != pages.StateProblem || !strings.Contains(connection.Items[0].Detail, "GITHUB_APP_ID") {
		t.Fatalf("development names the missing setting: %+v", connection)
	}
	v.production = true
	if connection = step(t, derived(v), stepConnection); len(connection.Items) != 0 || strings.Contains(connection.Summary, "GITHUB") {
		t.Fatalf("hosted pages never show operator settings: %+v", connection)
	}
}

func TestAnUncheckedCommitOffersTheCheck(t *testing.T) {
	p := derived(withPR(baseView(), nil))
	configuration := step(t, p, stepConfiguration)
	if configuration.State != pages.StateCurrent || configuration.Form != "check" || p.Check == nil || !p.Check.Primary || p.Check.Label != "Check setup" {
		t.Fatalf("unchecked: %+v %+v", configuration, p.Check)
	}
	if states(p) != "done current waiting waiting waiting" || p.Build != nil {
		t.Fatalf("later steps wait: %s", states(p))
	}
	if p.Selected == nil || p.Selected.SHA != testSHA {
		t.Fatalf("the selected commit is shown: %+v", p.Selected)
	}
	old := withPR(baseView(), checked(t, ready))
	old.pr.HeadSha = strings.Repeat("b", 40)
	if configuration = step(t, derived(old), stepConfiguration); configuration.State != pages.StateCurrent || !strings.Contains(configuration.Items[0].Title, "earlier commit") {
		t.Fatalf("a result for an earlier commit is not this commit's: %+v", configuration)
	}
}

func TestACheckOnItsWayAndOneThatNeverFinished(t *testing.T) {
	inFlight := func(age time.Duration) func(*dbgen.SetupPullRequestRow) {
		return func(row *dbgen.SetupPullRequestRow) {
			row.CheckSha, row.CheckState, row.CheckRequestedAt = row.HeadSha, "queued", at(-age)
		}
	}
	p := derived(withPR(baseView(), inFlight(30*time.Second)))
	configuration := step(t, p, stepConfiguration)
	if configuration.State != pages.StateWorking || configuration.Form != "" || p.RefreshAt != stepsNow.Add(-30*time.Second+CheckStaleAfter).Format(time.RFC3339) {
		t.Fatalf("working, refreshed when it would turn stale: %+v %s", configuration, p.RefreshAt)
	}
	p = derived(withPR(baseView(), inFlight(3*time.Minute)))
	if configuration = step(t, p, stepConfiguration); configuration.State != pages.StateProblem || p.Check == nil || p.Check.Label != "Check again" {
		t.Fatalf("a stale check can be asked for again: %+v", configuration)
	}
	v := baseView()
	v.caps.Discovery = false
	configuration = step(t, derived(withPR(v, inFlight(time.Minute))), stepConfiguration)
	if !strings.Contains(configuration.Items[0].Title, "No worker") || !strings.Contains(configuration.Items[0].Detail, "make run-worker") {
		t.Fatalf("development says what runs checks: %+v", configuration.Items)
	}
	v.production = true
	configuration = step(t, derived(withPR(v, inFlight(time.Minute))), stepConfiguration)
	if strings.Contains(configuration.Items[0].Detail, "make") || configuration.Items[0].Title != "Setup checks are delayed." {
		t.Fatalf("hosted wording: %+v", configuration.Items)
	}
}

func TestAFailedAttemptIsNotASuccessButKeepsTheLastResult(t *testing.T) {
	p := derived(withPR(baseView(), func(row *dbgen.SetupPullRequestRow) {
		checked(t, ready)(row)
		row.CheckState, row.CheckFailure, row.CheckFinishedAt = "failed", FailRateLimited, at(0)
	}))
	configuration := step(t, p, stepConfiguration)
	if configuration.State != pages.StateProblem || p.Check.Label != "Try again" || len(configuration.Items) != 2 ||
		!strings.Contains(configuration.Items[0].Title, "slow down") || !strings.Contains(configuration.Items[1].Title, "it passed") {
		t.Fatalf("rate limited: %+v", configuration)
	}
	if step(t, p, stepRequirements).State != pages.StateWaiting || p.Build != nil {
		t.Fatal("a failed attempt never unlocks building")
	}
}

func TestAPassingCheckLeadsToBuildingWithTheAppsNeedsBeside(t *testing.T) {
	v := withPR(baseView(), checked(t, ready))
	p := derived(v)
	if states(p) != "done done current waiting waiting" {
		t.Fatalf("states: %s", states(p))
	}
	requirements := step(t, p, stepRequirements)
	if requirements.Form != "build" || p.Build == nil || p.Build.Action != "build" || p.Build.ExpectedRevision != 4 || p.Build.RequestKey == "" || requirements.Blocked != "" {
		t.Fatalf("build offered: %+v %+v", requirements, p.Build)
	}
	if !strings.Contains(requirements.Summary, "beside this list") || strings.Contains(requirements.Summary, "3000") {
		t.Fatalf("the summary points at the facts instead of repeating them: %q", requirements.Summary)
	}
	if p.Check != nil || step(t, p, stepConfiguration).Form != "" || step(t, p, stepConfiguration).Summary != "Valid .previewrc.yaml and Dockerfile at "+pages.Short(testSHA)+"." {
		t.Fatalf("a passing check needs no repeat: %+v", step(t, p, stepConfiguration))
	}
	if p.Facts.SHA != testSHA || len(p.Facts.Rows) < 5 || p.Facts.Rows[0].Label != "Port" {
		t.Fatalf("facts: %+v", p.Facts)
	}
	hasInfo := false
	for _, item := range requirements.Items {
		hasInfo = hasInfo || strings.Contains(item.Title, "No secrets are stored")
	}
	if !hasInfo {
		t.Fatalf("a manager without secrets is reminded: %+v", requirements.Items)
	}
	v.pr.IsFork = true
	if requirements = step(t, derived(v), stepRequirements); requirements.Items[0].Tone != "warn" || !strings.Contains(requirements.Items[0].Title, "fork") {
		t.Fatalf("forks are told what they lack: %+v", requirements.Items)
	}
	v.repo.CanManage = false
	for _, item := range step(t, derived(v), stepRequirements).Items {
		if strings.Contains(item.Title, "No secrets are stored") {
			t.Fatal("Developers are not told about secrets they cannot see")
		}
	}
}

func TestAChangedPlatformAsksForAnotherCheck(t *testing.T) {
	v := withPR(baseView(), checked(t, ready))
	v.fingerprint = "new catalog"
	p := derived(v)
	if configuration := step(t, p, stepConfiguration); configuration.State != pages.StateCurrent || p.Check.Label != "Check again" {
		t.Fatalf("stale platform: %+v", configuration)
	}
}

func TestProblemsNameTheFix(t *testing.T) {
	p := derived(withPR(baseView(), checked(t, Result{Config: ConfigMissing, RootDockerfile: true})))
	configuration := step(t, p, stepConfiguration)
	if configuration.State != pages.StateProblem || len(configuration.Links) != 1 || configuration.Links[0].Href != "/starter-kits?stack=custom" || !configuration.Links[0].Primary {
		t.Fatalf("missing config with a Dockerfile: %+v", configuration)
	}
	if !strings.Contains(configuration.Summary, "check again") || p.Check.Label != "Check again" || p.Check.Primary {
		t.Fatalf("the fix comes first; checking again is secondary: %+v %+v", configuration, p.Check)
	}
	p = derived(withPR(baseView(), checked(t, Result{Config: ConfigInvalid, Issues: []Issue{{Field: "services.postgres", Code: "unknown_service_version", Message: "postgres 15 is not offered", Line: 3}}})))
	configuration = step(t, p, stepConfiguration)
	if configuration.Items[0].Field != "services.postgres" || configuration.Items[0].Line != 3 || configuration.Links[0].Href != "/starter-kits" {
		t.Fatalf("invalid config: %+v", configuration)
	}
	missingDockerfile := ready
	missingDockerfile.DockerfileState = PathMissing
	configuration = step(t, derived(withPR(baseView(), checked(t, missingDockerfile))), stepConfiguration)
	if !strings.Contains(configuration.Items[0].Title, "No Dockerfile") || configuration.Links[0].Label != "Get a Dockerfile from a Starter kit" {
		t.Fatalf("missing Dockerfile: %+v", configuration)
	}
	if p.Build != nil || step(t, p, stepRequirements).State != pages.StateWaiting {
		t.Fatal("a commit with problems cannot be built from the checklist")
	}
}

func TestBuildingIsShownDisabledWithTheReason(t *testing.T) {
	v := withPR(baseView(), checked(t, ready))
	v.pr.BuildStatus = "conflicts"
	if requirements := step(t, derived(v), stepRequirements); !strings.Contains(requirements.Blocked, "merge conflicts") {
		t.Fatalf("conflicts: %q", requirements.Blocked)
	}
	v = withPR(baseView(), checked(t, ready))
	v.caps = workers.Capabilities{Discovery: true}
	if requirements := step(t, derived(v), stepRequirements); !strings.Contains(requirements.Blocked, "PREVIEW_BASE_DOMAIN") {
		t.Fatalf("development: %q", requirements.Blocked)
	}
	v.production = true
	if requirements := step(t, derived(v), stepRequirements); requirements.Blocked != "Build capacity is temporarily unavailable. Try again in a few minutes." {
		t.Fatalf("hosted: %q", requirements.Blocked)
	}
}

func TestThePreviewStepFollowsTheBuild(t *testing.T) {
	build := func(state string) view {
		return withPR(baseView(), func(row *dbgen.SetupPullRequestRow) {
			checked(t, ready)(row)
			row.RequestCurrent, row.BuildState, row.RequestedSha, row.BuildStartedAt = true, state, row.HeadSha, at(-time.Minute)
		})
	}
	p := derived(build("queued"))
	if states(p) != "done done done working waiting" || step(t, p, stepRequirements).Form != "" {
		t.Fatalf("queued: %s", states(p))
	}
	preview := step(t, derived(build("running")), stepPreview)
	if preview.State != pages.StateWorking || preview.Links[0].Href != "/repositories/5/pull-requests/42/logs" || !preview.Links[0].Primary {
		t.Fatalf("running: %+v", preview)
	}
	failed := build("failed")
	failed.pr.ExecutionErrorCode = "build_failed"
	p = derived(failed)
	preview = step(t, p, stepPreview)
	if preview.State != pages.StateProblem || preview.Failure != "build_failed" || preview.Form != "build" || p.Build.Action != "retry" || !strings.HasPrefix(preview.Headline(), "Your Dockerfile build failed") {
		t.Fatalf("failed: %+v %+v", preview, p.Build)
	}
	live := build("succeeded")
	live.pr.DeployedSha, live.pr.EnvironmentStatus, live.pr.Hostname, live.pr.PublishedAt = live.pr.HeadSha, "running", "pr-42-x", at(-time.Minute)
	p = derived(live)
	opened := step(t, p, stepOpened)
	if states(p) != "done done done done current" || opened.Links[0].Href != "/repositories/5/pull-requests/42/open" || !opened.Links[0].External ||
		!strings.Contains(step(t, p, stepPreview).Summary, "https://pr-42-x.preview.test") {
		t.Fatalf("live: %s %+v", states(p), opened)
	}
	if p.Complete() || len(p.Next) != 0 {
		t.Fatal("not done until it is opened")
	}
}

func TestOpeningThePreviewFinishesSetup(t *testing.T) {
	v := withPR(baseView(), func(row *dbgen.SetupPullRequestRow) {
		row.RequestCurrent, row.BuildState, row.RequestedSha = true, "succeeded", row.HeadSha
		row.DeployedSha, row.EnvironmentStatus, row.Hostname = row.HeadSha, "running", "pr-42-x"
	})
	v.opened = &dbgen.LatestOpenRow{UserID: 1, DisplayName: "Owner", OpenedAt: at(-time.Minute)}
	p := derived(v)
	if !p.Complete() || !strings.Contains(step(t, p, stepOpened).Summary, "by you") || len(p.Next) != 2 || p.Next[1].Href != "/workspaces/9/members" {
		t.Fatalf("opened by the viewer: %s %+v", states(p), p.Next)
	}
	if step(t, p, stepConfiguration).State != pages.StateDone {
		t.Fatal("a commit that built needs no separate check")
	}
	v.opened.UserID, v.opened.DisplayName = 2, "Ada"
	v.repo.CanManage = false
	p = derived(v)
	if !strings.Contains(step(t, p, stepOpened).Summary, "by Ada") || len(p.Next) != 1 {
		t.Fatalf("opened by a teammate; Developers are not offered invitations: %+v", p.Next)
	}
	v.pr.HeadSha, v.pr.RequestCurrent = strings.Repeat("c", 40), false
	p = derived(v)
	if p.Complete() || states(p) != "done current waiting waiting waiting" || !strings.Contains(step(t, p, stepPreview).Summary, "earlier commit") {
		t.Fatalf("a new commit starts the path again, and says an earlier one is live: %s %q", states(p), step(t, p, stepPreview).Summary)
	}
}
