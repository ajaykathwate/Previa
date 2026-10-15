package environments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
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
// the job ID and attempt, preventing a rescued attempt from stealing a live
// claim from its predecessor before that lease expires.
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
			// The durable operation and River have independent backoffs. Waiting
			// for a due time or a live lease is not a failed execution attempt.
			until := current.AvailableAt
			if current.State == OperationRunning && current.LeaseExpiresAt.After(until) {
				until = current.LeaseExpiresAt
			}
			return river.JobSnooze(max(time.Until(until), time.Second))
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
		return w.supersede(ctx, op, owner)
	}

	workCtx, cancel := context.WithTimeout(ctx, w.timeouts[op.Kind])
	execErr := w.executor.Execute(workCtx, op)
	cancel()
	if errors.Is(execErr, ErrSuperseded) {
		return w.supersede(ctx, op, owner)
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
	w.logger.InfoContext(ctx, "environment operation completed", op.logAttrs()...)
	return nil
}

// supersede records an operation that newer work replaced.
func (w *OperationWorker) supersede(ctx context.Context, op Operation, owner string) error {
	if err := w.repo.supersedeExecuted(ctx, op, owner); err != nil {
		return err
	}
	w.logger.InfoContext(ctx, "environment operation superseded", op.logAttrs()...)
	return nil
}

// logAttrs identifies an operation in logs.
func (op Operation) logAttrs(extra ...any) []any {
	return append([]any{"operation_id", op.ID, "kind", op.Kind, "pr_id", op.PullRequestID, "generation", op.Generation, "revision", op.Revision}, extra...)
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
			markFailed = func(ctx context.Context, _ int64, owner, code string) error {
				return w.repo.failActivation(ctx, op, owner, code)
			}
		}
		if err := markFailed(writeCtx, op.ID, owner, code); err != nil {
			return errors.Join(errors.New("terminal environment operation failed"), err)
		}
		w.logger.ErrorContext(ctx, "environment operation permanently failed", op.logAttrs("error_code", code)...)
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
	w.logger.WarnContext(ctx, "environment operation will retry", op.logAttrs("error_code", code)...)
	return errors.New("environment operation failed: " + code)
}
