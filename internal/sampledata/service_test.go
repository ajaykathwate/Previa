package sampledata

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestOnlyOwnersAndAdminsChangeTestDataSettings(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	on := Form{Enabled: true, KeepData: true, UseBaseline: true, AutoRefresh: true}
	if _, err := f.service.Save(ctx, f.dev, f.repo, on); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a Developer cannot change settings: %v", err)
	}
	if _, err := f.service.Save(ctx, f.reviewer, f.repo, on); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a Reviewer does not see the tab: %v", err)
	}
	if _, err := f.service.Overview(ctx, f.reviewer, f.repo); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a Reviewer does not see the tab: %v", err)
	}
	overview, err := f.service.Overview(ctx, f.dev, f.repo)
	if err != nil || overview.Scope.CanManage || overview.Settings.Enabled || !overview.Settings.KeepData || !overview.Offer.Databases {
		t.Fatalf("a Developer reads the defaults: %+v %v", overview, err)
	}
	saved, err := f.service.Save(ctx, f.admin, f.repo, on)
	if err != nil || !saved.Enabled || saved.Revision != 1 {
		t.Fatalf("an Admin turns Test data on: %+v %v", saved, err)
	}
	if _, err := f.service.Save(ctx, f.owner, f.repo, Form{Revision: 0, Enabled: true, KeepData: false, UseBaseline: true, AutoRefresh: true}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a stale form is refused: %v", err)
	}
	if _, err := f.service.Save(ctx, f.owner, f.repo, Form{Revision: 1, Enabled: false, KeepData: true, UseBaseline: true, AutoRefresh: true}); !errors.Is(err, ErrConflict) {
		t.Fatalf("turning off through Save is refused: %v", err)
	}
	var invalid *InvalidError
	if _, err := f.service.Save(ctx, f.owner, f.repo, Form{Revision: 1, Enabled: true, UseBaseline: true, Branch: "feature branch"}); !errors.As(err, &invalid) || invalid.Field != "baseline_branch" {
		t.Fatalf("an invalid branch is refused: %v", err)
	}
}

func TestSettingsChangesFollowTheirConsequences(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	// Settings can be prepared while Test data is off: no refresh.
	saved, err := f.service.Save(ctx, f.owner, f.repo, Form{KeepData: true, UseBaseline: true, AutoRefresh: false, Branch: "develop"})
	if err != nil || saved.Enabled || saved.Branch != "develop" || len(f.refreshes(t)) != 0 {
		t.Fatalf("prepared while off: %+v %v %v", saved, err, f.refreshes(t))
	}
	if saved, err = f.service.Save(ctx, f.owner, f.repo, Form{Revision: saved.Revision, Enabled: true, KeepData: true, UseBaseline: true, Branch: "develop"}); err != nil {
		t.Fatal(err)
	}
	if r := f.refreshes(t); len(r) != 1 || r[0].Trigger != TriggerEnabled {
		t.Fatalf("turning on builds the first baseline even without automatic refresh: %+v", r)
	}
	f.exec(t, "UPDATE baseline_refreshes SET state='failed', failure_code='branch_not_found', finished_at=now() WHERE repo_id=$1", f.repo)
	// Without automatic refresh another branch waits for Refresh baseline.
	if saved, err = f.service.Save(ctx, f.owner, f.repo, Form{Revision: saved.Revision, Enabled: true, KeepData: true, UseBaseline: true, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	if r := f.refreshes(t); len(r) != 1 {
		t.Fatalf("no automatic refresh: %+v", r)
	}
	// With it, another branch rebuilds the baseline.
	if saved, err = f.service.Save(ctx, f.owner, f.repo, Form{Revision: saved.Revision, Enabled: true, KeepData: true, UseBaseline: true, AutoRefresh: true, Branch: "release"}); err != nil {
		t.Fatal(err)
	}
	if r := f.refreshes(t); len(r) != 2 || r[1].Trigger != TriggerBranch || r[1].State != StatePending {
		t.Fatalf("another branch refreshes: %+v", r)
	}
	// Turning the baseline off cancels the waiting refresh but keeps data
	// kept between builds.
	if saved, err = f.service.Save(ctx, f.owner, f.repo, Form{Revision: saved.Revision, Enabled: true, KeepData: true, UseBaseline: false, AutoRefresh: true, Branch: "release"}); err != nil {
		t.Fatal(err)
	}
	if r := f.refreshes(t); r[1].State != StateCancelled {
		t.Fatalf("the waiting refresh is cancelled: %+v", r)
	}
	if err := f.service.Refresh(ctx, f.dev, f.repo); !errors.Is(err, ErrNoBaseline) {
		t.Fatalf("Refresh baseline needs the baseline on: %v", err)
	}
	if kinds := f.activity.kinds(); len(kinds) != 5 || slices.ContainsFunc(kinds, func(k string) bool { return k != KindSettingsUpdated }) {
		t.Fatalf("every save is recorded: %v", kinds)
	}
}

func TestTurningTestDataOffAndDeletingRetireBaselines(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	saved := f.enable(t)
	f.builder.result = dumps(strings.Repeat("a", 32))
	if _, _, err := f.runner.Run(ctx, f.repo); err != nil {
		t.Fatal(err)
	}
	if retired, err := f.service.Delete(ctx, f.dev, f.repo); !errors.Is(err, ErrForbidden) || retired != 0 {
		t.Fatalf("a Developer cannot delete baselines: %v", err)
	}
	if retired, err := f.service.Delete(ctx, f.admin, f.repo); err != nil || retired != 1 || len(f.current(t)) != 0 {
		t.Fatalf("Delete baseline retires it: %d %v", retired, err)
	}
	if retired, err := f.service.Delete(ctx, f.admin, f.repo); err != nil || retired != 0 {
		t.Fatalf("nothing left to delete: %d %v", retired, err)
	}
	if err := f.service.Refresh(ctx, f.dev, f.repo); err != nil {
		t.Fatalf("a Developer refreshes the baseline: %v", err)
	}
	f.builder.result = dumps(strings.Repeat("b", 32))
	if _, _, err := f.runner.Run(ctx, f.repo); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Refresh(ctx, f.dev, f.repo); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.TurnOff(ctx, f.dev, f.repo, saved.Revision); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a Developer cannot turn Test data off: %v", err)
	}
	if _, err := f.service.TurnOff(ctx, f.owner, f.repo, saved.Revision+5); !errors.Is(err, ErrConflict) {
		t.Fatalf("a stale confirmation is refused: %v", err)
	}
	retired, err := f.service.TurnOff(ctx, f.owner, f.repo, saved.Revision)
	if err != nil || retired != 1 || len(f.current(t)) != 0 {
		t.Fatalf("turning off retires the baselines: %d %v", retired, err)
	}
	if r := f.refreshes(t); r[len(r)-1].State != StateCancelled {
		t.Fatalf("the waiting refresh is cancelled: %+v", r)
	}
	if kinds := f.activity.kinds(); !slices.Contains(kinds, KindBaselineDeleted) {
		t.Fatalf("activity: %v", kinds)
	}
}

func TestMergesIntoTheBaselineBranchRequestARefresh(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	merge := func(installation, githubRepo int64, base, defaultBranch string) {
		t.Helper()
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := f.store.MergedTx(ctx, tx, installation, githubRepo, base, defaultBranch); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	merge(7, 100, "main", "main")
	if len(f.refreshes(t)) != 0 {
		t.Fatal("no refresh while Test data is off")
	}
	f.enable(t)
	f.exec(t, "UPDATE baseline_refreshes SET state='succeeded', finished_at=now() WHERE repo_id=$1", f.repo)
	merge(7, 100, "feature", "main")
	merge(8, 100, "main", "main") // another installation's event
	merge(7, 999, "main", "main") // another repository
	if r := f.refreshes(t); len(r) != 1 {
		t.Fatalf("only merges into this repository's baseline branch count: %+v", r)
	}
	merge(7, 100, "main", "main")
	if r := f.refreshes(t); len(r) != 2 || r[1].Trigger != TriggerMerged {
		t.Fatalf("a merge into the default branch refreshes: %+v", r)
	}
	f.exec(t, "UPDATE baseline_refreshes SET state='cancelled', finished_at=now() WHERE repo_id=$1 AND state='pending'", f.repo)
	f.exec(t, "UPDATE repository_sampledata_settings SET baseline_branch='develop' WHERE repo_id=$1", f.repo)
	merge(7, 100, "main", "main")
	merge(7, 100, "develop", "main")
	if r := f.refreshes(t); len(r) != 3 {
		t.Fatalf("a chosen branch replaces the default: %+v", r)
	}
	f.exec(t, "UPDATE baseline_refreshes SET state='cancelled', finished_at=now() WHERE repo_id=$1 AND state='pending'", f.repo)
	f.exec(t, "UPDATE repository_sampledata_settings SET auto_refresh=false WHERE repo_id=$1", f.repo)
	merge(7, 100, "develop", "main")
	if r := f.refreshes(t); len(r) != 3 {
		t.Fatalf("without automatic refresh a merge only waits for the button: %+v", r)
	}
}

func TestUnusableBaselinesAreRebuilt(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.enable(t)
	f.builder.result = dumps(strings.Repeat("a", 32))
	if _, _, err := f.runner.Run(ctx, f.repo); err != nil {
		t.Fatal(err)
	}
	b := f.current(t)[0]
	if err := f.store.Unusable(ctx, f.repo, b.ID); err != nil {
		t.Fatal(err)
	}
	if len(f.current(t)) != 0 {
		t.Fatal("an unreadable baseline is no longer current")
	}
	if r := f.refreshes(t); len(r) != 2 || r[1].Trigger != TriggerUnusable {
		t.Fatalf("a refresh rebuilds it: %+v", r)
	}
	// Reported twice (two candidates at once): one refresh.
	if err := f.store.Unusable(ctx, f.repo, b.ID); err != nil || len(f.refreshes(t)) != 2 {
		t.Fatalf("one refresh: %v", err)
	}
}

func TestBaselineBranchNamesFollowGit(t *testing.T) {
	for name, want := range map[string]bool{"": true, " main ": true, "release/2.0": true, "develop": true, "feature branch": false,
		"-x": false, "a..b": false, "refs/heads/main": false, "x.lock": false, "a//b": false, "a~1": false, "a:b": false, "trailing/": false, "@": false} {
		_, err := ValidBranch(name)
		if (err == nil) != want {
			t.Errorf("%q: %v", name, err)
		}
	}
	if got, _ := ValidBranch("  main "); got != "main" {
		t.Fatalf("trimmed: %q", got)
	}
}

func TestFreshnessAndCleanup(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.enable(t)
	f.builder.result = dumps(strings.Repeat("a", 32))
	if _, _, err := f.runner.Run(ctx, f.repo); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "baselines")
	storage, err := NewStorage(dir, testKeyring(t), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	leftovers := &fakeLeftovers{found: map[int64]time.Time{}}
	offered := map[string]bool{"postgres 17": true}
	jobs, err := NewJobs(f.store, JobParts{Runner: f.runner, Branches: f.branches, Directory: storage, Leftovers: leftovers,
		Offered: func(kind, version string) bool { return offered[kind+" "+version] }}, f.runner.logger)
	if err != nil {
		t.Fatal(err)
	}
	// Checked within a day: nothing to do.
	if err := jobs.Freshness(ctx); err != nil || f.branches.calls != 1 {
		t.Fatalf("a branch looked at today is not looked at again: %v %d", err, f.branches.calls)
	}
	stale := func() {
		f.exec(t, "UPDATE repository_sampledata_settings SET checked_at = now() - interval '25 hours' WHERE repo_id=$1", f.repo)
	}
	stale()
	if err := jobs.Freshness(ctx); err != nil || len(f.refreshes(t)) != 1 {
		t.Fatalf("an unmoved branch needs no refresh: %v %+v", err, f.refreshes(t))
	}
	// The head moved: a daily refresh.
	f.branches.set("main", shaTwo)
	stale()
	if err := jobs.Freshness(ctx); err != nil {
		t.Fatal(err)
	}
	if r := f.refreshes(t); len(r) != 2 || r[1].Trigger != TriggerDaily {
		t.Fatalf("a moved branch refreshes: %+v", r)
	}
	settings, _ := f.store.Settings(ctx, f.repo)
	if settings.CheckedSHA != shaTwo {
		t.Fatalf("the check is recorded: %+v", settings)
	}
	// The refresh fails at that commit: the daily check does not repeat it.
	f.exec(t, "UPDATE baseline_refreshes SET state='failed', failure_code='seed_failed', commit_sha=$2, finished_at=now() WHERE repo_id=$1 AND state='pending'", f.repo, shaTwo)
	stale()
	if err := jobs.Freshness(ctx); err != nil || len(f.refreshes(t)) != 2 {
		t.Fatalf("a commit already tried is not retried daily: %v %+v", err, f.refreshes(t))
	}
	// A catalog that no longer offers the baseline's version rebuilds it.
	f.branches.set("main", shaOne)
	delete(offered, "postgres 17")
	f.exec(t, "UPDATE baseline_refreshes SET commit_sha=NULL WHERE repo_id=$1 AND sequence=2", f.repo)
	stale()
	if err := jobs.Freshness(ctx); err != nil {
		t.Fatal(err)
	}
	if r := f.refreshes(t); len(r) != 3 || r[2].Trigger != TriggerUnusable {
		t.Fatalf("a version no longer offered is rebuilt: %+v", r)
	}
	// Without automatic refresh the check only records what it saw; a
	// missing branch is recorded without a commit.
	f.exec(t, "UPDATE baseline_refreshes SET state='cancelled', finished_at=now() WHERE repo_id=$1 AND state='pending'", f.repo)
	f.exec(t, "UPDATE repository_sampledata_settings SET auto_refresh=false, baseline_branch='gone' WHERE repo_id=$1", f.repo)
	stale()
	if err := jobs.Freshness(ctx); err != nil || len(f.refreshes(t)) != 3 {
		t.Fatalf("no refresh without automatic refresh: %v", err)
	}
	if settings, _ = f.store.Settings(ctx, f.repo); settings.CheckedBranch != "gone" || settings.CheckedSHA != "" {
		t.Fatalf("a missing branch is recorded: %+v", settings)
	}

	// Cleanup: a retired file goes after its grace, a file no baseline
	// records goes once old, leftovers of refreshes that are not running go.
	kept := store(t, storage, f.repo, []byte("current dump"))
	retired := store(t, storage, f.repo, []byte("retired dump"))
	orphan := store(t, storage, f.repo, []byte("orphan dump"))
	f.exec(t, "UPDATE repository_baselines SET storage_key=$2 WHERE repo_id=$1", f.repo, kept.Key)
	f.exec(t, `INSERT INTO repository_baselines (repo_id, service_kind, service_version, branch, commit_sha, refresh_sequence, storage_key, size_bytes, checksum, key_version, status, retired_at)
		VALUES ($1, 'postgres', '17', 'main', $2, 1, $3, 1, $4, 1, 'retired', now() - interval '2 hours')`, f.repo, shaOne, retired.Key, retired.Checksum)
	old := time.Now().Add(-7 * time.Hour)
	for _, key := range []string{kept.Key, retired.Key, orphan.Key} {
		if err := osChtimes(filepath.Join(dir, key), old); err != nil {
			t.Fatal(err)
		}
	}
	var running int64
	if err := f.pool.QueryRow(ctx, "INSERT INTO baseline_refreshes (repo_id, sequence, trigger, state, started_at) VALUES ($1, 50, 'button', 'running', now()) RETURNING id", f.repo).Scan(&running); err != nil {
		t.Fatal(err)
	}
	leftovers.found[running] = old
	leftovers.found[9999] = old
	leftovers.found[9998] = time.Now()
	if err := jobs.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := readAll(storage, f.repo, kept); err != nil {
		t.Fatalf("the current file stays: %v", err)
	}
	for _, gone := range []Stored{retired, orphan} {
		if _, err := readAll(storage, f.repo, gone); !errors.Is(err, ErrMissing) {
			t.Fatalf("removed: %v", err)
		}
	}
	var rows int
	if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM repository_baselines WHERE repo_id=$1 AND status='retired'", f.repo).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("the retired row is deleted with its file: %d %v", rows, err)
	}
	if !slices.Equal(leftovers.removed, []int64{9999}) {
		t.Fatalf("only an old leftover of a refresh that is not running is removed: %v", leftovers.removed)
	}
}

func TestDispatchReplacesLostJobs(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.enable(t)
	f.jobs.finish(f.repo) // the job was lost with its worker
	jobs, err := NewJobs(f.store, JobParts{Runner: f.runner, Branches: f.branches, Directory: &emptyDirectory{}, Leftovers: &fakeLeftovers{},
		Offered: func(string, string) bool { return true }}, f.runner.logger)
	if err != nil {
		t.Fatal(err)
	}
	before := f.jobs.count()
	if err := jobs.Dispatch(ctx); err != nil || f.jobs.count() != before+1 {
		t.Fatalf("a due refresh without a job gets one: %v %d", err, f.jobs.count())
	}
	if err := jobs.Dispatch(ctx); err != nil || f.jobs.count() != before+1 {
		t.Fatalf("one job per repository: %v %d", err, f.jobs.count())
	}
}
