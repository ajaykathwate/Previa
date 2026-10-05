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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

const BuildQueue = "build"
const TeardownQueue = "teardown"

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
	if r.PullRequestID <= 0 || r.Generation < 0 || r.Revision < 0 || r.HeadSHA == "" || r.PRKey != fmt.Sprintf("environment:pr:%d", r.PullRequestID) || (r.State != "open" && r.State != "closed" && r.State != "merged") {
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
	enabled         bool
	now             func() time.Time
	recoveryMu      sync.Mutex
	recoveryAfterID int64
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
	owner, err := q.FindPROwner(ctx, request.PullRequestID)
	if err != nil {
		return 0, fmt.Errorf("find enqueue owner: %w", err)
	}
	scope, err := lockIntentScope(ctx, q, owner.Int64, request.PullRequestID)
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
	if request.State == "open" && (i == nil || i.Revision != request.Revision) {
		return 0, nil
	}
	return b.dispatchLockedTx(ctx, tx, scope, i)
}

// finishJob releases the executable PR key and admits only current eligible
// authorized intent in one transaction. A future deadline keeps its separate
// durable wake-up; a new Manual observation alone never creates a successor.
func finishJob[T river.JobArgs](ctx context.Context, broker *Broker, request Request, job *river.Job[T], workErr error) error {
	if workErr != nil && job.Attempt < job.MaxAttempts {
		return workErr
	}
	err := database.InTransaction(ctx, broker.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		owner, err := q.FindPROwner(ctx, request.PullRequestID)
		if err != nil {
			return fmt.Errorf("find handoff owner: %w", err)
		}
		// Locks do not require current access here: revocation must not strand a
		// running River row. The successor admission below rechecks authorization.
		scope, accessErr := lockIntentScope(ctx, q, owner.Int64, request.PullRequestID)
		if accessErr != nil && !errors.Is(accessErr, ErrIntentUnavailable) {
			return accessErr
		}
		_, err = q.LockRequest(ctx, request.PullRequestID)
		if err != nil {
			return fmt.Errorf("lock PR for job handoff: %w", err)
		}
		if workErr != nil {
			if err := discardJob(ctx, broker, tx, job.JobRow, workErr); err != nil {
				return err
			}
		} else {
			if _, err := river.JobCompleteTx[*riverpgxv5.Driver](ctx, tx, job); err != nil {
				return fmt.Errorf("complete PR job: %w", err)
			}
		}
		// A started teardown is recorded whatever the access: it needs none.
		if request.State != "open" && workErr == nil {
			if _, err := q.CompleteTeardownRequest(ctx, dbgen.CompleteTeardownRequestParams{ID: request.PullRequestID, Generation: request.Generation}); err != nil {
				return fmt.Errorf("complete teardown intent: %w", err)
			}
		}
		if accessErr != nil {
			return nil
		}
		if request.State == "open" {
			next := "succeeded"
			if workErr != nil {
				next = "failed"
			}
			if _, err := q.FinishBuildIntent(ctx, dbgen.FinishBuildIntentParams{PrID: request.PullRequestID, Generation: request.Generation, Revision: request.Revision, NextState: next}); err != nil {
				return fmt.Errorf("finish build intent: %w", err)
			}
		}
		current, err := readLockedIntent(ctx, q, request.PullRequestID)
		if err != nil {
			return err
		}
		if _, err := broker.dispatchLockedTx(ctx, tx, scope, current); err != nil {
			return err
		}
		if scope.pr.State == "open" && current != nil && current.State == IntentPending && broker.now().Before(current.AvailableAt) {
			return broker.wakeTx(ctx, tx, NewRequest(scope.pr.ID, scope.pr.Generation, scope.pr.HeadSha, scope.pr.State), current)
		}
		return nil
	})
	return errors.Join(workErr, err)
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
