package pullrequests

import (
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/pullrequests/dbgen"
	pages "github.com/ajaykathwatee/previa/web/pages/pullrequests"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestHeadlineSaysWhatMattersMostFirst(t *testing.T) {
	now := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	open := func(edit func(*dbgen.OwnedPRRow, *pages.PR)) (dbgen.OwnedPRRow, pages.PR) {
		row := dbgen.OwnedPRRow{Available: pgtype.Bool{Bool: true, Valid: true}, State: "open", BuildStatus: "open",
			HeadSha: "head-commit", BuildState: "Not requested", EnvironmentStatus: "Not deployed"}
		pr := pages.PR{SHA: "head-commit", Environment: "Not deployed"}
		edit(&row, &pr)
		return row, pr
	}
	for name, c := range map[string]struct {
		edit  func(*dbgen.OwnedPRRow, *pages.PR)
		title string
		tone  string
	}{
		"nothing yet": {func(*dbgen.OwnedPRRow, *pages.PR) {}, "No preview yet", "tone-neutral"},
		"access gone": {func(r *dbgen.OwnedPRRow, _ *pages.PR) { r.Available.Bool = false }, "GitHub access to this repository is unavailable", "tone-neutral"},
		"merged":      {func(r *dbgen.OwnedPRRow, _ *pages.PR) { r.State = "merged" }, "Pull request merged", "tone-merged"},
		"building": {func(r *dbgen.OwnedPRRow, _ *pages.PR) {
			r.RequestCurrent, r.BuildState, r.RequestedSha = true, "running", "head-commit"
		}, "Building head-commit", "tone-info"},
		"waiting for capacity": {func(r *dbgen.OwnedPRRow, p *pages.PR) {
			r.RequestCurrent, r.BuildState = true, "queued"
			p.Waiting = "Waiting for capacity"
		}, "Waiting for capacity", "tone-info"},
		"failed": {func(r *dbgen.OwnedPRRow, p *pages.PR) {
			r.RequestCurrent, r.BuildState = true, "failed"
			p.Failure = "The image build failed."
		}, "Build failed", "tone-danger"},
		"live": {func(r *dbgen.OwnedPRRow, p *pages.PR) {
			r.DeployedSha, r.EnvironmentStatus = "head-commit", "running"
			p.DeployedSHA, p.Environment = "head-commit", "running"
		}, "Preview is live", "tone-ok"},
		"live on an older commit": {func(_ *dbgen.OwnedPRRow, p *pages.PR) {
			p.DeployedSHA, p.Environment, p.OlderPreview = "older", "running", true
		}, "Preview live on an earlier commit", "tone-warn"},
		"conflicts": {func(r *dbgen.OwnedPRRow, _ *pages.PR) { r.BuildStatus = "conflicts" }, "Blocked by merge conflicts", "tone-warn"},
		"removed":   {func(_ *dbgen.OwnedPRRow, p *pages.PR) { p.RemovedNote = "The owner tore this preview down." }, "Preview removed", "tone-neutral"},
		"automatic deadline passed": {func(r *dbgen.OwnedPRRow, _ *pages.PR) {
			r.RequestCurrent, r.BuildState, r.RequestSource, r.RequestedSha = true, "pending", "automatic", "head-commit"
			r.AvailableAt = pgtype.Timestamptz{Time: now.Add(-time.Second), Valid: true}
		}, "Starting the build of head-commit", "tone-info"},
	} {
		row, pr := open(c.edit)
		if got := headline(pr, row, now); got.Title != c.title || got.Tone != c.tone {
			t.Errorf("%s: headline %q (%s), want %q (%s)", name, got.Title, got.Tone, c.title, c.tone)
		}
	}
}

func TestBuildLabelNamesTheActionThatWouldApply(t *testing.T) {
	for _, c := range []struct {
		current       bool
		state, source string
		want          string
	}{
		{false, "succeeded", "manual", "Build latest commit"},
		{true, "failed", "manual", "Retry build"},
		{true, "succeeded", "manual", "Rebuild preview"},
		{true, "pending", "automatic", "Build now"},
		{true, "queued", "manual", ""},
		{true, "running", "manual", ""},
		{true, "superseded", "manual", "Build latest commit"},
	} {
		row := dbgen.OwnedPRRow{RequestCurrent: c.current, BuildState: c.state, RequestSource: c.source}
		if got := buildLabel(row); got != c.want {
			t.Errorf("%v %s %s: %q, want %q", c.current, c.state, c.source, got, c.want)
		}
	}
}

// An Automatic build waiting out its quiet period counts down, says that
// another push restarts the wait, that a live preview keeps serving, and that
// Build now skips it.
func TestHeadlineCountsDownTheQuietPeriod(t *testing.T) {
	now := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	deadline := now.Add(4*time.Minute + 32*time.Second)
	row := dbgen.OwnedPRRow{Available: pgtype.Bool{Bool: true, Valid: true}, State: "open", BuildStatus: "open", HeadSha: "new-commit",
		RequestCurrent: true, BuildState: "pending", RequestSource: "automatic", RequestedSha: "new-commit",
		AvailableAt: pgtype.Timestamptz{Time: deadline, Valid: true}, DeployedSha: "old-commit", EnvironmentStatus: "running"}
	pr := pages.PR{SHA: "new-commit", DeployedSHA: "old-commit", Environment: "running", OlderPreview: true, Actions: pages.Actions{BuildNow: true}}
	got := headline(pr, row, now)
	if got.Title != "New commit new-commit builds in" || got.Until != deadline.Format(time.RFC3339) ||
		!strings.Contains(got.Detail, "another push restarts the wait") || !strings.Contains(got.Detail, "keeps serving old-commit") ||
		!strings.Contains(got.Detail, "Build now starts it at once") {
		t.Fatalf("live preview, new commit waiting: %+v", got)
	}
	row.DeployedSha, row.EnvironmentStatus, pr.DeployedSHA, pr.Environment, pr.Actions.BuildNow = "", "Not deployed", "", "Not deployed", false
	got = headline(pr, row, now)
	if got.Title != "Commit new-commit builds in" || strings.Contains(got.Detail, "keeps serving") || strings.Contains(got.Detail, "Build now") {
		t.Fatalf("first build waiting: %+v", got)
	}
	row.RequestSource = "manual"
	if got = headline(pr, row, now); got.Until != "" || got.Title != "Starting the build of new-commit" {
		t.Fatalf("a manual request has no quiet period: %+v", got)
	}
}
