package environments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// Request is an immutable snapshot of the PR's desired state when work was requested.
// Only PRKey participates in River uniqueness; a newer generation is retained on
// the PR row until the active worker can transactionally hand off to its successor.
type Request struct {
	PRKey         string `json:"pr_key" river:"unique"`
	PullRequestID int64  `json:"pull_request_id"`
	Generation    int64  `json:"generation"`
	HeadSHA       string `json:"head_sha"`
	State         string `json:"state"`
}

func NewRequest(id, generation int64, sha, state string) Request {
	return Request{PRKey: fmt.Sprintf("environment:pr:%d", id), PullRequestID: id, Generation: generation, HeadSHA: sha, State: state}
}

func (r Request) validate() error {
	if r.PullRequestID <= 0 || r.Generation <= 0 || r.HeadSHA == "" || r.PRKey != fmt.Sprintf("environment:pr:%d", r.PullRequestID) || (r.State != "open" && r.State != "closed" && r.State != "merged") {
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
	pool   *pgxpool.Pool
	client jobInserter
}

func NewBroker(pool *pgxpool.Pool, client jobInserter) *Broker {
	return &Broker{pool: pool, client: client}
}

// EnqueueTx must run in the same transaction as the generation increment.
func (b *Broker) EnqueueTx(ctx context.Context, tx pgx.Tx, request Request) (int64, error) {
	if err := request.validate(); err != nil {
		return 0, err
	}
	var args river.JobArgs = BuildArgs(request)
	if request.State != "open" {
		args = TeardownArgs(request)
	}
	result, err := b.client.InsertTx(ctx, tx, args, nil)
	if err != nil {
		return 0, fmt.Errorf("enqueue PR generation: %w", err)
	}
	if result.Job.Kind != "build" && result.Job.Kind != "teardown" {
		return 0, errors.New("PR job key conflicts with an unrelated job kind")
	}
	return result.Job.ID, nil
}

// finishJob holds the same PR lock as event recording. Completing the old River
// row releases its unique key; inserting the latest request happens in this same
// transaction. An event racing with completion either precedes this handoff or
// sees the completed job and inserts its own successor. No request can be lost.
func finishJob[T river.JobArgs](ctx context.Context, broker *Broker, request Request, job *river.Job[T], workErr error) error {
	if workErr != nil && job.Attempt < job.MaxAttempts {
		return workErr
	}
	err := database.InTransaction(ctx, broker.pool, func(tx pgx.Tx) error {
		latest, err := dbgen.New(tx).LockRequest(ctx, request.PullRequestID)
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
		if latest.Generation > request.Generation {
			_, err := broker.EnqueueTx(ctx, tx, NewRequest(latest.ID, latest.Generation, latest.HeadSha, latest.State))
			return err
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
