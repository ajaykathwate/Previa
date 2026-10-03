package environments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments/dbgen"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

const QuietPeriod = 5 * time.Minute
const CoordinationQueue = "environment_coordination"

var ErrExecutionUnavailable = errors.New("preview execution adapters are not configured")

// WakeArgs has a different uniqueness namespace from executable PR jobs. Every
// immutable revision/deadline has its own wake-up; old wake-ups are harmless.
type WakeArgs struct {
	Key           string `json:"wake_key" river:"unique"`
	PullRequestID int64  `json:"pull_request_id"`
	Generation    int64  `json:"generation"`
	Revision      int64  `json:"revision"`
}

func (WakeArgs) Kind() string { return "environment_wake" }
func (WakeArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: CoordinationQueue, MaxAttempts: 5, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled}}}
}

// NewExecutionBroker is used only with real execution dependencies (test adapters
// in integration tests). The production app does not construct this broker yet.
// All consuming processes must register build, teardown, wake and operation workers.
func NewExecutionBroker(pool *pgxpool.Pool, client jobInserter, service *Service, operations *OperationWorker, now func() time.Time) (*Broker, error) {
	if pool == nil || client == nil || service == nil || service.runtime == nil || service.repo == nil || service.repo.operations == nil || operations == nil || operations.executor == nil {
		return nil, ErrExecutionUnavailable
	}
	b := NewBroker(pool, client)
	b.enabled = true
	if now != nil {
		b.now = now
	}
	return b, nil
}

// ObserveTx is composed with the locked PR event update; no external IO occurs.
func (b *Broker) ObserveTx(ctx context.Context, tx pgx.Tx, request Request) (Request, error) {
	owner, err := dbgen.New(tx).FindPROwner(ctx, request.PullRequestID)
	if err != nil {
		return Request{}, fmt.Errorf("find PR owner: %w", err)
	}
	outcome, err := NewIntentRepository(b.pool).ObserveIntentTx(ctx, tx, owner.Int64, request.PullRequestID, b.now().Add(QuietPeriod))
	if err != nil {
		return Request{}, err
	}
	if outcome.Intent != nil {
		request.Revision = outcome.Intent.Revision
	}
	if outcome.TeardownRequired || outcome.Intent != nil {
		if err := b.wakeTx(ctx, tx, request, outcome.Intent); err != nil {
			return Request{}, err
		}
	}
	return request, nil
}

func (b *Broker) wakeTx(ctx context.Context, tx pgx.Tx, request Request, intent *BuildIntent) error {
	at := b.now()
	if request.State == "open" {
		if intent == nil || (intent.State != IntentPending && intent.State != IntentQueued && intent.State != IntentRunning) {
			return nil
		}
		request.Revision = intent.Revision
		at = intent.AvailableAt
	}
	args := WakeArgs{Key: fmt.Sprintf("wake:pr:%d:g:%d:r:%d", request.PullRequestID, request.Generation, request.Revision), PullRequestID: request.PullRequestID, Generation: request.Generation, Revision: request.Revision}
	if _, err := b.client.InsertTx(ctx, tx, args, &river.InsertOpts{ScheduledAt: at}); err != nil {
		return fmt.Errorf("schedule intent wake-up: %w", err)
	}
	return nil
}

// RequestBuild commits manual authorization and the wake-up together. The
// outer scope locks are reentrant in RequestIntentTx; lock order never reverses.
func (b *Broker) RequestBuild(ctx context.Context, command IntentCommand) (outcome IntentOutcome, err error) {
	if !b.enabled {
		return outcome, ErrExecutionUnavailable
	}
	if err := command.validate(); err != nil {
		return outcome, err
	}
	err = database.InTransaction(ctx, b.pool, func(tx pgx.Tx) error {
		var e error
		outcome, e = NewIntentRepository(b.pool).requestIntentTx(ctx, tx, command, b.now())
		if e != nil {
			return e
		}
		i := outcome.Intent
		r := NewRequest(i.PullRequestID, i.Generation, i.HeadSHA, "open")
		r.Revision = i.Revision
		return b.wakeTx(ctx, tx, r, &i)
	})
	if err != nil {
		return IntentOutcome{}, err
	}
	return outcome, nil
}

// Dispatch rechecks the immutable wake-up under the ownership/policy locks.
// Scheduled wake-ups never own the build/teardown uniqueness slot.
func (b *Broker) Dispatch(ctx context.Context, wake WakeArgs) error {
	if wake.PullRequestID <= 0 || wake.Generation < 0 || wake.Revision < 0 || wake.Key != fmt.Sprintf("wake:pr:%d:g:%d:r:%d", wake.PullRequestID, wake.Generation, wake.Revision) {
		return errors.New("invalid coordination wake-up")
	}
	if !b.enabled {
		return ErrExecutionUnavailable
	}
	return database.InTransaction(ctx, b.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		owner, err := q.FindPROwner(ctx, wake.PullRequestID)
		if err != nil {
			return fmt.Errorf("find dispatch owner: %w", err)
		}
		scope, err := lockIntentScope(ctx, q, owner.Int64, wake.PullRequestID)
		if errors.Is(err, ErrIntentUnavailable) {
			return nil
		}
		if err != nil {
			return err
		}
		i, err := readLockedIntent(ctx, q, wake.PullRequestID)
		if err != nil {
			return err
		}
		if scope.pr.Generation != wake.Generation {
			return nil
		}
		if scope.pr.State == "open" && (i == nil || i.Revision != wake.Revision) {
			return nil
		}
		_, err = b.dispatchLockedTx(ctx, tx, scope, i)
		return err
	})
}

func (b *Broker) dispatchLockedTx(ctx context.Context, tx pgx.Tx, scope intentScope, i *BuildIntent) (int64, error) {
	if !b.enabled {
		return 0, ErrExecutionUnavailable
	}
	q := dbgen.New(tx)
	r := NewRequest(scope.pr.ID, scope.pr.Generation, scope.pr.HeadSha, scope.pr.State)
	if r.State == "open" {
		if i == nil || i.Generation != r.Generation || i.HeadSHA != r.HeadSHA || (i.State != IntentPending && i.State != IntentQueued && i.State != IntentRunning) || (i.Source == "automatic" && scope.mode != "automatic" && i.State != IntentRunning) {
			return 0, nil
		}
		if b.now().Before(i.AvailableAt) {
			return 0, nil
		}
		r.Revision = i.Revision
	} else {
		pr, err := q.LockSchedulingPR(ctx, r.PullRequestID)
		if err != nil {
			return 0, fmt.Errorf("read teardown eligibility: %w", err)
		}
		if pr.TeardownCompletedGeneration >= pr.Generation {
			return 0, nil
		}
	}
	var args river.JobArgs = BuildArgs(r)
	if r.State != "open" {
		args = TeardownArgs(r)
	}
	result, err := b.client.InsertTx(ctx, tx, args, nil)
	if err != nil {
		return 0, fmt.Errorf("enqueue authorized PR request: %w", err)
	}
	if result.Job.Kind != "build" && result.Job.Kind != "teardown" {
		return 0, errors.New("PR job key conflicts with unrelated work")
	}
	var captured Request
	if err := json.Unmarshal(result.Job.EncodedArgs, &captured); err != nil {
		return 0, fmt.Errorf("decode admitted request: %w", err)
	}
	if captured.Generation == r.Generation && captured.Revision == r.Revision && captured.State == r.State && r.State == "open" {
		if _, err := q.QueueCurrentIntent(ctx, dbgen.QueueCurrentIntentParams{PullRequestID: r.PullRequestID, Revision: r.Revision}); err != nil {
			return 0, fmt.Errorf("mark admitted intent queued: %w", err)
		}
	}
	return result.Job.ID, nil
}

// RecoverScheduling recreates bounded wake-ups from durable current state. It is
// safe after job deletion, crash rescue exhaustion, or a scheduler restart.
func (b *Broker) RecoverScheduling(ctx context.Context, batchSize int32) (int, error) {
	if batchSize <= 0 {
		return 0, errors.New("scheduling recovery batch size must be positive")
	}
	b.recoveryMu.Lock()
	defer b.recoveryMu.Unlock()
	rows, err := dbgen.New(b.pool).ListSchedulingCandidates(ctx, dbgen.ListSchedulingCandidatesParams{AfterID: b.recoveryAfterID, BatchSize: batchSize})
	if err != nil {
		return 0, fmt.Errorf("list scheduling recovery: %w", err)
	}
	if len(rows) == 0 {
		b.recoveryAfterID = 0
		return 0, nil
	}
	count := 0
	for _, id := range rows {
		err := database.InTransaction(ctx, b.pool, func(tx pgx.Tx) error {
			q := dbgen.New(tx)
			owner, err := q.FindPROwner(ctx, id)
			if err != nil {
				return fmt.Errorf("find recovery owner: %w", err)
			}
			scope, err := lockIntentScope(ctx, q, owner.Int64, id)
			if errors.Is(err, ErrIntentUnavailable) {
				return nil
			}
			if err != nil {
				return err
			}
			i, err := readLockedIntent(ctx, q, id)
			if err != nil {
				return err
			}
			r := NewRequest(id, scope.pr.Generation, scope.pr.HeadSha, scope.pr.State)
			return b.wakeTx(ctx, tx, r, i)
		})
		if err != nil {
			return count, err
		}
		b.recoveryAfterID = id
		count++
	}
	return count, nil
}

type WakeWorker struct {
	river.WorkerDefaults[WakeArgs]
	Broker *Broker
}

func (*WakeWorker) Timeout(*river.Job[WakeArgs]) time.Duration { return 15 * time.Second }
func (w *WakeWorker) Work(ctx context.Context, job *river.Job[WakeArgs]) error {
	return w.Broker.Dispatch(ctx, job.Args)
}
