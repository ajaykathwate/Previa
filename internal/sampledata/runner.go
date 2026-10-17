package sampledata

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/sampledata/dbgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Refresh failure categories, recorded with the refresh and shown on the
// Test data tab.
const (
	FailBranchNotFound = "branch_not_found"
	FailConfigInvalid  = "config_invalid"
	FailBuild          = "build_failed"
	FailService        = "service_failed"
	FailSetup          = "setup_failed"
	FailSeed           = "seed_failed"
	FailCapture        = "capture_failed"
	FailCaptureTooBig  = "capture_too_large"
	FailQuota          = "quota_blocked"
	FailCapacity       = "capacity_wait"
	FailUnavailable    = "unavailable"
)

// Activity kinds this feature records, visible to the people who may build
// in the repository.
const (
	KindSettingsUpdated   = "test_data.settings_updated"
	KindBaselineRefreshed = "test_data.baseline_refreshed"
	KindBaselineFailed    = "test_data.baseline_failed"
	KindBaselineDeleted   = "test_data.baseline_deleted"
)

// BuildJob is one baseline to build and capture.
type BuildJob struct {
	RepoID, GitHubRepoID, InstallationID, WorkspaceID int64
	RefreshID                                         int64
	Branch, CommitSHA                                 string
}

// Dump is one stored dump of a built baseline.
type Dump struct {
	Kind, Version string
	Stored        Stored
}

// BuildResult is a built baseline.
type BuildResult struct {
	ConfigHash string
	Dumps      []Dump
}

// BuildError is a classified build failure: Code is a failure category,
// Detail a sentence safe to show; Retryable failures wait and try again.
type BuildError struct {
	Code, Detail string
	Retryable    bool
	Err          error
}

func (e *BuildError) Error() string { return e.Code + ": " + e.Detail }
func (e *BuildError) Unwrap() error { return e.Err }

// BaselineBuilder builds and captures a baseline; internal/app adapts the
// runtime. Dumps it stored before failing are its own to remove.
type BaselineBuilder interface {
	BuildBaseline(ctx context.Context, job BuildJob) (BuildResult, error)
}

// Branches resolves a baseline branch to its head; *GitHubBranches
// implements it.
type Branches interface {
	BranchHead(ctx context.Context, installationID, repoID int64, branch string) (string, string, error)
}

// Admission decides when a baseline build may run and accounts for its
// time; the environments feature implements it.
type Admission interface {
	AdmitBaselineTx(ctx context.Context, tx pgx.Tx, workspaceID, refreshID int64, now time.Time) (string, time.Time, error)
	RecordBaselineUsage(ctx context.Context, workspaceID int64, day time.Time, seconds int64) error
}

// Activity records repository events in the caller's transaction.
type Activity interface {
	RecordTx(ctx context.Context, tx pgx.Tx, e Event) error
}

// Event is one test-data event of a repository. Metadata is small and safe:
// never data, row counts or credentials.
type Event struct {
	RepoID, ActorID int64
	Kind            string
	Metadata        map[string]any
	DedupKey        string
}

// Files removes stored dumps; *Storage implements it.
type Files interface {
	Remove(key string) error
}

// RunnerParts are what refreshes need from other features.
type RunnerParts struct {
	Builder   BaselineBuilder
	Branches  Branches
	Admission Admission
	Activity  Activity
	Files     Files
	// Timeout bounds one baseline build, as BUILD_TIMEOUT bounds a pull
	// request's.
	Timeout time.Duration
	// MinInterval spaces automatic refreshes of one repository
	// (PREVIEW_BASELINE_MIN_INTERVAL); those a person asked for are not.
	MinInterval time.Duration
	// Paused: the operator stopped new builds (BUILDS_PAUSED); refreshes
	// wait like pull request builds.
	Paused bool
}

// maxRetries bounds how often a refresh that could not run (GitHub or the
// worker unavailable) tries again before it fails; waiting for capacity or
// the daily allowance has no bound.
const maxRetries = 10

// Runner runs baseline refreshes on a worker.
type Runner struct {
	store  *Store
	parts  RunnerParts
	logger *slog.Logger
	retry  time.Duration
	now    func() time.Time
}

// NewRunner assembles the worker side of refreshes.
func NewRunner(store *Store, parts RunnerParts, logger *slog.Logger) (*Runner, error) {
	p := parts
	if store == nil || p.Builder == nil || p.Branches == nil || p.Admission == nil || p.Activity == nil || p.Files == nil || logger == nil {
		return nil, errors.New("baseline refreshes require a store, a builder, branch lookups, admission, activity, files and a logger")
	}
	if p.Timeout <= 0 || p.MinInterval <= 0 {
		return nil, errors.New("baseline refreshes require a build timeout and a minimum interval")
	}
	return &Runner{store: store, parts: parts, logger: logger, retry: 2 * time.Minute, now: time.Now}, nil
}

// JobTimeout bounds one run of the refresh job: a build and its bookkeeping.
func (r *Runner) JobTimeout() time.Duration { return r.parts.Timeout + 3*time.Minute }

// claimed is a refresh this run started.
type claimed struct {
	refresh  dbgen.BaselineRefresh
	settings Settings
	scope    dbgen.WorkerScopeRow
}

// Run runs the repository's waiting refresh if it may start now. pending
// reports that a refresh still waits, and after when to look again.
func (r *Runner) Run(ctx context.Context, repoID int64) (after time.Duration, pending bool, err error) {
	if r.parts.Paused {
		return r.retry, true, nil
	}
	// One run per repository at a time across workers: a session lock that a
	// crashed worker's lost connection releases.
	conn, err := r.store.pool.Acquire(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("acquire a connection: %w", err)
	}
	defer conn.Release()
	const lock = "hashtextextended('previa.baseline.' || instance || '.' || ($1::bigint)::text, 0)"
	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock("+lock+") FROM platform_instance", repoID).Scan(&locked); err != nil {
		return 0, false, fmt.Errorf("lock the repository's refreshes: %w", err)
	}
	if !locked {
		return r.retry, true, nil
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock("+lock+") FROM platform_instance", repoID); err != nil {
			// The connection goes back to the pool: close it so the lock goes.
			_ = conn.Conn().Close(unlockCtx)
		}
	}()
	c, wait, err := r.claim(ctx, repoID)
	switch {
	case err != nil:
		return 0, false, err
	case c == nil && wait > 0:
		return wait, true, nil
	case c != nil:
		if err := r.run(ctx, *c); err != nil {
			return 0, false, err
		}
	}
	return r.next(ctx, repoID)
}

// next says when the repository's waiting refresh, if any, may start.
func (r *Runner) next(ctx context.Context, repoID int64) (time.Duration, bool, error) {
	waiting, err := dbgen.New(r.store.pool).PendingRefresh(ctx, repoID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read the waiting refresh: %w", err)
	}
	return max(waiting.AvailableAt.Time.Sub(r.now()), 0), true, nil
}

// claim starts the repository's waiting refresh, or takes over one whose
// worker died, when its time came and the build may be admitted. Otherwise
// it returns how long the refresh waits.
func (r *Runner) claim(ctx context.Context, repoID int64) (*claimed, time.Duration, error) {
	var out *claimed
	var wait time.Duration
	err := database.InTransaction(ctx, r.store.pool, func(tx pgx.Tx) error {
		var err error
		out, wait, err = r.claimTx(ctx, tx, repoID)
		return err
	})
	return out, wait, err
}

func (r *Runner) claimTx(ctx context.Context, tx pgx.Tx, repoID int64) (*claimed, time.Duration, error) {
	q := dbgen.New(tx)
	refresh, settings, found, err := waitingRefresh(ctx, q, repoID)
	if err != nil || !found {
		return nil, 0, err
	}
	if !settings.Enabled || !settings.UseBaseline {
		return nil, 0, finishTx(ctx, q, refresh.ID, StateCancelled, "", "")
	}
	now := r.now()
	if wait, err := r.notYet(ctx, q, refresh, now); err != nil || wait > 0 {
		return nil, wait, err
	}
	scope, err := q.WorkerScope(ctx, repoID)
	if err != nil {
		return nil, 0, fmt.Errorf("read the repository: %w", err)
	}
	if !scope.Available || !scope.GithubRepoID.Valid {
		return nil, 0, finishTx(ctx, q, refresh.ID, StateFailed, FailUnavailable, "Previa's GitHub access to this repository was removed.")
	}
	if wait, err := r.admit(ctx, tx, q, scope, refresh, now); err != nil || wait > 0 {
		return nil, wait, err
	}
	started, err := q.ClaimRefresh(ctx, refresh.ID)
	if err != nil {
		return nil, 0, fmt.Errorf("start the refresh: %w", err)
	}
	return &claimed{refresh: started, settings: settings, scope: scope}, 0, nil
}

// waitingRefresh locks the repository and its settings and finds the refresh
// to run: a running one (orphaned, since this run holds the repository's
// refresh lock) or the waiting one. found is false when there is none.
func waitingRefresh(ctx context.Context, q *dbgen.Queries, repoID int64) (dbgen.BaselineRefresh, Settings, bool, error) {
	settings, err := lockSettings(ctx, q, repoID)
	if errors.Is(err, pgx.ErrNoRows) {
		return dbgen.BaselineRefresh{}, Settings{}, false, nil // the repository is gone
	}
	if err != nil {
		return dbgen.BaselineRefresh{}, Settings{}, false, err
	}
	refresh, err := q.RunningRefresh(ctx, repoID)
	if errors.Is(err, pgx.ErrNoRows) {
		refresh, err = q.PendingRefresh(ctx, repoID)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return dbgen.BaselineRefresh{}, settings, false, nil
	}
	if err != nil {
		return dbgen.BaselineRefresh{}, settings, false, fmt.Errorf("read the waiting refresh: %w", err)
	}
	return refresh, settings, true, nil
}

// notYet says how long a waiting refresh still waits: until its time, or to
// space automatic refreshes of the repository.
func (r *Runner) notYet(ctx context.Context, q *dbgen.Queries, refresh dbgen.BaselineRefresh, now time.Time) (time.Duration, error) {
	if refresh.State != StatePending {
		return 0, nil
	}
	if automatic(refresh.Trigger) {
		last, err := q.LastAutomaticStart(ctx, dbgen.LastAutomaticStartParams{RepoID: refresh.RepoID, RefreshID: refresh.ID})
		if err != nil {
			return 0, fmt.Errorf("read the last automatic refresh: %w", err)
		}
		if spaced := last.Time.Add(r.parts.MinInterval); last.Valid && spaced.After(refresh.AvailableAt.Time) && spaced.After(now) {
			if err := q.WaitRefresh(ctx, dbgen.WaitRefreshParams{ID: refresh.ID, AvailableAt: timestamp(spaced)}); err != nil {
				return 0, fmt.Errorf("space the refresh: %w", err)
			}
			return spaced.Sub(now), nil
		}
	}
	if refresh.AvailableAt.Time.After(now) {
		return refresh.AvailableAt.Time.Sub(now), nil
	}
	return 0, nil
}

// admit asks for a build slot. A refresh that may not run now waits and keeps
// its place; the returned time is how long.
func (r *Runner) admit(ctx context.Context, tx pgx.Tx, q *dbgen.Queries, scope dbgen.WorkerScopeRow, refresh dbgen.BaselineRefresh, now time.Time) (time.Duration, error) {
	reason, until, err := r.parts.Admission.AdmitBaselineTx(ctx, tx, scope.WorkspaceID.Int64, refresh.ID, now)
	if err != nil || reason == "" {
		return 0, err
	}
	at := until
	if at.IsZero() {
		at = now.Add(r.retry)
	}
	if err := q.WaitRefresh(ctx, dbgen.WaitRefreshParams{ID: refresh.ID, WaitingReason: text(reason), AvailableAt: timestamp(at)}); err != nil {
		return 0, fmt.Errorf("let the refresh wait: %w", err)
	}
	return max(at.Sub(now), time.Second), nil
}

// finishTx ends a refresh that will not run.
func finishTx(ctx context.Context, q *dbgen.Queries, id int64, state, code, detail string) error {
	if _, err := q.FinishRefresh(ctx, dbgen.FinishRefreshParams{ID: id, State: state, FailureCode: text(code), FailureDetail: text(detail)}); err != nil {
		return fmt.Errorf("finish the refresh: %w", err)
	}
	return nil
}

// run resolves the branch head, builds and captures the baseline and
// records the outcome. It returns an error only when nothing could be
// recorded; the refresh then stays running and the job's next run takes it
// over.
func (r *Runner) run(ctx context.Context, c claimed) error {
	refresh, scope := c.refresh, c.scope
	branch, sha, err := r.parts.Branches.BranchHead(ctx, scope.GithubInstallationID, scope.GithubRepoID.Int64, c.settings.Branch)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, ErrBranchNotFound):
		detail := "The repository has no default branch on GitHub."
		if name := cmp.Or(c.settings.Branch, branch); name != "" {
			detail = "The branch " + name + " does not exist on GitHub; choose another baseline branch."
		}
		return r.fail(ctx, refresh, &BuildError{Code: FailBranchNotFound, Detail: detail}, 0)
	case err != nil:
		return r.retryLater(ctx, refresh, &BuildError{Code: FailUnavailable, Detail: "GitHub could not be reached.", Retryable: true, Err: err})
	}
	q := dbgen.New(r.store.pool)
	if err := q.SetRefreshTarget(ctx, dbgen.SetRefreshTargetParams{ID: refresh.ID, Branch: text(branch), CommitSha: text(sha)}); err != nil {
		return fmt.Errorf("record the refresh's commit: %w", err)
	}
	if err := q.MarkChecked(ctx, dbgen.MarkCheckedParams{RepoID: refresh.RepoID, Branch: text(branch), Sha: text(sha)}); err != nil {
		return fmt.Errorf("record the branch head: %w", err)
	}
	started := r.now()
	buildCtx, cancel := context.WithTimeout(ctx, r.parts.Timeout)
	result, err := r.parts.Builder.BuildBaseline(buildCtx, BuildJob{
		RepoID: refresh.RepoID, GitHubRepoID: scope.GithubRepoID.Int64, InstallationID: scope.GithubInstallationID, WorkspaceID: scope.WorkspaceID.Int64,
		RefreshID: refresh.ID, Branch: branch, CommitSHA: sha,
	})
	timedOut := buildCtx.Err() != nil && ctx.Err() == nil
	cancel()
	took := r.now().Sub(started)
	if usageErr := r.parts.Admission.RecordBaselineUsage(context.WithoutCancel(ctx), scope.WorkspaceID.Int64, started, int64(took.Seconds())); usageErr != nil {
		r.logger.WarnContext(ctx, "record baseline build time", "repo_id", refresh.RepoID, "error", usageErr)
	}
	if err != nil && ctx.Err() != nil {
		return ctx.Err() // the worker stops: the next run takes the refresh over
	}
	var failed *BuildError
	switch {
	case err != nil && timedOut:
		failed = &BuildError{Code: FailBuild, Detail: fmt.Sprintf("The baseline did not finish within %s (BUILD_TIMEOUT).", r.parts.Timeout), Err: err}
	case err != nil && !errors.As(err, &failed):
		failed = &BuildError{Code: FailUnavailable, Detail: "The baseline could not be built on the worker.", Retryable: true, Err: err}
	}
	switch {
	case failed != nil && failed.Retryable:
		return r.retryLater(ctx, refresh, failed)
	case failed != nil:
		return r.fail(ctx, refresh, failed, took)
	}
	return r.succeed(ctx, refresh, branch, sha, result, took)
}

// retryLater lets the refresh wait and try again, unless one that could not
// run already tried too often. Waiting for capacity has no bound.
func (r *Runner) retryLater(ctx context.Context, refresh dbgen.BaselineRefresh, failed *BuildError) error {
	r.logger.WarnContext(ctx, "baseline refresh waits", "repo_id", refresh.RepoID, "refresh_id", refresh.ID, "code", failed.Code, "error", failed.Err)
	reason := ""
	if failed.Code == FailCapacity || failed.Code == FailQuota {
		reason = failed.Code
	} else if refresh.Attempts >= maxRetries {
		return r.fail(ctx, refresh, failed, 0)
	}
	err := dbgen.New(r.store.pool).WaitRefresh(ctx, dbgen.WaitRefreshParams{ID: refresh.ID, WaitingReason: text(reason), AvailableAt: timestamp(r.now().Add(r.retry))})
	if err != nil {
		return fmt.Errorf("let the refresh wait: %w", err)
	}
	return nil
}

// fail records a failed refresh. The previous baseline stays current.
func (r *Runner) fail(ctx context.Context, refresh dbgen.BaselineRefresh, failed *BuildError, took time.Duration) error {
	r.logger.InfoContext(ctx, "baseline refresh failed", "repo_id", refresh.RepoID, "refresh_id", refresh.ID, "code", failed.Code, "error", failed.Err)
	return database.InTransaction(ctx, r.store.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, err := q.LockRepository(ctx, refresh.RepoID); err != nil {
			return fmt.Errorf("lock the repository: %w", err)
		}
		if _, err := q.FinishRefresh(ctx, dbgen.FinishRefreshParams{ID: refresh.ID, State: StateFailed, FailureCode: text(failed.Code),
			FailureDetail: text(trim(failed.Detail, 500)), BuildSeconds: seconds(took)}); errors.Is(err, pgx.ErrNoRows) {
			return nil // cancelled meanwhile
		} else if err != nil {
			return fmt.Errorf("record the failed refresh: %w", err)
		}
		return r.parts.Activity.RecordTx(ctx, tx, Event{RepoID: refresh.RepoID, Kind: KindBaselineFailed,
			Metadata: map[string]any{"refresh": refresh.Sequence, "code": failed.Code}, DedupKey: fmt.Sprintf("baseline:%d:failed", refresh.ID)})
	})
}

// succeed makes the captured dumps the repository's baselines and retires
// the ones they replace, unless the result no longer applies (resultApplies).
// A result that does not apply is discarded with its files.
func (r *Runner) succeed(ctx context.Context, refresh dbgen.BaselineRefresh, branch, sha string, result BuildResult, took time.Duration) error {
	kept := false
	err := database.InTransaction(ctx, r.store.pool, func(tx pgx.Tx) error {
		var err error
		kept, err = r.succeedTx(ctx, tx, refresh, branch, sha, result, took)
		return err
	})
	if err != nil || !kept {
		// Unrecorded files are removed now; cleanup would remove them later.
		for _, d := range result.Dumps {
			if removeErr := r.parts.Files.Remove(d.Stored.Key); removeErr != nil {
				r.logger.WarnContext(ctx, "remove an unrecorded baseline file", "repo_id", refresh.RepoID, "error", removeErr)
			}
		}
	}
	return err
}

func (r *Runner) succeedTx(ctx context.Context, tx pgx.Tx, refresh dbgen.BaselineRefresh, branch, sha string, result BuildResult, took time.Duration) (bool, error) {
	q := dbgen.New(tx)
	current, applies, err := resultApplies(ctx, q, refresh, branch)
	if err != nil {
		return false, err
	}
	if !applies {
		_, err := q.FinishRefresh(ctx, dbgen.FinishRefreshParams{ID: refresh.ID, State: StateCancelled, BuildSeconds: seconds(took)})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, fmt.Errorf("record the discarded refresh: %w", err)
		}
		return false, nil
	}
	for _, b := range current {
		if err := q.RetireBaseline(ctx, b.ID); err != nil {
			return false, fmt.Errorf("retire a baseline: %w", err)
		}
	}
	for _, d := range result.Dumps {
		if _, err := q.InsertBaseline(ctx, dbgen.InsertBaselineParams{
			RepoID: refresh.RepoID, RefreshID: pgtype.Int8{Int64: refresh.ID, Valid: true}, ServiceKind: d.Kind, ServiceVersion: d.Version,
			Branch: branch, CommitSha: sha, ConfigHash: text(result.ConfigHash), RefreshSequence: refresh.Sequence,
			StorageKey: d.Stored.Key, SizeBytes: d.Stored.Size, Checksum: d.Stored.Checksum, KeyVersion: d.Stored.KeyVersion, Status: "current",
		}); err != nil {
			return false, fmt.Errorf("record a baseline: %w", err)
		}
	}
	if _, err := q.FinishRefresh(ctx, dbgen.FinishRefreshParams{ID: refresh.ID, State: StateSucceeded, BuildSeconds: seconds(took)}); err != nil {
		return false, fmt.Errorf("record the refresh: %w", err)
	}
	return true, r.parts.Activity.RecordTx(ctx, tx, Event{RepoID: refresh.RepoID, Kind: KindBaselineRefreshed,
		Metadata: map[string]any{"refresh": refresh.Sequence, "branch": branch, "commit": sha}, DedupKey: fmt.Sprintf("baseline:%d:refreshed", refresh.ID)})
}

// resultApplies locks the repository, its settings and its current
// baselines, and says whether a finished refresh's result may become
// current: Test data and the baseline are still on, its branch is still the
// baseline branch, and no newer refresh finished first (the sequence fence).
func resultApplies(ctx context.Context, q *dbgen.Queries, refresh dbgen.BaselineRefresh, branch string) ([]dbgen.RepositoryBaseline, bool, error) {
	settings, err := lockSettings(ctx, q, refresh.RepoID)
	if err != nil {
		return nil, false, err
	}
	current, err := q.LockCurrentBaselines(ctx, refresh.RepoID)
	if err != nil {
		return nil, false, fmt.Errorf("read current baselines: %w", err)
	}
	applies := settings.Enabled && settings.UseBaseline && (settings.Branch == "" || settings.Branch == branch)
	for _, b := range current {
		applies = applies && b.RefreshSequence < refresh.Sequence
	}
	return current, applies, nil
}

func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func timestamp(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func seconds(d time.Duration) pgtype.Int4 {
	if d <= 0 {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(min(d.Seconds(), 1<<31-1)), Valid: true}
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
