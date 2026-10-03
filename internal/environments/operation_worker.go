package environments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments/dbgen"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"
)

// OperationExecutor performs exactly one external side effect. Implementations
// must be idempotent, honor the context, and enforce generation/revision fencing for route
// changes. It is called only after the claim transaction has committed.
type OperationExecutor interface {
	Execute(context.Context, Operation) error
}

// OperationFailure lets an adapter classify a stable, non-secret error code.
// Raw provider errors are intentionally not persisted in River or PostgreSQL.
type OperationFailure struct {
	Code      string
	Permanent bool
	Err       error
}

func (e *OperationFailure) Error() string {
	if e == nil || e.Code == "" {
		return "external operation failed"
	}
	return "external operation failed: " + e.Code
}
func (e *OperationFailure) Unwrap() error { return e.Err }

var safeOperationCode = regexp.MustCompile(`^[a-zA-Z0-9_]{1,64}$`)

type OperationWorker struct {
	repo     *Repository
	executor OperationExecutor
	logger   *slog.Logger
	lease    time.Duration
	timeouts map[OperationKind]time.Duration
}

func NewOperationWorker(repo *Repository, executor OperationExecutor, logger *slog.Logger, lease time.Duration, timeouts map[OperationKind]time.Duration) (*OperationWorker, error) {
	if repo == nil || executor == nil || logger == nil {
		return nil, errors.New("operation worker requires repository, executor, and logger")
	}
	if lease <= 0 {
		return nil, errors.New("operation worker lease must be positive")
	}
	copyTimeouts := make(map[OperationKind]time.Duration, len(timeouts))
	for _, kind := range []OperationKind{ActivateRoute, RemoveRoute, ReportStatus, CleanupArtifact} {
		timeout := timeouts[kind]
		if timeout <= 0 || lease <= timeout {
			return nil, fmt.Errorf("operation %s needs a positive timeout shorter than its lease", kind)
		}
		copyTimeouts[kind] = timeout
	}
	return &OperationWorker{repo: repo, executor: executor, logger: logger, lease: lease, timeouts: copyTimeouts}, nil
}

func (w *OperationWorker) Timeout(job *river.Job[OperationArgs]) time.Duration {
	if timeout := w.timeouts[job.Args.OperationKind]; timeout > 0 {
		return timeout
	}
	return 10 * time.Second
}

func (w *OperationWorker) Work(ctx context.Context, job *river.Job[OperationArgs]) error {
	if job == nil || job.ID <= 0 || job.Args.OperationID <= 0 || !job.Args.OperationKind.valid() {
		return river.JobCancel(errors.New("invalid environment operation job"))
	}
	op, err := w.repo.operation(ctx, job.Args.OperationID)
	if err != nil {
		return err
	}
	if op.Kind != job.Args.OperationKind {
		return river.JobCancel(errors.New("environment operation kind does not match durable operation"))
	}
	owner := fmt.Sprintf("%d:%d", job.ID, job.Attempt)
	return w.Process(ctx, job.Args.OperationID, owner, job.Attempt, job.MaxAttempts)
}

// Process is also used by deterministic integration tests. The owner value is
// River's stable job ID, allowing a rescued attempt of that same job to reclaim
// its own lease while preventing a different job from stealing a live claim.
func (w *OperationWorker) Process(ctx context.Context, operationID int64, owner string, attempt, maxAttempts int) error {
	if operationID <= 0 || owner == "" {
		return errors.New("invalid environment operation claim")
	}
	op, err := w.repo.claimOperation(ctx, operationID, owner, w.lease)
	if errors.Is(err, pgx.ErrNoRows) {
		current, readErr := w.repo.operation(ctx, operationID)
		if readErr != nil {
			return readErr
		}
		switch current.State {
		case OperationSucceeded, OperationSuperseded, OperationFailed:
			return nil
		default:
			return errors.New("environment operation is already leased")
		}
	}
	if err != nil {
		return err
	}
	if !operationArgsMatch(op, operationID) {
		return w.failClaim(ctx, op, owner, "invalid_operation", true, nil)
	}

	safe, err := w.repo.operationStillRelevant(ctx, op)
	if err != nil {
		return w.failClaim(ctx, op, owner, "generation_check_failed", false, err)
	}
	if !safe {
		if err := w.repo.markOperationSuperseded(ctx, op.ID, owner); err != nil {
			return err
		}
		w.logger.InfoContext(ctx, "environment operation superseded", "operation_id", op.ID, "kind", op.Kind, "pr_id", op.PullRequestID, "generation", op.Generation, "revision", op.Revision)
		return nil
	}

	workCtx, cancel := context.WithTimeout(ctx, w.timeouts[op.Kind])
	execErr := w.executor.Execute(workCtx, op)
	cancel()
	if execErr != nil {
		var classified *OperationFailure
		permanent := errors.As(execErr, &classified) && classified.Permanent
		code := operationErrorCode(execErr)
		if attempt <= 0 {
			attempt = int(op.Attempts)
		}
		return w.failClaim(ctx, op, owner, code, permanent || (maxAttempts > 0 && attempt >= maxAttempts), execErr)
	}
	if err := w.repo.completeOperation(ctx, op, owner); err != nil {
		return fmt.Errorf("complete environment operation %d: %w", op.ID, err)
	}
	w.logger.InfoContext(ctx, "environment operation completed", "operation_id", op.ID, "kind", op.Kind, "pr_id", op.PullRequestID, "generation", op.Generation, "revision", op.Revision)
	return nil
}

func operationArgsMatch(op Operation, id int64) bool {
	return op.ID == id && op.Payload.Request.PullRequestID == op.PullRequestID
}

func operationErrorCode(err error) string {
	var classified *OperationFailure
	if errors.As(err, &classified) && safeOperationCode.MatchString(classified.Code) {
		return classified.Code
	}
	return "external_operation_failed"
}

func (w *OperationWorker) failClaim(ctx context.Context, op Operation, owner, code string, terminal bool, cause error) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if terminal {
		if err := w.repo.markOperationFailed(writeCtx, op.ID, owner, code); err != nil {
			return errors.Join(errors.New("terminal environment operation failed"), err)
		}
		w.logger.ErrorContext(ctx, "environment operation permanently failed", "operation_id", op.ID, "kind", op.Kind, "pr_id", op.PullRequestID, "generation", op.Generation, "revision", op.Revision, "error_code", code)
		return river.JobCancel(errors.New("environment operation permanently failed: " + code))
	}
	if err := w.repo.markOperationRetryable(writeCtx, op.ID, owner, code); err != nil {
		return errors.Join(errors.New("environment operation failed"), err)
	}
	w.logger.WarnContext(ctx, "environment operation will retry", "operation_id", op.ID, "kind", op.Kind, "pr_id", op.PullRequestID, "generation", op.Generation, "revision", op.Revision, "error_code", code)
	return errors.New("environment operation failed: " + code)
}

func (r *Repository) claimOperation(ctx context.Context, id int64, owner string, lease time.Duration) (Operation, error) {
	interval := pgtype.Interval{Microseconds: lease.Microseconds(), Valid: true}
	row, err := dbgen.New(r.pool).ClaimOperation(ctx, dbgen.ClaimOperationParams{
		ID: id, LeaseOwner: pgtype.Text{String: owner, Valid: true}, LeaseDuration: interval,
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
	if op.Kind != CleanupArtifact {
		return currentOperationRequest(ctx, dbgen.New(r.pool), op.Payload.Request, op.Kind)
	}
	if op.Payload.Resource.PullRequestID != op.PullRequestID || op.Payload.Resource.Generation < 0 || op.Payload.Resource.Revision < 0 {
		return false, nil
	}
	var safe bool
	err := database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		pr, err := q.LockRequest(ctx, op.PullRequestID)
		if err != nil {
			return fmt.Errorf("lock PR for cleanup operation: %w", err)
		}
		current, err := q.ReadBuildIntent(ctx, op.PullRequestID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read cleanup intent: %w", err)
		}
		if op.Payload.Resource.Generation > pr.Generation || (op.Payload.Resource.Generation == pr.Generation && op.Payload.Resource.Revision > current.Revision) {
			return nil
		}
		e, err := q.ReadEnvironment(ctx, op.PullRequestID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read environment before cleanup operation: %w", err)
		}
		keep := []Artifact{{ContainerID: e.ContainerID.String, ImageTag: e.ImageTag.String}}
		if e.PendingGeneration.Valid {
			keep = append(keep, Artifact{ContainerID: e.PendingContainerID.String, ImageTag: e.PendingImageTag.String})
		}
		protected, err := listProtectedArtifacts(ctx, q, op.PullRequestID)
		if err != nil {
			return err
		}
		keep = append(keep, protected...)
		safe = !artifactReferenced(op.Payload.Resource.Artifact, keep)
		return nil
	})
	return safe, err
}

func (r *Repository) markOperationSuperseded(ctx context.Context, id int64, owner string) error {
	affected, err := dbgen.New(r.pool).MarkOperationSuperseded(ctx, dbgen.MarkOperationSupersededParams{ID: id, LeaseOwner: pgtype.Text{String: owner, Valid: true}})
	if err != nil {
		return fmt.Errorf("mark operation superseded: %w", err)
	}
	return requireOperationTransition(id, "superseded", affected)
}

func (r *Repository) markOperationFailed(ctx context.Context, id int64, owner, code string) error {
	affected, err := dbgen.New(r.pool).MarkOperationFailed(ctx, dbgen.MarkOperationFailedParams{ID: id, LeaseOwner: pgtype.Text{String: owner, Valid: true}, LastErrorCode: pgtype.Text{String: code, Valid: true}})
	if err != nil {
		return fmt.Errorf("mark operation failed: %w", err)
	}
	return requireOperationTransition(id, "failed", affected)
}

func (r *Repository) markOperationRetryable(ctx context.Context, id int64, owner, code string) error {
	var backoffSeconds float64 = 5
	row, err := r.operation(ctx, id)
	if err != nil {
		return err
	}
	for i := int32(1); i < row.Attempts && backoffSeconds < 300; i++ {
		backoffSeconds *= 2
	}
	if backoffSeconds > 300 {
		backoffSeconds = 300
	}
	backoff := pgtype.Interval{Microseconds: int64(backoffSeconds * float64(time.Second/time.Microsecond)), Valid: true}
	affected, err := dbgen.New(r.pool).MarkOperationRetryable(ctx, dbgen.MarkOperationRetryableParams{
		ID: id, LeaseOwner: pgtype.Text{String: owner, Valid: true}, Backoff: backoff,
		LastErrorCode: pgtype.Text{String: code, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("mark operation retryable: %w", err)
	}
	return requireOperationTransition(id, "retryable", affected)
}

func requireOperationTransition(id int64, state string, affected int64) error {
	if affected != 1 {
		return fmt.Errorf("operation %d lease lost before %s transition", id, state)
	}
	return nil
}

func (r *Repository) completeOperation(ctx context.Context, op Operation, owner string) error {
	return database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		authorized := true
		if op.Kind != CleanupArtifact && op.Payload.Request.State == "open" {
			user, err := q.FindPROwner(ctx, op.PullRequestID)
			if err != nil {
				return fmt.Errorf("find finalization owner: %w", err)
			}
			if _, err := lockIntentScope(ctx, q, user.Int64, op.PullRequestID); err != nil {
				if !errors.Is(err, ErrIntentUnavailable) {
					return err
				}
				authorized = false
			}
		}
		if _, err := q.LockRequest(ctx, op.PullRequestID); err != nil {
			return fmt.Errorf("lock PR before operation finalization: %w", err)
		}
		if _, err := q.LockCurrentOperationLease(ctx, dbgen.LockCurrentOperationLeaseParams{
			ID: op.ID, LeaseOwner: pgtype.Text{String: owner, Valid: true},
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errors.New("environment operation lease expired before finalization")
			}
			return fmt.Errorf("lock current environment operation lease: %w", err)
		}
		if op.Kind != CleanupArtifact {
			pr, err := q.LockRequest(ctx, op.PullRequestID)
			if err != nil {
				return fmt.Errorf("lock PR to finalize operation: %w", err)
			}
			_ = pr // The locked PR serializes all generation/revision mutations.
			relevant, err := currentOperationRequest(ctx, q, op.Payload.Request, op.Kind)
			if err != nil {
				return err
			}
			if !authorized || !relevant {
				affected, err := q.MarkOperationSuperseded(ctx, dbgen.MarkOperationSupersededParams{ID: op.ID, LeaseOwner: pgtype.Text{String: owner, Valid: true}})
				if err != nil {
					return fmt.Errorf("supersede operation after external effect: %w", err)
				}
				return requireOperationTransition(op.ID, "superseded", affected)
			}
		}
		switch op.Kind {
		case ActivateRoute:
			active, err := q.ActivatePendingEnvironment(ctx, dbgen.ActivatePendingEnvironmentParams{PrID: op.PullRequestID, ExpectedGeneration: op.Generation, ExpectedRevision: op.Revision})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return errors.New("route activation has no current pending candidate")
				}
				return fmt.Errorf("activate pending environment: %w", err)
			}
			for _, previous := range []Artifact{
				{ContainerID: active.PreviousContainerID.String, ImageTag: active.PreviousImageTag.String},
				op.Payload.PreviousPending,
			} {
				if (previous.ContainerID == "" && previous.ImageTag == "") || artifactReferenced(previous, []Artifact{op.Payload.Artifact}) {
					continue
				}
				resource := Resource{PullRequestID: op.PullRequestID, Generation: op.Generation, Revision: op.Revision, Artifact: previous}
				if err := r.enqueueCleanupTx(ctx, tx, op.Payload.Request, resource); err != nil {
					return err
				}
			}
		case RemoveRoute:
			removed, err := q.FinalizePendingTeardown(ctx, dbgen.FinalizePendingTeardownParams{PrID: op.PullRequestID, ExpectedGeneration: op.Generation, ExpectedRevision: op.Revision})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return errors.New("route removal has no current teardown")
				}
				return fmt.Errorf("finalize environment teardown: %w", err)
			}
			for _, artifact := range []Artifact{
				{ContainerID: removed.ContainerID.String, ImageTag: removed.ImageTag.String},
				op.Payload.Artifact,
			} {
				if artifact.ContainerID == "" && artifact.ImageTag == "" {
					continue
				}
				resource := Resource{PullRequestID: op.PullRequestID, Generation: op.Generation, Revision: op.Revision, Artifact: artifact}
				if err := r.enqueueCleanupTx(ctx, tx, op.Payload.Request, resource); err != nil {
					return err
				}
			}
		case ReportStatus, CleanupArtifact:
		default:
			return fmt.Errorf("unknown environment operation kind %q", op.Kind)
		}
		affected, err := q.MarkOperationSucceeded(ctx, dbgen.MarkOperationSucceededParams{ID: op.ID, LeaseOwner: pgtype.Text{String: owner, Valid: true}})
		if err != nil {
			return fmt.Errorf("mark operation succeeded: %w", err)
		}
		return requireOperationTransition(op.ID, "succeeded", affected)
	})
}
