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
		if _, err := s.jobs.InsertTx(ctx, tx, SyncArgs{id, row.Revision, 1}, nil); err != nil {
			return err
		}
	}
	return nil
}

var errMergeabilityPending = errors.New("GitHub is still checking mergeability")

func (s *SyncService) CheckMergeability(ctx context.Context, args MergeabilityArgs) error {
	if args.RepoID <= 0 || args.Number <= 0 || args.Generation < 0 || args.SHA == "" {
		return errors.New("invalid mergeability job identity")
	}
	var before scope
	current := false
	err := database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		before, err = lockScope(ctx, tx, args.RepoID, 0)
		if err != nil {
			return err
		}
		pr, err := dbgen.New(tx).LockPR(ctx, dbgen.LockPRParams{RepoID: args.RepoID, Number: args.Number})
		if err != nil {
			return err
		}
		current = pr.Generation == args.Generation && pr.HeadSha == args.SHA && pr.State == "open" && pr.BuildStatus == "checking"
		return nil
	})
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
		after, err := lockScope(ctx, tx, args.RepoID, 0)
		if err != nil {
			return err
		}
		if before.install.Revision != after.install.Revision || before.repo.ObservationVersion != after.repo.ObservationVersion {
			return ErrSnapshotChanged
		}
		pr, err := dbgen.New(tx).LockPR(ctx, dbgen.LockPRParams{RepoID: args.RepoID, Number: args.Number})
		if err != nil {
			return err
		}
		if pr.Generation != args.Generation || pr.HeadSha != args.SHA || pr.State != "open" || pr.BuildStatus != "checking" {
			return nil
		}
		if o.SHA != args.SHA || pr.SourceUpdatedAt.Valid && o.UpdatedAt.Before(pr.SourceUpdatedAt.Time) {
			return ErrSnapshotChanged
		}
		if o.State == "open" && MergeabilityStatus(o.Mergeable, o.MergeableState) == "checking" {
			return errMergeabilityPending
		}
		authorized, err := dbgen.New(tx).HasAutomaticIntent(ctx, dbgen.HasAutomaticIntentParams{PullRequestID: pr.ID, Generation: pr.Generation})
		if err != nil {
			return fmt.Errorf("check existing mergeability authorization: %w", err)
		}
		// The resolved snapshot is authoritative and fenced; it may also close a
		// tracked PR. It can authorize Automatic work, never an untracked PR.
		applied, err := ObserveTx(ctx, tx, args.RepoID, o, false, s.observer)
		if err != nil || !applied {
			return err
		}
		if (args.Authorize || authorized) && s.observer != nil {
			saved, err := dbgen.New(tx).LockPR(ctx, dbgen.LockPRParams{RepoID: args.RepoID, Number: args.Number})
			if err != nil {
				return err
			}
			_, err = s.observer.ObserveTx(ctx, tx, environments.NewRequest(saved.ID, saved.Generation, saved.HeadSha, saved.State))
			return err
		}
		return nil
	})
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
	return err
}

// Ordinary pending errors consume the eight-attempt budget; rate limits snooze.
func (*MergeabilityWorker) NextRetry(*river.Job[MergeabilityArgs]) time.Time {
	return time.Now().Add(2 * time.Second)
}
