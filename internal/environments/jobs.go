package environments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments/dbgen"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

const BuildQueue = "build"
const TeardownQueue = "teardown"

// PR states a Request carries, as the pull_requests table stores them.
const (
	StateOpen   = "open"
	StateClosed = "closed"
	StateMerged = "merged"
)

// Request captures observed state plus the authorized intent revision. Only PRKey
// participates in executable River uniqueness. Newer eligible intent is handed
// off atomically; future deadlines live in separate coordination wake-ups.
type Request struct {
	PRKey         string `json:"pr_key" river:"unique"`
	PullRequestID int64  `json:"pull_request_id"`
	Generation    int64  `json:"generation"`
	Revision      int64  `json:"revision"`
	HeadSHA       string `json:"head_sha"`
	State         string `json:"state"`
}

func NewRequest(id, generation int64, sha, state string) Request {
	return Request{PRKey: fmt.Sprintf("environment:pr:%d", id), PullRequestID: id, Generation: generation, HeadSHA: sha, State: state}
}

func (r Request) validate() error {
	if r.PullRequestID <= 0 || r.Generation < 0 || r.Revision < 0 || r.HeadSHA == "" || r.PRKey != fmt.Sprintf("environment:pr:%d", r.PullRequestID) || (r.State != StateOpen && r.State != StateClosed && r.State != StateMerged) {
		return errors.New("invalid environment job request")
	}
	return nil
}

type BuildArgs Request

func (BuildArgs) Kind() string { return "build" }
func (BuildArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: BuildQueue, MaxAttempts: 5, UniqueOpts: queue.ActivePRUniqueOpts()}
}

type TeardownArgs Request

func (TeardownArgs) Kind() string { return "teardown" }
func (TeardownArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: TeardownQueue, MaxAttempts: 5, UniqueOpts: queue.ActivePRUniqueOpts()}
}

type jobInserter interface {
	InsertTx(context.Context, pgx.Tx, river.JobArgs, *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// Broker retains one active job per PR without losing later webhook requests.
type Broker struct {
	pool            *pgxpool.Pool
	client          jobInserter
	enabled         bool        // dispatches and admits: real execution adapters
	requests        bool        // records owner build requests (the server)
	reports         *Repository // reporting enabled: records report work
	now             func() time.Time
	recoveryMu      sync.Mutex
	recoveryAfterID int64
}

// WithReports makes the broker's observations, requests and dispatches record
// report work through a repository built WithReports.
func (b *Broker) WithReports(repo *Repository) *Broker {
	b.reports = repo
	return b
}

func (b *Broker) reportTx(ctx context.Context, tx pgx.Tx, pullRequestID int64) error {
	if b.reports == nil {
		return nil
	}
	return b.reports.reportTx(ctx, tx, pullRequestID)
}

// RecordReportTx records, in tx, that the PR's GitHub report must catch up
// with a change recorded elsewhere, such as a refreshed PR observation. It
// does nothing unless reports are enabled.
func (b *Broker) RecordReportTx(ctx context.Context, tx pgx.Tx, pullRequestID int64) error {
	return b.reportTx(ctx, tx, pullRequestID)
}

func NewBroker(pool *pgxpool.Pool, client jobInserter) *Broker {
	return &Broker{pool: pool, client: client, now: time.Now}
}

// EnqueueTx admits only current authorized due work; observations use ObserveTx.
func (b *Broker) EnqueueTx(ctx context.Context, tx pgx.Tx, request Request) (int64, error) {
	if err := request.validate(); err != nil {
		return 0, err
	}
	if !b.enabled {
		return 0, ErrExecutionUnavailable
	}
	q := dbgen.New(tx)
	scope, err := lockOwnerIntentScope(ctx, q, request.PullRequestID, "enqueue")
	if err != nil {
		return 0, err
	}
	i, err := readLockedIntent(ctx, q, request.PullRequestID)
	if err != nil {
		return 0, err
	}
	if scope.pr.Generation != request.Generation || scope.pr.HeadSha != request.HeadSHA || scope.pr.State != request.State {
		return 0, nil
	}
	if request.State == StateOpen && (i == nil || i.Revision != request.Revision) {
		return 0, nil
	}
	return b.dispatchLockedTx(ctx, tx, scope, i)
}

// finishJob releases the executable PR key and admits only current eligible
// authorized intent in one transaction. A future deadline keeps its separate
// durable wake-up; a new Manual observation alone never creates a successor.
func finishJob[T river.JobArgs](ctx context.Context, broker *Broker, request Request, job *river.Job[T], workErr error) error {
	if workErr != nil && job.Attempt < job.MaxAttempts && !permanent(workErr) {
		return workErr
	}
	err := database.InTransaction(ctx, broker.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		// Locks do not require current access here: revocation must not strand a
		// running River row. The successor admission below rechecks authorization.
		scope, accessErr := lockOwnerIntentScope(ctx, q, request.PullRequestID, "handoff")
		if accessErr != nil && !errors.Is(accessErr, ErrIntentUnavailable) {
			return accessErr
		}
		if _, err := q.LockRequest(ctx, request.PullRequestID); err != nil {
			return fmt.Errorf("lock PR for job handoff: %w", err)
		}
		if err := settleJobTx(ctx, broker, tx, job, workErr); err != nil {
			return err
		}
		// A started teardown is recorded whatever the access: it needs none.
		if request.State != StateOpen && workErr == nil {
			if _, err := q.CompleteTeardownRequest(ctx, dbgen.CompleteTeardownRequestParams{ID: request.PullRequestID, Generation: request.Generation}); err != nil {
				return fmt.Errorf("complete teardown intent: %w", err)
			}
		}
		if accessErr != nil {
			return nil
		}
		if request.State == StateOpen {
			if err := finishBuildIntentTx(ctx, broker, tx, request, workErr); err != nil {
				return err
			}
		}
		if err := broker.handOffTx(ctx, tx, scope); err != nil {
			return err
		}
		if request.State == StateOpen {
			return wakeNextWaitingTx(ctx, broker, tx)
		}
		return nil
	})
	return errors.Join(workErr, err)
}

// settleJobTx completes the River job, or discards it after its final failure.
func settleJobTx[T river.JobArgs](ctx context.Context, broker *Broker, tx pgx.Tx, job *river.Job[T], workErr error) error {
	if workErr != nil {
		return discardJob(ctx, broker, tx, job.JobRow, workErr)
	}
	if _, err := river.JobCompleteTx[*riverpgxv5.Driver](ctx, tx, job); err != nil {
		return fmt.Errorf("complete PR job: %w", err)
	}
	return nil
}

// finishBuildIntentTx ends the request's build intent as succeeded or failed.
// A final failure is reported only now, after the last attempt.
func finishBuildIntentTx(ctx context.Context, broker *Broker, tx pgx.Tx, request Request, workErr error) error {
	next := string(IntentSucceeded)
	if workErr != nil {
		next = string(IntentFailed)
	}
	q := dbgen.New(tx)
	finished, err := q.FinishBuildIntent(ctx, dbgen.FinishBuildIntentParams{PrID: request.PullRequestID, Generation: request.Generation, Revision: request.Revision, NextState: next})
	if err != nil {
		return fmt.Errorf("finish build intent: %w", err)
	}
	if finished == 1 {
		// The account's build time, for its daily budget (BUILD_MINUTES_PER_ACCOUNT_DAY).
		if err := q.RecordFinishedBuildTime(ctx, dbgen.RecordFinishedBuildTimeParams{PrID: request.PullRequestID, Revision: request.Revision}); err != nil {
			return fmt.Errorf("record the account's build time: %w", err)
		}
	}
	return broker.reportTx(ctx, tx, request.PullRequestID)
}

// wakeNextWaitingTx hands a freed build slot to the request whose turn is
// next (an account with nothing building first, then the longest wait):
// its snoozed job runs now instead of at the end of its backoff, so a slot
// never sits idle while others wait. Admission still decides; waking only
// asks the next request to look again.
func wakeNextWaitingTx(ctx context.Context, broker *Broker, tx pgx.Tx) error {
	next, err := dbgen.New(tx).NextWaitingBuild(ctx, pgtype.Timestamptz{Time: broker.now().Add(-liveWait), Valid: true})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("find the next waiting build: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE river_job SET scheduled_at = clock_timestamp()
		WHERE kind = 'build' AND state = 'scheduled' AND (args->>'pull_request_id')::bigint = $1`, next); err != nil {
		return fmt.Errorf("wake the next waiting build: %w", err)
	}
	return nil
}

// handOffTx admits the PR's current eligible intent as the successor job, or
// schedules a wake-up for a pending intent whose deadline is still ahead.
func (b *Broker) handOffTx(ctx context.Context, tx pgx.Tx, scope intentScope) error {
	current, err := readLockedIntent(ctx, dbgen.New(tx), scope.pr.ID)
	if err != nil {
		return err
	}
	if _, err := b.dispatchLockedTx(ctx, tx, scope, current); err != nil {
		return err
	}
	if scope.pr.State == StateOpen && current != nil && current.State == IntentPending && b.now().Before(current.AvailableAt) {
		return b.wakeTx(ctx, tx, NewRequest(scope.pr.ID, scope.pr.Generation, scope.pr.HeadSha, scope.pr.State), current)
	}
	return nil
}

// Terminal failures need the same handoff as successful/stale completions.
// Otherwise a new event between the last error and River's discard would be
// deduplicated against a job that will never run again. Use the pinned driver's
// conditional state transition, rather than an unguarded UPDATE of River tables.
func discardJob(ctx context.Context, broker *Broker, tx pgx.Tx, job *rivertype.JobRow, workErr error) error {
	now := time.Now()
	data, err := json.Marshal(rivertype.AttemptError{At: now, Attempt: job.Attempt, Error: workErr.Error()})
	if err != nil {
		return fmt.Errorf("encode terminal job error: %w", err)
	}
	p := riverdriver.JobSetStateDiscarded(job.ID, now, data, nil)
	exec := riverpgxv5.New(broker.pool).UnwrapExecutor(tx)
	_, err = exec.JobSetStateIfRunningMany(ctx, &riverdriver.JobSetStateIfRunningManyParams{
		ID: []int64{p.ID}, Attempt: []*int{&job.Attempt}, ErrData: [][]byte{p.ErrData},
		FinalizedAt: []*time.Time{p.FinalizedAt}, MetadataDoMerge: []bool{false}, MetadataUpdates: [][]byte{nil},
		ScheduledAt: []*time.Time{nil}, State: []rivertype.JobState{p.State},
	})
	if err != nil {
		return fmt.Errorf("discard terminal PR job: %w", err)
	}
	return nil
}
