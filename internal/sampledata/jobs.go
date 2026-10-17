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
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// Queue runs the dispatcher, the freshness check and cleanup, one job at a
// time. Refreshes themselves run on the build queue.
const Queue = "test_data"

const (
	DispatchInterval  = time.Minute
	FreshnessInterval = time.Hour
	CleanupInterval   = time.Hour
	// FreshnessAge is how often each baseline branch head is looked at.
	FreshnessAge = 24 * time.Hour
	// A retired file stays this long: a candidate may still be restoring it.
	retiredGrace = time.Hour
	// A file no baseline records is removed once this old: a refresh records
	// its dumps right after capturing them.
	orphanGrace = 6 * time.Hour
	// Docker objects of a refresh that is not running are removed once this
	// old.
	leftoverGrace = 10 * time.Minute
	// keepRefreshes is how many finished refreshes (and their logs) each
	// repository keeps.
	keepRefreshes  = 20
	freshnessBatch = 50
	cleanupBatch   = 200
)

// Leftovers finds and removes what baseline refreshes left in Docker;
// *runtime.Runtime implements it.
type Leftovers interface {
	BaselineRefreshes(ctx context.Context) (map[int64]time.Time, error)
	RemoveBaseline(ctx context.Context, refreshID int64) error
	RemoveBaselineImages(ctx context.Context, refreshID int64) error
}

// Directory is the baseline storage as cleanup sees it; *Storage implements
// it.
type Directory interface {
	Files() ([]StoredFile, error)
	Remove(key string) error
	RemoveTemporary(key string) error
}

// JobParts are what the worker's Test data jobs need.
type JobParts struct {
	Runner    *Runner
	Branches  Branches
	Directory Directory
	Leftovers Leftovers
	// Offered reports whether the worker's service catalog offers a version;
	// a baseline of a version no longer offered is rebuilt.
	Offered func(kind, version string) bool
}

// Jobs runs the worker side of Test data.
type Jobs struct {
	store  *Store
	parts  JobParts
	logger *slog.Logger
	now    func() time.Time
}

// NewJobs needs a store that can schedule refresh jobs.
func NewJobs(store *Store, parts JobParts, logger *slog.Logger) (*Jobs, error) {
	p := parts
	if store == nil || store.jobs == nil || p.Runner == nil || p.Branches == nil || p.Directory == nil || p.Leftovers == nil || p.Offered == nil || logger == nil {
		return nil, errors.New("test data jobs require a store with a job client, a runner, branch lookups, the baseline directory, Docker, the catalog and a logger")
	}
	return &Jobs{store: store, parts: parts, logger: logger, now: time.Now}, nil
}

// Job arguments of the periodic jobs carry no data.
type (
	DispatchArgs  struct{}
	FreshnessArgs struct{}
	CleanupArgs   struct{}
)

func (DispatchArgs) Kind() string  { return "test_data_dispatch" }
func (FreshnessArgs) Kind() string { return "test_data_freshness" }
func (CleanupArgs) Kind() string   { return "test_data_cleanup" }

// Register adds the refresh worker, the test data queue and the periodic
// jobs. River runs periodic jobs only on its elected leader.
func (j *Jobs) Register(cfg *river.Config) {
	if cfg.Queues == nil {
		cfg.Queues = map[string]river.QueueConfig{}
	}
	cfg.Queues[Queue] = river.QueueConfig{MaxWorkers: 1}
	if cfg.Workers == nil {
		cfg.Workers = river.NewWorkers()
	}
	river.AddWorker(cfg.Workers, &refreshWorker{runner: j.parts.Runner})
	river.AddWorker(cfg.Workers, &dispatchWorker{jobs: j})
	river.AddWorker(cfg.Workers, &freshnessWorker{jobs: j})
	river.AddWorker(cfg.Workers, &cleanupWorker{jobs: j})
	for _, p := range []struct {
		every time.Duration
		args  river.JobArgs
	}{{DispatchInterval, DispatchArgs{}}, {FreshnessInterval, FreshnessArgs{}}, {CleanupInterval, CleanupArgs{}}} {
		args := p.args
		cfg.PeriodicJobs = append(cfg.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(p.every), func() (river.JobArgs, *river.InsertOpts) {
			return args, &river.InsertOpts{Queue: Queue, MaxAttempts: 1, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
				rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled,
			}}}
		}, &river.PeriodicJobOpts{RunOnStart: true}))
	}
}

type refreshWorker struct {
	river.WorkerDefaults[RefreshArgs]
	runner *Runner
}

func (w *refreshWorker) Timeout(*river.Job[RefreshArgs]) time.Duration { return w.runner.JobTimeout() }

// Work runs the repository's refreshes; while one still waits, the job
// waits with it instead of ending.
func (w *refreshWorker) Work(ctx context.Context, job *river.Job[RefreshArgs]) error {
	if job.Args.RepoID <= 0 {
		return river.JobCancel(errors.New("a baseline refresh needs a repository"))
	}
	after, pending, err := w.runner.Run(ctx, job.Args.RepoID)
	switch {
	case err != nil && errors.Is(ctx.Err(), context.Canceled):
		return river.JobSnooze(0) // the worker stops; its next start takes the refresh over
	case err != nil:
		return err
	case pending:
		return river.JobSnooze(max(after, time.Second))
	}
	return nil
}

type dispatchWorker struct {
	river.WorkerDefaults[DispatchArgs]
	jobs *Jobs
}

func (*dispatchWorker) Timeout(*river.Job[DispatchArgs]) time.Duration { return time.Minute }
func (w *dispatchWorker) Work(ctx context.Context, _ *river.Job[DispatchArgs]) error {
	return w.jobs.Dispatch(ctx)
}

type freshnessWorker struct {
	river.WorkerDefaults[FreshnessArgs]
	jobs *Jobs
}

func (*freshnessWorker) Timeout(*river.Job[FreshnessArgs]) time.Duration { return 10 * time.Minute }
func (w *freshnessWorker) Work(ctx context.Context, _ *river.Job[FreshnessArgs]) error {
	return w.jobs.Freshness(ctx)
}

type cleanupWorker struct {
	river.WorkerDefaults[CleanupArgs]
	jobs *Jobs
}

func (*cleanupWorker) Timeout(*river.Job[CleanupArgs]) time.Duration { return 10 * time.Minute }
func (w *cleanupWorker) Work(ctx context.Context, _ *river.Job[CleanupArgs]) error {
	return w.jobs.Cleanup(ctx)
}

// Dispatch makes sure every repository with a refresh due, or running, has
// its job: a job lost with a worker, or discarded, is replaced.
func (j *Jobs) Dispatch(ctx context.Context) error {
	repos, err := dbgen.New(j.store.pool).RefreshesDue(ctx, 100)
	if err != nil {
		return fmt.Errorf("read due refreshes: %w", err)
	}
	var errs []error
	for _, repoID := range repos {
		if err := database.InTransaction(ctx, j.store.pool, func(tx pgx.Tx) error {
			return j.store.enqueueTx(ctx, tx, repoID, j.now())
		}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Freshness looks at the head of each baseline branch once a day. It records
// what it saw, so the tab can say the branch moved, and with automatic
// refresh asks for a refresh when the head moved past the baseline (and the
// latest refresh did not already try that commit) or when the catalog no
// longer offers a baseline's version.
func (j *Jobs) Freshness(ctx context.Context) error {
	q := dbgen.New(j.store.pool)
	due, err := q.DueForFreshness(ctx, dbgen.DueForFreshnessParams{Cutoff: timestamp(j.now().Add(-FreshnessAge)), Batch: freshnessBatch})
	if err != nil {
		return fmt.Errorf("read baseline branches to check: %w", err)
	}
	for _, row := range due {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		branch, sha, err := j.parts.Branches.BranchHead(ctx, row.GithubInstallationID, row.GithubRepoID.Int64, row.BaselineBranch)
		switch {
		case errors.Is(err, ErrBranchNotFound):
			// Recorded without a commit: the tab says the branch is gone.
			if err := q.MarkChecked(ctx, dbgen.MarkCheckedParams{RepoID: row.RepoID, Branch: text(cmp.Or(branch, row.BaselineBranch))}); err != nil {
				return fmt.Errorf("record the baseline branch check: %w", err)
			}
			continue
		case err != nil:
			j.logger.WarnContext(ctx, "check a baseline branch", "repo_id", row.RepoID, "error", err)
			continue // looked at again next hour
		}
		if err := q.MarkChecked(ctx, dbgen.MarkCheckedParams{RepoID: row.RepoID, Branch: text(branch), Sha: text(sha)}); err != nil {
			return fmt.Errorf("record the baseline branch check: %w", err)
		}
		if !row.AutoRefresh {
			continue
		}
		trigger, err := j.stale(ctx, row.RepoID, branch, sha)
		if err != nil {
			return err
		}
		if trigger == "" {
			continue
		}
		if err := j.store.Request(ctx, row.RepoID, trigger, 0); err != nil {
			return err
		}
	}
	return nil
}

// stale says why the repository's baseline should be rebuilt, if it should.
func (j *Jobs) stale(ctx context.Context, repoID int64, branch, sha string) (string, error) {
	q := dbgen.New(j.store.pool)
	latest, err := q.LatestRefreshes(ctx, dbgen.LatestRefreshesParams{RepoID: repoID, Limit: 1})
	if err != nil {
		return "", fmt.Errorf("read the latest refresh: %w", err)
	}
	if len(latest) == 1 && latest[0].CommitSha.String == sha {
		return "", nil // tried already; Refresh baseline tries again
	}
	current, err := q.CurrentBaselines(ctx, repoID)
	if err != nil {
		return "", fmt.Errorf("read baselines: %w", err)
	}
	if len(current) == 0 {
		return TriggerDaily, nil
	}
	for _, b := range current {
		if b.CommitSha != sha || b.Branch != branch {
			return TriggerDaily, nil
		}
	}
	for _, b := range current {
		if !j.parts.Offered(b.ServiceKind, b.ServiceVersion) {
			return TriggerUnusable, nil
		}
	}
	return "", nil
}

// Cleanup removes retired baseline files after a grace period, files and
// Docker objects no refresh accounts for, and old refresh records.
func (j *Jobs) Cleanup(ctx context.Context) error {
	q := dbgen.New(j.store.pool)
	now := j.now()
	var errs []error
	retired, err := q.RetiredBaselines(ctx, dbgen.RetiredBaselinesParams{Before: timestamp(now.Add(-retiredGrace)), Batch: cleanupBatch})
	if err != nil {
		return fmt.Errorf("read retired baselines: %w", err)
	}
	for _, b := range retired {
		if err := j.parts.Directory.Remove(b.StorageKey); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := q.DeleteBaseline(ctx, b.ID); err != nil {
			errs = append(errs, fmt.Errorf("delete a retired baseline: %w", err))
		}
	}
	errs = append(errs, j.removeOrphanFiles(ctx, now), j.removeLeftovers(ctx, now))
	if _, err := q.PruneRefreshes(ctx, keepRefreshes); err != nil {
		errs = append(errs, fmt.Errorf("prune baseline refreshes: %w", err))
	}
	return errors.Join(errs...)
}

// removeOrphanFiles removes files no baseline records (their repository was
// disconnected, or a refresh stopped before recording them).
func (j *Jobs) removeOrphanFiles(ctx context.Context, now time.Time) error {
	files, err := j.parts.Directory.Files()
	if err != nil {
		return err
	}
	keys, err := dbgen.New(j.store.pool).StorageKeys(ctx)
	if err != nil {
		return fmt.Errorf("read baseline files: %w", err)
	}
	recorded := make(map[string]bool, len(keys))
	for _, key := range keys {
		recorded[key] = true
	}
	var errs []error
	for _, f := range files {
		switch {
		case now.Sub(f.Modified) < orphanGrace:
		case f.Temporary:
			errs = append(errs, j.parts.Directory.RemoveTemporary(f.Key))
		case !recorded[f.Key]:
			errs = append(errs, j.parts.Directory.Remove(f.Key))
		}
	}
	return errors.Join(errs...)
}

// removeLeftovers removes Docker objects of refreshes that are not running,
// such as those of a worker that crashed mid-refresh.
func (j *Jobs) removeLeftovers(ctx context.Context, now time.Time) error {
	found, err := j.parts.Leftovers.BaselineRefreshes(ctx)
	if err != nil || len(found) == 0 {
		return err
	}
	ids := make([]int64, 0, len(found))
	for id := range found {
		ids = append(ids, id)
	}
	active, err := dbgen.New(j.store.pool).KnownRefreshIDs(ctx, ids)
	if err != nil {
		return fmt.Errorf("read active refreshes: %w", err)
	}
	busy := make(map[int64]bool, len(active))
	for _, id := range active {
		busy[id] = true
	}
	var errs []error
	for id, created := range found {
		if busy[id] || now.Sub(created) < leftoverGrace {
			continue
		}
		j.logger.InfoContext(ctx, "remove baseline leftovers", "refresh_id", id)
		errs = append(errs, j.parts.Leftovers.RemoveBaseline(ctx, id), j.parts.Leftovers.RemoveBaselineImages(ctx, id))
	}
	return errors.Join(errs...)
}

// RegisterWaiting keeps refresh jobs waiting on an execution worker that
// cannot store baselines (it has no secret keys), instead of failing them as
// unknown; a worker that can store them runs them.
func RegisterWaiting(cfg *river.Config) {
	if cfg.Workers == nil {
		cfg.Workers = river.NewWorkers()
	}
	river.AddWorker(cfg.Workers, &waitingWorker{})
}

type waitingWorker struct {
	river.WorkerDefaults[RefreshArgs]
}

func (*waitingWorker) Work(context.Context, *river.Job[RefreshArgs]) error {
	return river.JobSnooze(10 * time.Minute)
}
