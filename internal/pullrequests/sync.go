package pullrequests

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/pullrequests/dbgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

const SyncQueue = "pr_discovery"
const SyncTimeout = 45 * time.Second

// SyncStaleAfter is how long a requested sync may stay queued or running
// before the dashboard calls it stalled and Sync Pull Requests starts a new one.
// A live worker finishes a page within seconds; a sync this old never ran
// (no worker) or was lost.
const SyncStaleAfter = 5 * time.Minute

// syncStale reports whether an unfinished sync has outlived SyncStaleAfter.
func syncStale(row dbgen.PrSync, now time.Time) bool {
	return (row.State == "queued" || row.State == "syncing") && row.RequestedAt.Valid && now.Sub(row.RequestedAt.Time) > SyncStaleAfter
}

type SyncArgs struct {
	RepoID   int64 `json:"repo_id"`
	Revision int64 `json:"revision"`
	Page     int32 `json:"page"`
}

func (SyncArgs) Kind() string { return "pr_discovery" }
func (SyncArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: SyncQueue, MaxAttempts: 8, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

type jobInserter interface {
	InsertTx(context.Context, pgx.Tx, river.JobArgs, *river.InsertOpts) (*rivertype.JobInsertResult, error)
}
type Source interface {
	Page(context.Context, int64, int64, int32) ([]Observation, int32, error)
}
type SyncService struct {
	pool     *pgxpool.Pool
	jobs     jobInserter
	source   Source
	observer Observer
}

func NewSyncService(pool *pgxpool.Pool, jobs jobInserter, source Source, observers ...Observer) *SyncService {
	s := &SyncService{pool: pool, jobs: jobs, source: source}
	if len(observers) > 0 {
		s.observer = observers[0]
	}
	return s
}
func (s *SyncService) Request(ctx context.Context, userID, repoID int64) error {
	if userID <= 0 || repoID <= 0 {
		return ErrUnavailable
	}
	return database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := lockScope(ctx, tx, repoID, userID); err != nil {
			return err
		}
		q := dbgen.New(tx)
		old, err := q.LockSync(ctx, repoID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// A sync in progress is not requested twice; a stalled one is replaced
		// (its new revision makes the old job's pages stale).
		if err == nil && (old.State == "queued" || old.State == "syncing") && !syncStale(old, time.Now()) {
			return nil
		}
		row, err := q.StartSync(ctx, repoID)
		if err != nil {
			return err
		}
		_, err = s.jobs.InsertTx(ctx, tx, SyncArgs{repoID, row.Revision, 1}, nil)
		return err
	})
}
func (s *SyncService) RunPage(ctx context.Context, args SyncArgs) error {
	if args.RepoID <= 0 || args.Revision <= 0 || args.Page <= 0 {
		return errors.New("invalid discovery job identity")
	}
	var before scope
	current := false
	err := database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		before, err = lockScope(ctx, tx, args.RepoID, 0)
		if err != nil {
			return err
		}
		q := dbgen.New(tx)
		row, err := q.LockSync(ctx, args.RepoID)
		if err != nil {
			return err
		}
		if row.Revision != args.Revision || row.NextPage != args.Page || row.State == "complete" {
			return nil
		}
		current = true
		return q.MarkSyncing(ctx, dbgen.MarkSyncingParams{RepoID: args.RepoID, Revision: args.Revision})
	})
	if err != nil {
		return err
	}
	if !current {
		return nil
	}
	if s.source == nil {
		return errors.New("GitHub App discovery credentials are not configured")
	}
	// Bounded external IO happens with no database transaction open.
	observations, next, err := s.source.Page(ctx, before.install.GithubInstallationID, before.repo.GithubRepoID.Int64, args.Page)
	if err != nil {
		return err
	}
	if len(observations) > 100 || next < 0 || next != 0 && next <= args.Page {
		return errors.New("invalid discovery pagination")
	}
	return database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		after, err := lockScope(ctx, tx, args.RepoID, 0)
		if err != nil {
			return err
		}
		q := dbgen.New(tx)
		row, err := q.LockSync(ctx, args.RepoID)
		if err != nil {
			return err
		}
		if row.Revision != args.Revision || row.NextPage != args.Page || row.State == "complete" {
			return nil
		}
		if after.install.ID != before.install.ID || after.install.Revision != before.install.Revision || after.repo.ObservationVersion != before.repo.ObservationVersion {
			return ErrSnapshotChanged
		}
		stored := int32(0)
		for _, o := range observations {
			if o.State != "open" {
				continue
			}
			if _, err := ObserveTx(ctx, tx, args.RepoID, o, false, s.observer); err != nil {
				return err
			}
			stored++
			if err := s.RefreshMergeabilityTx(ctx, tx, args.RepoID, o.Number); err != nil {
				return err
			}
		}
		state := "syncing"
		storedNext := next
		if next == 0 {
			state = "complete"
			storedNext = args.Page
		}
		if err := q.FinishSyncPage(ctx, dbgen.FinishSyncPageParams{RepoID: args.RepoID, Revision: args.Revision, NextPage: storedNext, Fetched: stored, State: state}); err != nil {
			return err
		}
		if next > 0 {
			_, err = s.jobs.InsertTx(ctx, tx, SyncArgs{args.RepoID, args.Revision, next}, nil)
		}
		return err
	})
}

type SyncWorker struct {
	river.WorkerDefaults[SyncArgs]
	Service *SyncService
}

func (*SyncWorker) Timeout(*river.Job[SyncArgs]) time.Duration { return SyncTimeout }
func (w *SyncWorker) Work(ctx context.Context, job *river.Job[SyncArgs]) error {
	err := w.Service.RunPage(ctx, job.Args)
	if err == nil {
		return nil
	}
	var rate *RetryAfterError
	if errors.As(err, &rate) {
		return river.JobSnooze(max(time.Second, rate.Delay))
	}
	if errors.Is(err, ErrSnapshotChanged) {
		return river.JobSnooze(2 * time.Second)
	}
	message := "GitHub refresh failed. Check App permissions and worker configuration, then retry."
	if errors.Is(err, ErrUnavailable) {
		message = "Repository access changed. Restore access before refreshing."
	}
	// Persist failure even if the bounded job context expired. Never save provider
	// bodies, tokens or private request URLs in River errors or user-facing state.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if saveErr := dbgen.New(w.Service.pool).FailSync(saveCtx, dbgen.FailSyncParams{RepoID: job.Args.RepoID, Revision: job.Args.Revision, NextPage: job.Args.Page, Message: message}); saveErr != nil {
		return fmt.Errorf("record discovery failure: %w", saveErr)
	}
	if errors.Is(err, ErrUnavailable) {
		return river.JobCancel(errors.New(message))
	}
	return errors.New(message)
}
