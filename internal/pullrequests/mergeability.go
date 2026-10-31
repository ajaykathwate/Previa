package pullrequests

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/pullrequests/dbgen"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

type MergeabilityRefresher interface {
	RefreshMergeabilityTx(context.Context, pgx.Tx, int64, int32, ...bool) error
}
type MergeabilitySource interface {
	Get(context.Context, int64, int64, int32) (Observation, error)
}
type MergeabilityArgs struct {
	RepoID     int64  `json:"repo_id"`
	Number     int32  `json:"number"`
	Generation int64  `json:"generation"`
	SHA        string `json:"sha"`
	Authorize  bool   `json:"authorize"`
}

func (MergeabilityArgs) Kind() string { return "pr_mergeability" }
func (MergeabilityArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: SyncQueue, MaxAttempts: 8, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}
func (s *SyncService) RefreshMergeabilityTx(ctx context.Context, tx pgx.Tx, repoID int64, number int32, authorize ...bool) error {
	pr, err := dbgen.New(tx).LockPR(ctx, dbgen.LockPRParams{RepoID: repoID, Number: number})
	if err != nil {
		return fmt.Errorf("read mergeability request: %w", err)
	}
	if pr.State != "open" || pr.BuildStatus != "checking" {
		return nil
	}
	// A new check starts: an earlier one that gave up no longer applies.
	if err := dbgen.New(tx).ClearMergeabilityUnresolved(ctx, pr.ID); err != nil {
		return fmt.Errorf("reset mergeability result: %w", err)
	}
	_, err = s.jobs.InsertTx(ctx, tx, MergeabilityArgs{RepoID: repoID, Number: number, Generation: pr.Generation, SHA: pr.HeadSha, Authorize: len(authorize) > 0 && authorize[0]}, &river.InsertOpts{ScheduledAt: time.Now().Add(2 * time.Second)})
	return err
}

// DiscoverInstallationTx seeds open-only discovery once, atomically with linking
// or adding an accessible repository. Repeated callbacks don't resync history.
func (s *SyncService) DiscoverInstallationTx(ctx context.Context, tx pgx.Tx, installationID int64) error {
	q := dbgen.New(tx)
	repos, err := q.InitialDiscoveryRepositories(ctx, installationID)
	if err != nil {
		return fmt.Errorf("find initial discovery repositories: %w", err)
	}
	for _, id := range repos {
		row, err := q.StartSync(ctx, id)
		if err != nil {
			return err
		}
		if _, err := s.jobs.InsertTx(ctx, tx, SyncArgs{RepoID: id, Revision: row.Revision, Page: 1}, nil); err != nil {
			return err
		}
	}
	return nil
}

var errMergeabilityPending = errors.New("GitHub is still checking mergeability")

// CheckMergeability asks GitHub again whether a PR still being checked is
// mergeable, and records a resolved answer. Like discovery it captures the
// scope, reads GitHub with no transaction open, then records the answer
// unless the PR or scope moved on.
func (s *SyncService) CheckMergeability(ctx context.Context, args MergeabilityArgs) error {
	if args.RepoID <= 0 || args.Number <= 0 || args.Generation < 0 || args.SHA == "" {
		return errors.New("invalid mergeability job identity")
	}
	before, current, err := s.captureChecking(ctx, args)
	if err != nil || !current {
		return err
	}
	source, ok := s.source.(MergeabilitySource)
	if !ok {
		return errors.New("GitHub mergeability source is not configured")
	}
	o, err := source.Get(ctx, before.install.GithubInstallationID, before.repo.GithubRepoID.Int64, args.Number)
	if err != nil {
		return err
	}
	if o.Number != args.Number {
		return errors.New("mergeability response identity changed")
	}
	return database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		return s.recordMergeabilityTx(ctx, tx, args, before, o)
	})
}

// captureChecking reads the scope, and whether the PR is still the job's
// commit with its mergeability being checked.
func (s *SyncService) captureChecking(ctx context.Context, args MergeabilityArgs) (before scope, current bool, err error) {
	err = database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		if before, err = lockScope(ctx, tx, args.RepoID, 0); err != nil {
			return err
		}
		pr, err := dbgen.New(tx).LockPR(ctx, dbgen.LockPRParams{RepoID: args.RepoID, Number: args.Number})
		if err != nil {
			return err
		}
		current = stillChecking(pr, args)
		return nil
	})
	return before, current, err
}

// recordMergeabilityTx records GitHub's resolved answer. The resolved
// snapshot is authoritative and fenced; it may also close a tracked PR. It
// can authorize Automatic work, never an untracked PR.
func (s *SyncService) recordMergeabilityTx(ctx context.Context, tx pgx.Tx, args MergeabilityArgs, before scope, o Observation) error {
	after, err := lockScope(ctx, tx, args.RepoID, 0)
	if err != nil {
		return err
	}
	if !after.unchanged(before) {
		return ErrSnapshotChanged
	}
	q := dbgen.New(tx)
	pr, err := q.LockPR(ctx, dbgen.LockPRParams{RepoID: args.RepoID, Number: args.Number})
	if err != nil {
		return err
	}
	if !stillChecking(pr, args) {
		return nil
	}
	if o.SHA != args.SHA || pr.SourceUpdatedAt.Valid && o.UpdatedAt.Before(pr.SourceUpdatedAt.Time) {
		return ErrSnapshotChanged
	}
	if o.State == environments.StateOpen && MergeabilityStatus(o.Mergeable, o.MergeableState) == "checking" {
		return errMergeabilityPending
	}
	authorized, err := q.HasAutomaticIntent(ctx, dbgen.HasAutomaticIntentParams{PullRequestID: pr.ID, Generation: pr.Generation})
	if err != nil {
		return fmt.Errorf("check existing mergeability authorization: %w", err)
	}
	applied, err := ObserveTx(ctx, tx, args.RepoID, o, Discovered, s.observer)
	if err != nil || !applied {
		return err
	}
	if (!args.Authorize && !authorized) || s.observer == nil {
		return nil
	}
	saved, err := q.LockPR(ctx, dbgen.LockPRParams{RepoID: args.RepoID, Number: args.Number})
	if err != nil {
		return err
	}
	_, err = s.observer.ObserveTx(ctx, tx, environments.NewRequest(saved.ID, saved.Generation, saved.HeadSha, saved.State))
	return err
}

// stillChecking reports whether the PR is still at the job's generation and
// commit, open, with its mergeability being checked.
func stillChecking(pr dbgen.PullRequest, args MergeabilityArgs) bool {
	return pr.Generation == args.Generation && pr.HeadSha == args.SHA && pr.State == environments.StateOpen && pr.BuildStatus == "checking"
}

type MergeabilityWorker struct {
	river.WorkerDefaults[MergeabilityArgs]
	Service *SyncService
}

func (*MergeabilityWorker) Timeout(*river.Job[MergeabilityArgs]) time.Duration { return SyncTimeout }
func (w *MergeabilityWorker) Work(ctx context.Context, job *river.Job[MergeabilityArgs]) error {
	err := w.Service.CheckMergeability(ctx, job.Args)

	if errors.Is(err, ErrUnavailable) {
		return river.JobCancel(ErrUnavailable)
	}
	var rate *RetryAfterError
	if errors.As(err, &rate) {
		return river.JobSnooze(max(time.Second, rate.Delay))
	}
	if err != nil && job.Attempt >= job.MaxAttempts {
		// The last attempt: record that GitHub never answered, so the dashboard
		// stops showing "Checking" and offers Sync Pull Requests instead.
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if markErr := w.Service.MarkMergeabilityUnresolved(saveCtx, job.Args); markErr != nil {
			return errors.Join(err, markErr)
		}
	}
	return err
}

// MarkMergeabilityUnresolved records that the bounded check gave up on exactly
// this observation; a newer commit or check is never marked.
func (s *SyncService) MarkMergeabilityUnresolved(ctx context.Context, args MergeabilityArgs) error {
	if _, err := dbgen.New(s.pool).MarkMergeabilityUnresolved(ctx, dbgen.MarkMergeabilityUnresolvedParams{
		RepoID: args.RepoID, Number: args.Number, Generation: args.Generation, HeadSha: args.SHA}); err != nil {
		return fmt.Errorf("record unresolved mergeability: %w", err)
	}
	return nil
}

// Ordinary pending errors consume the eight-attempt budget; rate limits snooze.
func (*MergeabilityWorker) NextRetry(*river.Job[MergeabilityArgs]) time.Time {
	return time.Now().Add(2 * time.Second)
}
