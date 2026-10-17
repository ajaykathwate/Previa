package sampledata

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestTurningTestDataOnBuildsTheFirstBaseline(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.enable(t)
	refreshes := f.refreshes(t)
	if len(refreshes) != 1 || refreshes[0].Trigger != TriggerEnabled || refreshes[0].State != StatePending || f.jobs.count() != 1 {
		t.Fatalf("turning Test data on requests one refresh and its job: %+v, %d jobs", refreshes, f.jobs.count())
	}
	f.builder.result = dumps(strings.Repeat("a", 32))
	if _, pending, err := f.runner.Run(ctx, f.repo); err != nil || pending {
		t.Fatalf("run: pending %v, %v", pending, err)
	}
	built := f.builder.built()
	if len(built) != 1 || built[0].Branch != "main" || built[0].CommitSHA != shaOne || built[0].GitHubRepoID != 100 || built[0].WorkspaceID != f.workspace {
		t.Fatalf("the default branch head is built: %+v", built)
	}
	current := f.current(t)
	if len(current) != 1 || current[0].Kind != "postgres" || current[0].Version != "17" || current[0].Branch != "main" || current[0].CommitSHA != shaOne || current[0].Sequence != 1 {
		t.Fatalf("the captured dump becomes the current baseline: %+v", current)
	}
	if r := f.refreshes(t); r[0].State != StateSucceeded {
		t.Fatalf("the refresh succeeded: %+v", r)
	}
	settings, err := f.store.Settings(ctx, f.repo)
	if err != nil || settings.CheckedBranch != "main" || settings.CheckedSHA != shaOne {
		t.Fatalf("the refresh records the branch head it saw: %+v %v", settings, err)
	}
	if kinds := f.activity.kinds(); !slices.Equal(kinds, []string{KindSettingsUpdated, KindBaselineRefreshed}) {
		t.Fatalf("activity: %v", kinds)
	}
	if len(f.admission.usage) != 1 {
		t.Fatalf("the build counts toward the workspace's build time: %v", f.admission.usage)
	}
}

func TestRefreshRequestsCoalesceAndTheNewestResultWins(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.enable(t)
	f.builder.result = dumps(strings.Repeat("a", 32))
	if _, _, err := f.runner.Run(ctx, f.repo); err != nil {
		t.Fatal(err)
	}
	// Two merges while nothing runs collapse into one waiting refresh.
	for range 2 {
		if err := f.store.Request(ctx, f.repo, TriggerMerged, 0); err != nil {
			t.Fatal(err)
		}
	}
	if r := f.refreshes(t); len(r) != 2 || r[1].State != StatePending {
		t.Fatalf("requests collapse: %+v", r)
	}
	// A request while that one runs becomes the single follow-up.
	f.branches.set("main", shaTwo)
	f.builder.result = dumps(strings.Repeat("b", 32))
	f.builder.during = func(BuildJob) {
		if err := f.store.Request(ctx, f.repo, TriggerButton, f.dev); err != nil {
			t.Error(err)
		}
		if err := f.store.Request(ctx, f.repo, TriggerButton, f.dev); err != nil {
			t.Error(err)
		}
	}
	// Spaced after the first automatic start: run the waiting merge refresh
	// as if its time came.
	f.exec(t, "UPDATE baseline_refreshes SET started_at = started_at - interval '1 hour' WHERE repo_id=$1", f.repo)
	after, pending, err := f.runner.Run(ctx, f.repo)
	if err != nil || !pending || after > time.Second {
		t.Fatalf("the follow-up waits to run next, at once: pending %v after %v, %v", pending, after, err)
	}
	if r := f.refreshes(t); len(r) != 3 || r[1].State != StateSucceeded || r[2].State != StatePending || r[2].Trigger != TriggerButton {
		t.Fatalf("one follow-up: %+v", r)
	}
	f.builder.during = nil
	f.branches.set("main", shaThree)
	f.builder.result = dumps(strings.Repeat("c", 32))
	if _, pending, err := f.runner.Run(ctx, f.repo); err != nil || pending {
		t.Fatalf("follow-up: %v %v", pending, err)
	}
	if c := f.current(t); len(c) != 1 || c[0].CommitSHA != shaThree || c[0].Sequence != 3 {
		t.Fatalf("the newest refresh is current: %+v", c)
	}
	// A refresh that finishes after a newer baseline became current is
	// discarded with its file (the sequence fence).
	if err := f.store.Request(ctx, f.repo, TriggerButton, f.dev); err != nil {
		t.Fatal(err)
	}
	f.exec(t, "UPDATE repository_baselines SET refresh_sequence = 99 WHERE repo_id=$1 AND status='current'", f.repo)
	f.builder.result = dumps(strings.Repeat("d", 32))
	if _, _, err := f.runner.Run(ctx, f.repo); err != nil {
		t.Fatal(err)
	}
	if c := f.current(t); len(c) != 1 || c[0].CommitSHA != shaThree {
		t.Fatalf("an older result never replaces a newer baseline: %+v", c)
	}
	if r := f.refreshes(t); r[3].State != StateCancelled || !slices.Contains(f.files.removed(), strings.Repeat("d", 32)) {
		t.Fatalf("the late result is discarded with its file: %+v %v", r, f.files.removed())
	}
	var retired int
	if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM repository_baselines WHERE repo_id=$1 AND status='retired'", f.repo).Scan(&retired); err != nil || retired != 2 {
		t.Fatalf("replaced baselines are retired: %d %v", retired, err)
	}
}

func TestAFailedRefreshKeepsThePreviousBaseline(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.enable(t)
	f.builder.result = dumps(strings.Repeat("a", 32))
	if _, _, err := f.runner.Run(ctx, f.repo); err != nil {
		t.Fatal(err)
	}
	f.builder.err = &BuildError{Code: FailSetup, Detail: "The setup command exited with code 1."}
	if err := f.service.Refresh(ctx, f.dev, f.repo); err != nil {
		t.Fatal(err)
	}
	if _, pending, err := f.runner.Run(ctx, f.repo); err != nil || pending {
		t.Fatalf("run: %v %v", pending, err)
	}
	r := f.refreshes(t)
	if r[1].State != StateFailed || r[1].FailureCode != FailSetup {
		t.Fatalf("the refresh failed with its category: %+v", r)
	}
	if c := f.current(t); len(c) != 1 || c[0].Sequence != 1 {
		t.Fatalf("the previous baseline stays current: %+v", c)
	}
	if kinds := f.activity.kinds(); kinds[len(kinds)-1] != KindBaselineFailed {
		t.Fatalf("activity: %v", kinds)
	}
	// A chosen branch that does not exist fails without building.
	settings, err := f.store.Settings(ctx, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Save(ctx, f.owner, f.repo, Form{Revision: settings.Revision, Enabled: true, KeepData: true, UseBaseline: true, AutoRefresh: true, Branch: "develop"}); err != nil {
		t.Fatal(err)
	}
	builds := len(f.builder.built())
	if _, _, err := f.runner.Run(ctx, f.repo); err != nil {
		t.Fatal(err)
	}
	var detail string
	if err := f.pool.QueryRow(ctx, "SELECT failure_detail FROM baseline_refreshes WHERE repo_id=$1 AND sequence=3", f.repo).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if r := f.refreshes(t); r[2].Trigger != TriggerBranch || r[2].FailureCode != FailBranchNotFound || !strings.Contains(detail, "develop") || len(f.builder.built()) != builds {
		t.Fatalf("a missing branch fails before building: %+v %q", r, detail)
	}
	if c := f.current(t); len(c) != 1 || c[0].Sequence != 1 {
		t.Fatalf("the previous baseline stays current: %+v", c)
	}
}

func TestRefreshesWaitForCapacityAndRetryWorkThatCouldNotRun(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.enable(t)
	f.admission.reason = FailCapacity
	after, pending, err := f.runner.Run(ctx, f.repo)
	if err != nil || !pending || after < time.Minute || len(f.builder.built()) != 0 {
		t.Fatalf("a refresh without a build slot waits: %v %v %v", after, pending, err)
	}
	if r := f.refreshes(t); r[0].State != StatePending || r[0].WaitingReason != FailCapacity || r[0].Attempts != 0 {
		t.Fatalf("it keeps its place, saying why: %+v", r)
	}
	tomorrow := time.Now().UTC().Add(20 * time.Hour).Truncate(time.Second)
	f.admission.reason, f.admission.until = FailQuota, tomorrow
	f.exec(t, "UPDATE baseline_refreshes SET available_at = now() WHERE repo_id=$1", f.repo)
	if after, pending, err := f.runner.Run(ctx, f.repo); err != nil || !pending || after < 19*time.Hour {
		t.Fatalf("the daily allowance waits until it resets: %v %v %v", after, pending, err)
	}
	if r := f.refreshes(t); r[0].WaitingReason != FailQuota || !r[0].AvailableAt.Equal(tomorrow) {
		t.Fatalf("waiting for the allowance: %+v", r)
	}
	f.admission.reason, f.admission.until = "", time.Time{}
	f.exec(t, "UPDATE baseline_refreshes SET available_at = now() WHERE repo_id=$1", f.repo)
	f.builder.err = &BuildError{Code: FailUnavailable, Detail: "GitHub could not be reached.", Retryable: true, Err: errors.New("timeout")}
	if after, pending, err := f.runner.Run(ctx, f.repo); err != nil || !pending || after < time.Minute {
		t.Fatalf("work that could not run tries again later: %v %v %v", after, pending, err)
	}
	if r := f.refreshes(t); r[0].State != StatePending || r[0].Attempts != 1 || r[0].WaitingReason != "" {
		t.Fatalf("retrying: %+v", r)
	}
	f.exec(t, "UPDATE baseline_refreshes SET available_at = now(), attempts = $2 WHERE repo_id=$1", f.repo, maxRetries)
	if _, pending, err := f.runner.Run(ctx, f.repo); err != nil || pending {
		t.Fatalf("run: %v %v", pending, err)
	}
	if r := f.refreshes(t); r[0].State != StateFailed || r[0].FailureCode != FailUnavailable {
		t.Fatalf("after too many tries it fails: %+v", r)
	}
}

func TestAutomaticRefreshesAreSpacedButRefreshBaselineIsNot(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.enable(t)
	f.builder.result = dumps(strings.Repeat("a", 32))
	if _, _, err := f.runner.Run(ctx, f.repo); err != nil {
		t.Fatal(err)
	}
	// Turning Test data on is a person's action: it does not space the first
	// merge refresh, but that one spaces the next.
	if err := f.store.Request(ctx, f.repo, TriggerMerged, 0); err != nil {
		t.Fatal(err)
	}
	f.builder.result = dumps(strings.Repeat("b", 32))
	if _, pending, err := f.runner.Run(ctx, f.repo); err != nil || pending || len(f.builder.built()) != 2 {
		t.Fatalf("the first merge refresh runs at once: %v %v", pending, err)
	}
	f.jobs.finish(f.repo)
	if err := f.store.Request(ctx, f.repo, TriggerMerged, 0); err != nil {
		t.Fatal(err)
	}
	after, pending, err := f.runner.Run(ctx, f.repo)
	if err != nil || !pending || after < 9*time.Minute || after > 10*time.Minute || len(f.builder.built()) != 2 {
		t.Fatalf("a merge right after an automatic refresh waits for the interval: %v %v %v", after, pending, err)
	}
	// Refresh baseline joins the waiting refresh, lifts the spacing and moves
	// the waiting job forward.
	f.jobs.scheduled[f.repo] = time.Now().Add(after)
	if err := f.service.Refresh(ctx, f.dev, f.repo); err != nil {
		t.Fatal(err)
	}
	if len(f.jobs.retried) != 1 {
		t.Fatalf("the waiting job is started now: %v", f.jobs.retried)
	}
	f.builder.result = dumps(strings.Repeat("c", 32))
	if _, pending, err := f.runner.Run(ctx, f.repo); err != nil || pending || len(f.builder.built()) != 3 {
		t.Fatalf("Refresh baseline is not spaced: %v %v", pending, err)
	}
	if r := f.refreshes(t); len(r) != 3 || r[2].Trigger != TriggerButton || r[2].State != StateSucceeded {
		t.Fatalf("refreshes: %+v", r)
	}
}

func TestARefreshWhoseWorkerDiedIsTakenOver(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.enable(t)
	f.exec(t, "UPDATE baseline_refreshes SET state='running', started_at=now(), attempts=1, log_open=true WHERE repo_id=$1", f.repo)
	f.builder.result = dumps(strings.Repeat("a", 32))
	if _, pending, err := f.runner.Run(ctx, f.repo); err != nil || pending {
		t.Fatalf("run: %v %v", pending, err)
	}
	if r := f.refreshes(t); r[0].State != StateSucceeded || r[0].Attempts != 2 || len(f.builder.built()) != 1 {
		t.Fatalf("the orphaned refresh is taken over: %+v", r)
	}
}

func TestTurningTestDataOffDiscardsARunningRefresh(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.enable(t)
	f.builder.result = dumps(strings.Repeat("a", 32))
	f.builder.during = func(BuildJob) {
		settings, err := f.store.Settings(ctx, f.repo)
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := f.service.TurnOff(ctx, f.owner, f.repo, settings.Revision); err != nil {
			t.Error(err)
		}
	}
	if _, _, err := f.runner.Run(ctx, f.repo); err != nil {
		t.Fatal(err)
	}
	if c := f.current(t); len(c) != 0 || !slices.Contains(f.files.removed(), strings.Repeat("a", 32)) {
		t.Fatalf("the result is discarded with its file: %+v %v", c, f.files.removed())
	}
	if r := f.refreshes(t); r[0].State != StateCancelled {
		t.Fatalf("the refresh is cancelled: %+v", r)
	}
	// A refresh waiting while Test data is off is cancelled, not built.
	f.exec(t, "INSERT INTO baseline_refreshes (repo_id, sequence, trigger) VALUES ($1, 2, 'button')", f.repo)
	builds := len(f.builder.built())
	if _, pending, err := f.runner.Run(ctx, f.repo); err != nil || pending || len(f.builder.built()) != builds {
		t.Fatalf("run: %v %v", pending, err)
	}
	if r := f.refreshes(t); r[1].State != StateCancelled {
		t.Fatalf("cancelled: %+v", r)
	}
}

func TestPausedBuildsLeaveRefreshesWaiting(t *testing.T) {
	f := newFixture(t)
	f.enable(t)
	f.runner.parts.Paused = true
	if after, pending, err := f.runner.Run(t.Context(), f.repo); err != nil || !pending || after <= 0 || len(f.builder.built()) != 0 {
		t.Fatalf("paused: %v %v %v", after, pending, err)
	}
	if r := f.refreshes(t); r[0].State != StatePending {
		t.Fatalf("still waiting: %+v", r)
	}
}

func TestABaselineIsBuiltFromAChosenBranch(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.branches.set("release", shaTwo)
	if _, err := f.service.Save(ctx, f.owner, f.repo, Form{Enabled: true, KeepData: true, UseBaseline: true, AutoRefresh: true, Branch: "release"}); err != nil {
		t.Fatal(err)
	}
	f.builder.result = dumps(strings.Repeat("a", 32))
	if _, _, err := f.runner.Run(ctx, f.repo); err != nil {
		t.Fatal(err)
	}
	built := f.builder.built()
	if len(built) != 1 || built[0].Branch != "release" || built[0].CommitSHA != shaTwo {
		t.Fatalf("the chosen branch is built, not the default: %+v", built)
	}
	if c := f.current(t); len(c) != 1 || c[0].Branch != "release" || c[0].CommitSHA != shaTwo {
		t.Fatalf("baseline: %+v", c)
	}
}
