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
	"github.com/ajaykathwatee/previa/internal/platform/metrics"
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
	// RetryAfter is the provider's own delay (a rate limit's reset or
	// Retry-After); the next attempt waits at least this long, at most an hour.
	RetryAfter time.Duration
	Err        error
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
	river.WorkerDefaults[OperationArgs]
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

// Timeout gives River's job the operation's own timeout plus room to claim
// the operation and record its outcome, which happen outside that timeout.
func (w *OperationWorker) Timeout(job *river.Job[OperationArgs]) time.Duration {
	const bookkeeping = 30 * time.Second
	if timeout := w.timeouts[job.Args.OperationKind]; timeout > 0 {
		return timeout + bookkeeping
	}
	return 10*time.Second + bookkeeping
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
		if err := w.repo.supersedeExecuted(ctx, op, owner); err != nil {
			return err
		}
		w.logger.InfoContext(ctx, "environment operation superseded", "operation_id", op.ID, "kind", op.Kind, "pr_id", op.PullRequestID, "generation", op.Generation, "revision", op.Revision)
		return nil
	}

	workCtx, cancel := context.WithTimeout(ctx, w.timeouts[op.Kind])
	execErr := w.executor.Execute(workCtx, op)
	cancel()
	if errors.Is(execErr, ErrSuperseded) {
		if err := w.repo.supersedeExecuted(ctx, op, owner); err != nil {
			return err
		}
		w.logger.InfoContext(ctx, "environment operation superseded", "operation_id", op.ID, "kind", op.Kind, "pr_id", op.PullRequestID, "generation", op.Generation, "revision", op.Revision)
		return nil
	}
	if execErr != nil {
		var classified *OperationFailure
		permanent := errors.As(execErr, &classified) && classified.Permanent
		code := operationErrorCode(execErr)
		if attempt <= 0 {
			attempt = int(op.Attempts)
		}
		// Route removal and cleanup are idempotent and must eventually happen:
		// running out of River attempts leaves them retryable for recovery.
		exhausted := maxAttempts > 0 && attempt >= maxAttempts && !op.Kind.retriedUntilDone()
		return w.failClaim(ctx, op, owner, code, permanent || exhausted, execErr)
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
		markFailed := w.repo.markOperationFailed
		if op.Kind == ActivateRoute {
			// The candidate never became the route: fail the attempt, keep the
			// active preview, and remove the candidate.
			markFailed = func(ctx context.Context, id int64, owner, code string) error {
				return w.repo.failActivation(ctx, op, owner, code)
			}
		}
		if err := markFailed(writeCtx, op.ID, owner, code); err != nil {
			return errors.Join(errors.New("terminal environment operation failed"), err)
		}
		w.logger.ErrorContext(ctx, "environment operation permanently failed", "operation_id", op.ID, "kind", op.Kind, "pr_id", op.PullRequestID, "generation", op.Generation, "revision", op.Revision, "error_code", code)
		return river.JobCancel(errors.New("environment operation permanently failed: " + code))
	}
	var delay time.Duration
	var classified *OperationFailure
	if errors.As(cause, &classified) {
		delay = classified.RetryAfter
	}
	if err := w.repo.markOperationRetryable(writeCtx, op.ID, owner, code, delay); err != nil {
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
	default:
		return currentOperationRequest(ctx, dbgen.New(r.pool), op.Payload.Request, op.Kind)
	}
	resource := op.Payload.Resource
	if resource.PullRequestID != op.PullRequestID || resource.Generation < 0 || resource.Revision < 0 {
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
		if resource.Generation > pr.Generation || (resource.Generation == pr.Generation && resource.Revision > current.Revision) {
			return nil
		}
		scheduledBy := op.Payload.Request
		inFlight := pr.State == "open" && (current.State == string(IntentQueued) || current.State == string(IntentRunning)) &&
			(scheduledBy.Generation != pr.Generation || scheduledBy.Revision != current.Revision)
		if inFlight && resource.Generation == pr.Generation && resource.Revision == current.Revision {
			return nil
		}
		e, err := q.ReadEnvironment(ctx, op.PullRequestID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read environment before cleanup operation: %w", err)
		}
		keep, err := referencedArtifacts(ctx, q, op.PullRequestID, e)
		if err != nil {
			return err
		}
		safe = !resourceReferenced(resource, keep)
		return nil
	})
	return safe, err
}

func (r *Repository) markOperationFailed(ctx context.Context, id int64, owner, code string) error {
	affected, err := dbgen.New(r.pool).MarkOperationFailed(ctx, dbgen.MarkOperationFailedParams{ID: id, LeaseOwner: pgtype.Text{String: owner, Valid: true}, LastErrorCode: pgtype.Text{String: code, Valid: true}})
	if err != nil {
		return fmt.Errorf("mark operation failed: %w", err)
	}
	return requireOperationTransition(id, "failed", affected)
}

func (r *Repository) markOperationRetryable(ctx context.Context, id int64, owner, code string, retryAfter time.Duration) error {
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
	if wait := min(retryAfter, time.Hour).Seconds(); wait > backoffSeconds {
		backoffSeconds = wait
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
	switch op.Kind {
	case RemoveRoute:
		return r.completeRemoval(ctx, op, owner)
	case ReportStatus:
		return r.completeReport(ctx, op, owner)
	}
	var published time.Time // the activated request's eligibility, once committed
	err := database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
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
			active, err := q.ActivatePendingEnvironment(ctx, dbgen.ActivatePendingEnvironmentParams{PrID: op.PullRequestID, ExpectedGeneration: op.Generation, ExpectedRevision: op.Revision, TtlSeconds: int64(r.lifetime / time.Second)})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return errors.New("route activation has no current pending candidate")
				}
				return fmt.Errorf("activate pending environment: %w", err)
			}
			if active.EligibleAt.Valid {
				published = active.EligibleAt.Time
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
			if err := r.reportTx(ctx, tx, op.PullRequestID); err != nil {
				return err
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
	if err == nil && !published.IsZero() {
		metrics.PreviewPublished(time.Since(published))
	}
	return err
}

// completeRemoval records a converged route removal. It never needs GitHub
// access. The environment becomes torn_down only if it is still in the
// teardown this removal belongs to; the artifacts the teardown took over are
// scheduled for cleanup either way, and cleanup itself keeps anything a newer
// publication references.
func (r *Repository) completeRemoval(ctx context.Context, op Operation, owner string) error {
	return database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, err := lockTeardownScope(ctx, q, op.PullRequestID); err != nil {
			return err
		}
		if _, err := q.LockCurrentOperationLease(ctx, dbgen.LockCurrentOperationLeaseParams{
			ID: op.ID, LeaseOwner: pgtype.Text{String: owner, Valid: true},
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errors.New("environment operation lease expired before finalization")
			}
			return fmt.Errorf("lock current environment operation lease: %w", err)
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
		affected, err := q.MarkOperationSucceeded(ctx, dbgen.MarkOperationSucceededParams{ID: op.ID, LeaseOwner: pgtype.Text{String: owner, Valid: true}})
		if err != nil {
			return fmt.Errorf("mark operation succeeded: %w", err)
		}
		return requireOperationTransition(op.ID, "succeeded", affected)
	})
}

// enqueueTakenTx schedules cleanup of artifacts an operation took over. The
// cleanup operation's own check keeps anything still referenced.
func (r *Repository) enqueueTakenTx(ctx context.Context, tx pgx.Tx, op Operation, artifacts []Artifact) error {
	for _, artifact := range artifacts {
		if artifact.ContainerID == "" && artifact.ImageTag == "" {
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
		if _, err := lockTeardownScope(ctx, q, op.PullRequestID); err != nil {
			return err
		}
		if _, err := q.LockCurrentOperationLease(ctx, dbgen.LockCurrentOperationLeaseParams{
			ID: op.ID, LeaseOwner: pgtype.Text{String: owner, Valid: true},
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errors.New("environment operation lease expired before it was superseded")
			}
			return fmt.Errorf("lock current environment operation lease: %w", err)
		}
		if op.Kind == RemoveRoute {
			if err := r.enqueueTakenTx(ctx, tx, op, []Artifact{op.Payload.Previous, op.Payload.Artifact}); err != nil {
				return err
			}
		}
		affected, err := q.MarkOperationSuperseded(ctx, dbgen.MarkOperationSupersededParams{ID: op.ID, LeaseOwner: pgtype.Text{String: owner, Valid: true}})
		if err != nil {
			return fmt.Errorf("mark operation superseded: %w", err)
		}
		return requireOperationTransition(op.ID, "superseded", affected)
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
		if _, err := lockTeardownScope(ctx, q, op.PullRequestID); err != nil {
			return err
		}
		if _, err := q.LockCurrentOperationLease(ctx, dbgen.LockCurrentOperationLeaseParams{
			ID: op.ID, LeaseOwner: pgtype.Text{String: owner, Valid: true},
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errors.New("environment operation lease expired before it failed")
			}
			return fmt.Errorf("lock current environment operation lease: %w", err)
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
			// The candidate, and any older candidate it had replaced, are done.
			if err := r.enqueueTakenTx(ctx, tx, op, []Artifact{candidate, op.Payload.PreviousPending}); err != nil {
				return err
			}
			if err := r.reportTx(ctx, tx, op.PullRequestID); err != nil {
				return err
			}
		}
		affected, err := q.MarkOperationFailed(ctx, dbgen.MarkOperationFailedParams{ID: op.ID, LeaseOwner: pgtype.Text{String: owner, Valid: true}, LastErrorCode: pgtype.Text{String: code, Valid: true}})
		if err != nil {
			return fmt.Errorf("mark operation failed: %w", err)
		}
		return requireOperationTransition(op.ID, "failed", affected)
	})
}

// completeReport records a finished report. The reporter itself recorded
// which version it wrote; a newer change has its own report operation.
func (r *Repository) completeReport(ctx context.Context, op Operation, owner string) error {
	affected, err := dbgen.New(r.pool).MarkOperationSucceeded(ctx, dbgen.MarkOperationSucceededParams{ID: op.ID, LeaseOwner: pgtype.Text{String: owner, Valid: true}})
	if err != nil {
		return fmt.Errorf("mark report succeeded: %w", err)
	}
	return requireOperationTransition(op.ID, "succeeded", affected)
}
