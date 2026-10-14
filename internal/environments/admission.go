package environments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments/dbgen"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (r *Repository) claimBuild(ctx context.Context, request Request, now time.Time) (attempt Attempt, allowed bool, err error) {
	var waiting *WaitingError
	err = database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		attempt, allowed, waiting = Attempt{}, false, nil
		q := dbgen.New(tx)
		scope, e := lockOwnerIntentScope(ctx, q, request.PullRequestID, "admission")
		if errors.Is(e, ErrIntentUnavailable) {
			return nil
		}
		if e != nil {
			return e
		}
		if scope.pr.Generation == request.Generation && scope.pr.HeadSha == request.HeadSHA && scope.pr.State == StateOpen {
			if e := BuildBlockReason(scope.pr.BuildStatus, scope.pr.NeedsRefresh); e != nil {
				return e
			}
		}
		wait, e := r.admissionWaitTx(ctx, q, request.PullRequestID, now)
		if e != nil {
			return e
		}
		if wait != nil {
			// No slot, or not this account's turn: the request stays queued
			// and says why.
			waiting = wait
			return r.markWaitingTx(ctx, tx, request, wait.Reason)
		}
		affected, e := q.ClaimBuildIntent(ctx, dbgen.ClaimBuildIntentParams{PrID: request.PullRequestID, Generation: request.Generation, Revision: request.Revision, HeadSha: request.HeadSHA, BuildMode: scope.mode, NowAt: pgtype.Timestamptz{Time: now, Valid: true}})
		if e != nil {
			return fmt.Errorf("claim authorized build intent: %w", e)
		}
		if allowed = affected == 1; !allowed {
			return nil
		}
		attempt, e = r.openAttemptTx(ctx, tx, request)
		return e
	})
	if err != nil {
		return Attempt{}, false, err
	}
	if waiting != nil {
		return Attempt{}, false, waiting
	}
	return attempt, allowed, nil
}

// openAttemptTx starts an admitted attempt: it provisions the PR's lifetime
// environment on its first build, opens the attempt's log stream and clears
// the previous attempt's logs. This happens only after admission, in the same
// transaction: rejected or stale jobs must keep the current logs and the
// current stream epoch.
func (r *Repository) openAttemptTx(ctx context.Context, tx pgx.Tx, request Request) (Attempt, error) {
	q := dbgen.New(tx)
	if r.hostnames != nil {
		if err := r.provisionTx(ctx, q, request.PullRequestID); err != nil {
			return Attempt{}, err
		}
	}
	var attempt Attempt
	stream, err := q.ResetBuildLogStream(ctx, dbgen.ResetBuildLogStreamParams{PrID: request.PullRequestID, Generation: request.Generation, Revision: request.Revision, HeadSha: request.HeadSHA})
	switch {
	case err == nil:
		attempt = Attempt{EnvironmentID: stream.EnvironmentID, LogEpoch: stream.Epoch}
	case errors.Is(err, pgx.ErrNoRows):
		// No lifetime environment row yet, so there is no log stream to open.
	default:
		return Attempt{}, fmt.Errorf("open build log stream: %w", err)
	}
	if err := q.ClearBuildLogs(ctx, request.PullRequestID); err != nil {
		return Attempt{}, fmt.Errorf("clear previous build logs: %w", err)
	}
	return attempt, r.reportTx(ctx, tx, request.PullRequestID)
}

// currentOperationRequest is an optimization before external work. Finalization
// locks the PR again and repeats these predicates. Adapters must enforce the same
// generation/revision fence atomically in their external system.
func currentOperationRequest(ctx context.Context, q *dbgen.Queries, request Request, kind OperationKind) (bool, error) {
	pr, err := q.ReadRequest(ctx, request.PullRequestID)
	if err != nil {
		return false, fmt.Errorf("read operation PR: %w", err)
	}
	if pr.Generation != request.Generation || pr.State != request.State || pr.HeadSha != request.HeadSHA {
		return false, nil
	}
	if request.State == StateOpen && BuildBlockReason(pr.BuildStatus, pr.NeedsRefresh) != nil {
		return false, nil
	}
	if request.State != StateOpen {
		return request.Revision == 0, nil
	}
	if kind == ActivateRoute {
		if allowed, err := publicationAuthorized(ctx, q, request.PullRequestID); err != nil || !allowed {
			return false, err
		}
	}
	i, err := q.ReadBuildIntent(ctx, request.PullRequestID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read operation intent revision: %w", err)
	}
	owner, err := q.FindPROwner(ctx, request.PullRequestID)
	if err != nil {
		return false, fmt.Errorf("read operation owner: %w", err)
	}
	// Ownership/access for publication, without acquiring locks across external IO.
	if _, err := q.ReadOwnedIntentView(ctx, dbgen.ReadOwnedIntentViewParams{PrID: request.PullRequestID, UserID: pgtype.Int8{Int64: owner, Valid: true}}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("check operation access: %w", err)
	}
	return i.Generation == request.Generation && i.Revision == request.Revision && i.HeadSha == request.HeadSHA && (i.State == string(IntentRunning) || i.State == string(IntentSucceeded) || (i.State == string(IntentFailed) && kind == ReportStatus)), nil
}

// provisionTx creates the PR's lifetime environment on its first admitted
// build. The hostname is allocated here, once: rebuilds, failures, teardown and
// reopening keep it, and the unique constraint means it is never reassigned.
func (r *Repository) provisionTx(ctx context.Context, q *dbgen.Queries, pullRequestID int64) error {
	target, err := q.ReadProvisioningTarget(ctx, pullRequestID)
	if err != nil {
		return fmt.Errorf("read environment provisioning target: %w", err)
	}
	if target.Provisioned {
		return nil
	}
	if !target.GithubRepoID.Valid {
		return errors.New("repository has no GitHub identity for a preview hostname")
	}
	for attempt := 0; attempt < 3; attempt++ {
		label, err := r.hostnames(target.Number, target.GithubRepoID.Int64)
		if err != nil {
			return fmt.Errorf("allocate preview hostname: %w", err)
		}
		inserted, err := q.ProvisionEnvironment(ctx, dbgen.ProvisionEnvironmentParams{PullRequestID: pullRequestID, Subdomain: label})
		if err != nil {
			return fmt.Errorf("provision environment: %w", err)
		}
		if inserted == 1 {
			return nil
		}
	}
	return errors.New("could not allocate a unique preview hostname")
}
