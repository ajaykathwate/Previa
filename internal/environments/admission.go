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
	err = database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		attempt, allowed = Attempt{}, false
		q := dbgen.New(tx)
		owner, e := q.FindPROwner(ctx, request.PullRequestID)
		if e != nil {
			return fmt.Errorf("find admission owner: %w", e)
		}
		scope, e := lockIntentScope(ctx, q, owner.Int64, request.PullRequestID)
		if errors.Is(e, ErrIntentUnavailable) {
			return nil
		}
		if e != nil {
			return e
		}
		if scope.pr.Generation == request.Generation && scope.pr.HeadSha == request.HeadSHA && scope.pr.State == "open" {
			if e := BuildBlockReason(scope.pr.BuildStatus, scope.pr.NeedsRefresh); e != nil {
				return e
			}
		}
		affected, e := q.ClaimBuildIntent(ctx, dbgen.ClaimBuildIntentParams{PrID: request.PullRequestID, Generation: request.Generation, Revision: request.Revision, HeadSha: request.HeadSHA, BuildMode: scope.mode, NowAt: pgtype.Timestamptz{Time: now, Valid: true}})
		if e != nil {
			return fmt.Errorf("claim authorized build intent: %w", e)
		}
		allowed = affected == 1
		if allowed && r.hostnames != nil {
			if e := r.provisionTx(ctx, q, request.PullRequestID); e != nil {
				return e
			}
		}
		if allowed {
			// Open the new attempt's log stream and clear the previous attempt only
			// after admission, in the same transaction. Rejected/stale jobs must
			// preserve current logs and the current stream epoch.
			stream, e := q.ResetBuildLogStream(ctx, dbgen.ResetBuildLogStreamParams{PrID: request.PullRequestID, Generation: request.Generation, Revision: request.Revision, HeadSha: request.HeadSHA})
			switch {
			case e == nil:
				attempt = Attempt{EnvironmentID: stream.EnvironmentID, LogEpoch: stream.Epoch}
			case errors.Is(e, pgx.ErrNoRows):
				// No lifetime environment row yet, so there is no log stream to open.
			default:
				return fmt.Errorf("open build log stream: %w", e)
			}
			if e := q.ClearBuildLogs(ctx, request.PullRequestID); e != nil {
				return fmt.Errorf("clear previous build logs: %w", e)
			}
		}
		return nil
	})
	if err != nil {
		return Attempt{}, false, err
	}
	return attempt, allowed, nil
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
	if request.State == "open" && BuildBlockReason(pr.BuildStatus, pr.NeedsRefresh) != nil {
		return false, nil
	}
	if request.State != "open" {
		return request.Revision == 0, nil
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
	if _, err := q.ReadOwnedIntentView(ctx, dbgen.ReadOwnedIntentViewParams{PrID: request.PullRequestID, UserID: owner}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("check operation access: %w", err)
	}
	return i.Generation == request.Generation && i.Revision == request.Revision && i.HeadSha == request.HeadSHA && (i.State == "running" || i.State == "succeeded" || (i.State == "failed" && kind == ReportStatus)), nil
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
