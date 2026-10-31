package sampledata

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/sampledata/dbgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// Refresh triggers (docs/32 §Triggers).
const (
	TriggerEnabled  = "enabled"  // Test data turned on
	TriggerMerged   = "merged"   // a pull request merged into the baseline branch
	TriggerButton   = "button"   // Refresh baseline
	TriggerDaily    = "daily"    // the daily check found the branch moved
	TriggerUnusable = "unusable" // a baseline file could not be read
	TriggerBranch   = "branch"   // another baseline branch was chosen
)

// automatic triggers are spaced by the minimum interval; the others follow a
// person's action and start at once.
func automatic(trigger string) bool {
	return trigger == TriggerMerged || trigger == TriggerDaily || trigger == TriggerUnusable
}

// Refresh states.
const (
	StatePending   = "pending"
	StateRunning   = "running"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
)

// Settings is a repository's Test data configuration. Without a saved row,
// Test data is off with the documented defaults.
type Settings struct {
	Enabled, KeepData, UseBaseline, AutoRefresh bool
	Branch                                      string // "" follows the GitHub default branch
	Revision                                    int64  // 0 before the first save
	UpdatedBy                                   int64
	UpdatedAt                                   time.Time
	// The latest look at the branch head, by a refresh or the daily check.
	CheckedBranch, CheckedSHA string
	CheckedAt                 time.Time
}

// DefaultSettings are a repository's settings before anyone saves them.
func DefaultSettings() Settings {
	return Settings{KeepData: true, UseBaseline: true, AutoRefresh: true}
}

func settingsOf(row dbgen.RepositorySampledataSetting) Settings {
	return Settings{
		Enabled: row.Enabled, KeepData: row.KeepData, UseBaseline: row.UseBaseline, AutoRefresh: row.AutoRefresh,
		Branch: row.BaselineBranch.String, Revision: row.Revision, UpdatedBy: row.UpdatedBy, UpdatedAt: row.UpdatedAt.Time,
		CheckedBranch: row.CheckedBranch.String, CheckedSHA: row.CheckedSha.String, CheckedAt: row.CheckedAt.Time,
	}
}

// Baseline is one stored baseline of a SQL service.
type Baseline struct {
	ID, RepoID, RefreshID, Sequence int64
	Kind, Version                   string
	Branch, CommitSHA, ConfigHash   string
	StorageKey, Checksum            string
	Size                            int64
	KeyVersion                      int32
	Status                          string
	BuiltAt                         time.Time
}

func baselineOf(row dbgen.RepositoryBaseline) Baseline {
	return Baseline{
		ID: row.ID, RepoID: row.RepoID, RefreshID: row.RefreshID.Int64, Sequence: row.RefreshSequence, Kind: row.ServiceKind, Version: row.ServiceVersion,
		Branch: row.Branch, CommitSHA: row.CommitSha, ConfigHash: row.ConfigHash.String, StorageKey: row.StorageKey, Checksum: row.Checksum,
		Size: row.SizeBytes, KeyVersion: row.KeyVersion, Status: row.Status, BuiltAt: row.BuiltAt.Time,
	}
}

// JobInserter inserts River jobs in a transaction and moves a scheduled one
// forward; *river.Client implements it.
type JobInserter interface {
	InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
	JobRetryTx(ctx context.Context, tx pgx.Tx, id int64) (*rivertype.JobRow, error)
}

// RefreshArgs runs a repository's baseline refreshes: one job per
// repository, which runs the waiting refresh and then waits for the next one.
// It runs on the build queue, so baseline builds share the worker's build
// concurrency.
type RefreshArgs struct {
	RepoID int64 `json:"repo_id" river:"unique"`
}

func (RefreshArgs) Kind() string { return "baseline_refresh" }

// Store keeps the settings, refreshes and baselines in PostgreSQL.
type Store struct {
	pool  *pgxpool.Pool
	jobs  JobInserter
	queue string
	now   func() time.Time
}

// NewStore takes the queue refreshes run on (the build queue). jobs may be
// nil: requests are then recorded and a worker's dispatcher starts them.
func NewStore(pool *pgxpool.Pool, jobs JobInserter, queue string) (*Store, error) {
	if pool == nil || queue == "" {
		return nil, errors.New("test data requires a database and a refresh queue")
	}
	return &Store{pool: pool, jobs: jobs, queue: queue, now: time.Now}, nil
}

// Settings returns a repository's settings, or the defaults.
func (s *Store) Settings(ctx context.Context, repoID int64) (Settings, error) {
	row, err := dbgen.New(s.pool).GetSettings(ctx, repoID)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultSettings(), nil
	}
	if err != nil {
		return Settings{}, fmt.Errorf("read test data settings: %w", err)
	}
	return settingsOf(row), nil
}

// Current returns a repository's current baselines.
func (s *Store) Current(ctx context.Context, repoID int64) ([]Baseline, error) {
	rows, err := dbgen.New(s.pool).CurrentBaselines(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("read baselines: %w", err)
	}
	baselines := make([]Baseline, len(rows))
	for i, row := range rows {
		baselines[i] = baselineOf(row)
	}
	return baselines, nil
}

// Plan is what a candidate's databases may start from: the settings in
// effect and the current baselines (none unless baselines are used).
func (s *Store) Plan(ctx context.Context, repoID int64) (Settings, []Baseline, error) {
	settings, err := s.Settings(ctx, repoID)
	if err != nil || !settings.Enabled {
		return settings, nil, err
	}
	if !settings.UseBaseline {
		return settings, nil, nil
	}
	baselines, err := s.Current(ctx, repoID)
	return settings, baselines, err
}

// requestTx asks for a refresh. The caller holds the repository's row lock.
// A request joins the refresh already waiting; otherwise a new one waits
// (one more may wait while another runs). The worker spaces automatic
// refreshes when it starts them.
func (s *Store) requestTx(ctx context.Context, tx pgx.Tx, repoID int64, trigger string, actor int64) error {
	q := dbgen.New(tx)
	available := s.now()
	requestedBy := pgtype.Int8{Int64: actor, Valid: actor > 0}
	pending, err := q.PendingRefresh(ctx, repoID)
	switch {
	case err == nil:
		if err := q.HastenRefresh(ctx, dbgen.HastenRefreshParams{ID: pending.ID, AvailableAt: pgtype.Timestamptz{Time: available, Valid: true}, Trigger: trigger, RequestedBy: requestedBy}); err != nil {
			return fmt.Errorf("join the waiting refresh: %w", err)
		}
	case errors.Is(err, pgx.ErrNoRows):
		sequence, err := q.NextSequence(ctx, repoID)
		if err != nil {
			return fmt.Errorf("number the refresh: %w", err)
		}
		if _, err := q.InsertRefresh(ctx, dbgen.InsertRefreshParams{RepoID: repoID, Sequence: sequence, Trigger: trigger, RequestedBy: requestedBy,
			AvailableAt: pgtype.Timestamptz{Time: available, Valid: true}}); err != nil {
			return fmt.Errorf("request a refresh: %w", err)
		}
	default:
		return fmt.Errorf("read the waiting refresh: %w", err)
	}
	return s.enqueueTx(ctx, tx, repoID, available)
}

// enqueueTx makes sure the repository's refresh job runs by at. There is one
// job per repository: a running job takes the next refresh when it finishes,
// and a job waiting until later is moved forward when a refresh is due now.
func (s *Store) enqueueTx(ctx context.Context, tx pgx.Tx, repoID int64, at time.Time) error {
	if s.jobs == nil {
		return nil
	}
	inserted, err := s.jobs.InsertTx(ctx, tx, RefreshArgs{RepoID: repoID}, &river.InsertOpts{
		Queue: s.queue, ScheduledAt: at,
		UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
			rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled,
		}},
	})
	if err != nil {
		return fmt.Errorf("schedule the baseline refresh: %w", err)
	}
	job := inserted.Job
	if !inserted.UniqueSkippedAsDuplicate || job == nil || at.After(s.now()) || !job.ScheduledAt.After(at) ||
		job.State != rivertype.JobStateScheduled && job.State != rivertype.JobStateRetryable {
		return nil
	}
	if _, err := s.jobs.JobRetryTx(ctx, tx, job.ID); err != nil {
		return fmt.Errorf("start the baseline refresh now: %w", err)
	}
	return nil
}

// RequestTx asks for a refresh within the caller's transaction, after
// locking the repository; for triggers outside this feature (a merge).
func (s *Store) RequestTx(ctx context.Context, tx pgx.Tx, repoID int64, trigger string, actor int64) error {
	if _, err := dbgen.New(tx).LockRepository(ctx, repoID); err != nil {
		return fmt.Errorf("lock the repository: %w", err)
	}
	return s.requestTx(ctx, tx, repoID, trigger, actor)
}

// Request asks for a refresh in its own transaction.
func (s *Store) Request(ctx context.Context, repoID int64, trigger string, actor int64) error {
	return database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		return s.RequestTx(ctx, tx, repoID, trigger, actor)
	})
}

// Unusable marks a baseline whose file could not be read and asks for a
// refresh; candidates fall back meanwhile.
func (s *Store) Unusable(ctx context.Context, repoID, baselineID int64) error {
	return database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, err := q.LockRepository(ctx, repoID); err != nil {
			return fmt.Errorf("lock the repository: %w", err)
		}
		marked, err := q.MarkBaselineMissing(ctx, baselineID)
		if err != nil {
			return fmt.Errorf("mark the baseline missing: %w", err)
		}
		settings, err := q.GetSettings(ctx, repoID)
		if marked == 0 || err != nil || !settings.Enabled || !settings.UseBaseline || !settings.AutoRefresh {
			return nil // without automatic refresh, the tab offers Refresh baseline
		}
		return s.requestTx(ctx, tx, repoID, TriggerUnusable, 0)
	})
}

// MergedTx asks for a refresh when a pull request merged into the
// repository's baseline branch: its chosen branch, or else the GitHub default
// branch the event names. Only with automatic refresh on. It runs in the
// webhook delivery's transaction, which already holds the repository's lock.
func (s *Store) MergedTx(ctx context.Context, tx pgx.Tx, installationID, githubRepoID int64, base, defaultBranch string) error {
	row, err := dbgen.New(tx).MergeTarget(ctx, dbgen.MergeTargetParams{GithubRepoID: githubRepoID, InstallationID: installationID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read test data settings: %w", err)
	}
	branch := row.BaselineBranch
	if branch == "" {
		branch = defaultBranch
	}
	if !row.Enabled || !row.UseBaseline || !row.AutoRefresh || branch == "" || base != branch {
		return nil
	}
	return s.RequestTx(ctx, tx, row.RepoID, TriggerMerged, 0)
}
