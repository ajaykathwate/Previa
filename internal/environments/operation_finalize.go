package environments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments/dbgen"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/platform/metrics"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// This file holds the repository side of durable operations: claiming one,
// deciding whether it is still wanted, and recording its outcome. Every
// outcome is written by the lease holder only, inside a transaction that
// takes the usual locks first.

func (r *Repository) claimOperation(ctx context.Context, id int64, owner string, lease time.Duration) (Operation, error) {
	interval := pgtype.Interval{Microseconds: lease.Microseconds(), Valid: true}
	row, err := dbgen.New(r.pool).ClaimOperation(ctx, dbgen.ClaimOperationParams{
		ID: id, LeaseOwner: leaseOwner(owner), LeaseDuration: interval,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Operation{}, err
		}
		return Operation{}, fmt.Errorf("claim environment operation: %w", err)
	}
	return operationFromRow(row)
}

func (r *Repository) operation(ctx context.Context, id int64) (Operation, error) {
	row, err := dbgen.New(r.pool).GetOperation(ctx, id)
	if err != nil {
		return Operation{}, fmt.Errorf("read environment operation: %w", err)
	}
	return operationFromRow(row)
}

func (r *Repository) operationStillRelevant(ctx context.Context, op Operation) (bool, error) {
	switch op.Kind {
	case ReportStatus:
		// A report renders the PR's current state, whatever change queued it.
		return true, nil
	case RemoveRoute:
		// A removal stays relevant until a newer route version exists; the PR
		// may have been reopened since without that making the removal unsafe.
		newer, err := dbgen.New(r.pool).RouteVersionNewerThan(ctx, dbgen.RouteVersionNewerThanParams{PullRequestID: op.PullRequestID, Generation: op.Generation, Revision: 0})
		if err != nil {
			return false, fmt.Errorf("check route removal relevance: %w", err)
		}
		return !newer, nil
	case CleanupArtifact:
		var safe bool
		err := database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
			fence, err := readCleanupFence(ctx, dbgen.New(tx), op.PullRequestID)
			safe = err == nil && fence.allows(op.Payload.Resource, op.Payload.Request)
			return err
		})
		return safe, err
	default:
		return currentOperationRequest(ctx, dbgen.New(r.pool), op.Payload.Request, op.Kind)
	}
}

func (r *Repository) markOperationFailed(ctx context.Context, id int64, owner, code string) error {
	return markFailedTx(ctx, dbgen.New(r.pool), id, owner, code)
}

func (r *Repository) markOperationRetryable(ctx context.Context, id int64, owner, code string, retryAfter time.Duration) error {
	row, err := r.operation(ctx, id)
	if err != nil {
		return err
	}
	affected, err := dbgen.New(r.pool).MarkOperationRetryable(ctx, dbgen.MarkOperationRetryableParams{
		ID: id, LeaseOwner: leaseOwner(owner), Backoff: retryBackoff(row.Attempts, retryAfter),
		LastErrorCode: pgtype.Text{String: code, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("mark operation retryable: %w", err)
	}
	return requireOperationTransition(id, "retryable", affected)
}

// retryBackoff doubles from 5 seconds per attempt up to 5 minutes, but waits
// at least as long as the provider asked (at most an hour).
func retryBackoff(attempts int32, retryAfter time.Duration) pgtype.Interval {
	var backoffSeconds float64 = 5
	for i := int32(1); i < attempts && backoffSeconds < 300; i++ {
		backoffSeconds *= 2
	}
	if backoffSeconds > 300 {
		backoffSeconds = 300
	}
	if wait := min(retryAfter, time.Hour).Seconds(); wait > backoffSeconds {
		backoffSeconds = wait
	}
	return pgtype.Interval{Microseconds: int64(backoffSeconds * float64(time.Second/time.Microsecond)), Valid: true}
}

func requireOperationTransition(id int64, state string, affected int64) error {
	if affected != 1 {
		return fmt.Errorf("operation %d lease lost before %s transition", id, state)
	}
	return nil
}

func (r *Repository) completeOperation(ctx context.Context, op Operation, owner string) error {
	switch op.Kind {
	case RemoveRoute:
		return r.completeRemoval(ctx, op, owner)
	case ReportStatus:
		return r.completeReport(ctx, op, owner)
	}
	var published time.Time // the activated request's eligibility, once committed
	err := database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		authorized, err := lockFinalization(ctx, q, op, owner)
		if err != nil {
			return err
		}
		if op.Kind != CleanupArtifact {
			// The locked PR serializes all generation/revision mutations.
			relevant, err := currentOperationRequest(ctx, q, op.Payload.Request, op.Kind)
			if err != nil {
				return err
			}
			if !authorized || !relevant {
				return markSupersededTx(ctx, q, op.ID, owner)
			}
		}
		switch op.Kind {
		case ActivateRoute:
			if published, err = r.activateTx(ctx, tx, q, op); err != nil {
				return err
			}
		case ReportStatus, CleanupArtifact:
		default:
			return fmt.Errorf("unknown environment operation kind %q", op.Kind)
		}
		return markSucceededTx(ctx, q, op.ID, owner)
	})
	if err == nil && !published.IsZero() {
		metrics.PreviewPublished(time.Since(published))
	}
	return err
}

// lockFinalization takes a finalizing transaction's locks in the usual order:
// the owner's intent scope for an open request, the PR, then the operation's
// own lease. A revoked owner does not stop finalization; it means the effect
// is no longer authorized, which the result reports.
func lockFinalization(ctx context.Context, q *dbgen.Queries, op Operation, owner string) (authorized bool, err error) {
	authorized = true
	if op.Kind != CleanupArtifact && op.Payload.Request.State == StateOpen {
		if _, err := lockOwnerIntentScope(ctx, q, op.PullRequestID, "finalization"); err != nil {
			if !errors.Is(err, ErrIntentUnavailable) {
				return false, err
			}
			authorized = false
		}
	}
	if _, err := q.LockRequest(ctx, op.PullRequestID); err != nil {
		return false, fmt.Errorf("lock PR before operation finalization: %w", err)
	}
	return authorized, lockOperationLease(ctx, q, op.ID, owner, "finalization")
}

// activateTx makes the pending candidate the environment's active preview and
// hands what it replaced (the previous active preview, and an older candidate
// it superseded) to cleanup. It returns when the published request became
// eligible, for the request-to-preview metric.
func (r *Repository) activateTx(ctx context.Context, tx pgx.Tx, q *dbgen.Queries, op Operation) (time.Time, error) {
	active, err := q.ActivatePendingEnvironment(ctx, dbgen.ActivatePendingEnvironmentParams{PrID: op.PullRequestID, ExpectedGeneration: op.Generation, ExpectedRevision: op.Revision, TtlSeconds: int64(r.lifetime / time.Second)})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, errors.New("route activation has no current pending candidate")
		}
		return time.Time{}, fmt.Errorf("activate pending environment: %w", err)
	}
	var replaced []Artifact
	for _, previous := range []Artifact{
		{ContainerID: active.PreviousContainerID.String, ImageTag: active.PreviousImageTag.String},
		op.Payload.PreviousPending,
	} {
		if !artifactReferenced(previous, []Artifact{op.Payload.Artifact}) {
			replaced = append(replaced, previous)
		}
	}
	if err := r.enqueueTakenTx(ctx, tx, op, replaced); err != nil {
		return time.Time{}, err
	}
	if err := r.reportTx(ctx, tx, op.PullRequestID); err != nil {
		return time.Time{}, err
	}
	if !active.EligibleAt.Valid {
		return time.Time{}, nil
	}
	return active.EligibleAt.Time, nil
}

// completeRemoval records a converged route removal. It never needs GitHub
// access. The environment becomes torn_down only if it is still in the
// teardown this removal belongs to; the artifacts the teardown took over are
// scheduled for cleanup either way, and cleanup itself keeps anything a newer
// publication references.
func (r *Repository) completeRemoval(ctx context.Context, op Operation, owner string) error {
	return database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if err := lockTeardownLease(ctx, q, op, owner, "finalization"); err != nil {
			return err
		}
		taken := []Artifact{op.Payload.Previous, op.Payload.Artifact}
		removed, err := q.FinalizePendingTeardown(ctx, dbgen.FinalizePendingTeardownParams{PrID: op.PullRequestID, ExpectedGeneration: op.Generation, ExpectedRevision: 0})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return fmt.Errorf("finalize environment teardown: %w", err)
		default:
			taken = append(taken,
				Artifact{ContainerID: removed.ContainerID.String, ImageTag: removed.ImageTag.String},
				Artifact{ContainerID: removed.PendingContainerID.String, ImageTag: removed.PendingImageTag.String})
		}
		if err := r.enqueueTakenTx(ctx, tx, op, taken); err != nil {
			return err
		}
		if err := r.reportTx(ctx, tx, op.PullRequestID); err != nil {
			return err
		}
		return markSucceededTx(ctx, q, op.ID, owner)
	})
}

// enqueueTakenTx schedules cleanup of artifacts an operation took over. The
// cleanup operation's own check keeps anything still referenced.
func (r *Repository) enqueueTakenTx(ctx context.Context, tx pgx.Tx, op Operation, artifacts []Artifact) error {
	for _, artifact := range artifacts {
		if artifact.empty() {
			continue
		}
		resource := Resource{PullRequestID: op.PullRequestID, Generation: op.Generation, Revision: op.Revision, Artifact: artifact}
		if err := r.enqueueCleanupTx(ctx, tx, op.Payload.Request, resource); err != nil {
			return err
		}
	}
	return nil
}

// supersedeExecuted records an operation that newer work replaced before its
// external effect. A superseded route removal still hands its artifacts to
// cleanup: they predate the teardown, so nothing newer can be using them.
func (r *Repository) supersedeExecuted(ctx context.Context, op Operation, owner string) error {
	return database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if err := lockTeardownLease(ctx, q, op, owner, "it was superseded"); err != nil {
			return err
		}
		if op.Kind == RemoveRoute {
			if err := r.enqueueTakenTx(ctx, tx, op, []Artifact{op.Payload.Previous, op.Payload.Artifact}); err != nil {
				return err
			}
		}
		return markSupersededTx(ctx, q, op.ID, owner)
	})
}

// failActivation ends an activation that cannot succeed (the candidate is
// unhealthy, missing or not the PR's, or retries ran out). If the PR still
// wants that candidate, the attempt fails: the environment keeps its active
// preview, the candidate is scheduled for cleanup (the route row keeps it if
// the router may serve it), and a failure report is queued.
func (r *Repository) failActivation(ctx context.Context, op Operation, owner, code string) error {
	return database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if err := lockTeardownLease(ctx, q, op, owner, "it failed"); err != nil {
			return err
		}
		candidate := op.Payload.Artifact
		category := safeErrorCode(code)
		if category == "" {
			category = "external_operation_failed"
		}
		_, err := q.FailPendingActivation(ctx, dbgen.FailPendingActivationParams{
			PrID: op.PullRequestID, ExpectedGeneration: op.Generation, ExpectedRevision: op.Revision,
			ContainerID: text(candidate.ContainerID), ImageTag: text(candidate.ImageTag), ErrorCode: category,
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// Newer work owns the environment; its own flow cleans the candidate.
		case err != nil:
			return fmt.Errorf("fail pending activation: %w", err)
		default:
			previous, err := q.FailPublicationBuild(ctx, dbgen.FailPublicationBuildParams{ID: op.PullRequestID, Generation: op.Generation, Revision: op.Revision})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("fail publication request: %w", err)
			}
			if err == nil && previous != "succeeded" {
				if err := q.RecordFinishedBuildTime(ctx, dbgen.RecordFinishedBuildTimeParams{PrID: op.PullRequestID, Revision: op.Revision}); err != nil {
					return fmt.Errorf("record failed publication build time: %w", err)
				}
			}
			// The candidate, and any older candidate it had replaced, are done.
			if err := r.enqueueTakenTx(ctx, tx, op, []Artifact{candidate, op.Payload.PreviousPending}); err != nil {
				return err
			}
			if err := r.reportTx(ctx, tx, op.PullRequestID); err != nil {
				return err
			}
		}
		return markFailedTx(ctx, q, op.ID, owner, code)
	})
}

// completeReport records a finished report. The reporter itself recorded
// which version it wrote; a newer change has its own report operation.
func (r *Repository) completeReport(ctx context.Context, op Operation, owner string) error {
	return markSucceededTx(ctx, dbgen.New(r.pool), op.ID, owner)
}

// lockTeardownLease takes the teardown locks (which need no GitHub access),
// then confirms owner still holds the operation's lease.
func lockTeardownLease(ctx context.Context, q *dbgen.Queries, op Operation, owner, before string) error {
	if _, err := lockTeardownScope(ctx, q, op.PullRequestID); err != nil {
		return err
	}
	return lockOperationLease(ctx, q, op.ID, owner, before)
}

// lockOperationLease confirms, inside the recording transaction, that owner
// still holds the operation's lease; before names what it is held for.
func lockOperationLease(ctx context.Context, q *dbgen.Queries, id int64, owner, before string) error {
	_, err := q.LockCurrentOperationLease(ctx, dbgen.LockCurrentOperationLeaseParams{ID: id, LeaseOwner: leaseOwner(owner)})
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("environment operation lease expired before " + before)
	}
	if err != nil {
		return fmt.Errorf("lock current environment operation lease: %w", err)
	}
	return nil
}

func markSucceededTx(ctx context.Context, q *dbgen.Queries, id int64, owner string) error {
	affected, err := q.MarkOperationSucceeded(ctx, dbgen.MarkOperationSucceededParams{ID: id, LeaseOwner: leaseOwner(owner)})
	if err != nil {
		return fmt.Errorf("mark operation succeeded: %w", err)
	}
	return requireOperationTransition(id, "succeeded", affected)
}

func markSupersededTx(ctx context.Context, q *dbgen.Queries, id int64, owner string) error {
	affected, err := q.MarkOperationSuperseded(ctx, dbgen.MarkOperationSupersededParams{ID: id, LeaseOwner: leaseOwner(owner)})
	if err != nil {
		return fmt.Errorf("mark operation superseded: %w", err)
	}
	return requireOperationTransition(id, "superseded", affected)
}

func markFailedTx(ctx context.Context, q *dbgen.Queries, id int64, owner, code string) error {
	affected, err := q.MarkOperationFailed(ctx, dbgen.MarkOperationFailedParams{ID: id, LeaseOwner: leaseOwner(owner), LastErrorCode: pgtype.Text{String: code, Valid: true}})
	if err != nil {
		return fmt.Errorf("mark operation failed: %w", err)
	}
	return requireOperationTransition(id, "failed", affected)
}

// leaseOwner is the lease holder as stored: always set, unlike text().
func leaseOwner(owner string) pgtype.Text { return pgtype.Text{String: owner, Valid: true} }
