package onboarding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ajaykathwatee/previa/internal/onboarding/dbgen"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/repoconfig"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

const (
	// CheckQueue runs setup checks; CheckWorkers bounds them per worker.
	CheckQueue   = "setup_checks"
	CheckWorkers = 2
	// checkTimeout bounds one attempt: a handful of small GitHub reads.
	checkTimeout = 45 * time.Second
	// ResultFreshFor is how long a result reads as current before the page
	// suggests checking again (its commit cannot change; the platform can).
	ResultFreshFor = 15 * time.Minute
	// CheckStaleAfter is how long a requested check may wait or run before
	// the page says it did not finish and lets people ask again.
	CheckStaleAfter = 2 * time.Minute
)

// CheckArgs identifies one request; a newer request has a newer revision.
type CheckArgs struct {
	CheckID  int64 `json:"check_id"`
	Revision int64 `json:"revision"`
}

func (CheckArgs) Kind() string { return "repository_setup_check" }
func (CheckArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: CheckQueue, MaxAttempts: 3, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

// PlatformSource says what commits are checked against: the parser's
// platform (services on offer, build deadline, ceilings) and a fingerprint
// that changes whenever any of it, or the check rules, change.
type PlatformSource interface {
	Platform(ctx context.Context) (repoconfig.Platform, string, error)
}

// Files opens a repository at a commit; *GitHubFiles implements it.
type Files interface {
	Commit(ctx context.Context, installationID, repoID int64, sha string) (Tree, error)
}

// Runner performs setup checks on a worker.
type Runner struct {
	pool     *pgxpool.Pool
	files    Files
	platform PlatformSource
	logger   *slog.Logger
}

// NewRunner needs GitHub file access and the platform checks are made against.
func NewRunner(pool *pgxpool.Pool, files Files, platform PlatformSource, logger *slog.Logger) (*Runner, error) {
	if pool == nil || files == nil || platform == nil || logger == nil {
		return nil, errors.New("setup checks need a database, GitHub access, a platform and a logger")
	}
	return &Runner{pool: pool, files: files, platform: platform, logger: logger}, nil
}

// claimed is the scope a running check's result must still match.
type claimed struct {
	installationID, githubRepoID int64
	sha                          string
}

// Run performs one requested check: claim it under lock, read GitHub with no
// transaction open, then record the result only if the request, the pull
// request's head and its access are unchanged. Known GitHub failures are
// recorded for people to retry; on the last attempt anything else is too.
func (r *Runner) Run(ctx context.Context, args CheckArgs, last bool) error {
	if args.CheckID <= 0 || args.Revision <= 0 {
		return errors.New("invalid setup check identity")
	}
	claim, current, err := r.claim(ctx, args)
	if err != nil || !current {
		return err
	}
	platform, fingerprint, err := r.platform.Platform(ctx)
	if err != nil {
		return r.attemptFailed(ctx, args, fmt.Errorf("read the platform for setup checks: %w", err), last)
	}
	attempt, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	tree, err := r.files.Commit(attempt, claim.installationID, claim.githubRepoID, claim.sha)
	var result Result
	if err == nil {
		result, err = Check(attempt, tree, platform)
	}
	if err != nil {
		return r.attemptFailed(ctx, args, err, last)
	}
	return r.complete(ctx, args, result, fingerprint)
}

func (r *Runner) claim(ctx context.Context, args CheckArgs) (c claimed, current bool, err error) {
	err = database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		row, err := q.ClaimCheck(ctx, args.CheckID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // The pull request and its check are gone.
		}
		if err != nil {
			return fmt.Errorf("claim setup check: %w", err)
		}
		if row.Revision != args.Revision || row.State != "queued" && row.State != "running" {
			return nil // A newer request replaced this one.
		}
		if code := scopeProblem(row); code != "" {
			_, err := q.FailCheck(ctx, dbgen.FailCheckParams{ID: args.CheckID, Revision: args.Revision, Code: code})
			return err
		}
		if err := q.StartCheck(ctx, dbgen.StartCheckParams{ID: args.CheckID, Revision: args.Revision}); err != nil {
			return fmt.Errorf("start setup check: %w", err)
		}
		c, current = claimed{installationID: row.GithubInstallationID, githubRepoID: row.GithubRepoID, sha: row.HeadSha}, true
		return nil
	})
	return c, current, err
}

// scopeProblem says why a check no longer describes its pull request.
func scopeProblem(row dbgen.ClaimCheckRow) string {
	switch {
	case row.PrState != "open":
		return FailClosed
	case row.CurrentSha != row.HeadSha || row.CurrentGeneration != row.Generation:
		return FailStaleCommit
	case !row.Accessible || row.InstallationStatus != "active":
		return FailAccess
	case row.GithubRepoID <= 0:
		return FailNotSupported
	}
	return ""
}

// attemptFailed records a classified failure. An unexpected one is retried by
// River until the last attempt, which records it too.
func (r *Runner) attemptFailed(ctx context.Context, args CheckArgs, err error, last bool) error {
	code := failureCode(err)
	if code == FailInternal {
		if !last {
			return err
		}
		r.logger.ErrorContext(ctx, "setup check failed", "check_id", args.CheckID, "error", err)
	}
	if _, failErr := dbgen.New(r.pool).FailCheck(ctx, dbgen.FailCheckParams{ID: args.CheckID, Revision: args.Revision, Code: code}); failErr != nil {
		return errors.Join(err, fmt.Errorf("record setup check failure: %w", failErr))
	}
	return nil
}

// complete records the result, or why it no longer applies.
func (r *Runner) complete(ctx context.Context, args CheckArgs, result Result, fingerprint string) error {
	data, err := encodeResult(result)
	if err != nil {
		return err
	}
	return database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		n, err := q.CompleteCheck(ctx, dbgen.CompleteCheckParams{ID: args.CheckID, Revision: args.Revision, Result: data,
			FreshSeconds: int32(ResultFreshFor / time.Second), Platform: fingerprint})
		if err != nil || n > 0 {
			return err
		}
		row, err := q.ClaimCheck(ctx, args.CheckID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("re-read setup check: %w", err)
		}
		if row.Revision != args.Revision {
			return nil
		}
		code := scopeProblem(row)
		if code == "" {
			code = FailInterrupted
		}
		_, err = q.FailCheck(ctx, dbgen.FailCheckParams{ID: args.CheckID, Revision: args.Revision, Code: code})
		return err
	})
}

// CheckWorker runs setup checks from the queue.
type CheckWorker struct {
	river.WorkerDefaults[CheckArgs]
	Runner *Runner
}

func (*CheckWorker) Timeout(*river.Job[CheckArgs]) time.Duration {
	return checkTimeout + 15*time.Second
}

func (w *CheckWorker) Work(ctx context.Context, job *river.Job[CheckArgs]) error {
	return w.Runner.Run(ctx, job.Args, job.Attempt >= job.MaxAttempts)
}

// Register adds the setup check worker and its queue to a River config.
func Register(rc *river.Config, runner *Runner) {
	river.AddWorker(rc.Workers, &CheckWorker{Runner: runner})
	rc.Queues[CheckQueue] = river.QueueConfig{MaxWorkers: CheckWorkers}
}
