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

func (r *Repository) claimBuild(ctx context.Context, request Request, now time.Time) (allowed bool, err error) {
	err = database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
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
		affected, e := q.ClaimBuildIntent(ctx, dbgen.ClaimBuildIntentParams{PrID: request.PullRequestID, Generation: request.Generation, Revision: request.Revision, HeadSha: request.HeadSHA, BuildMode: scope.mode, NowAt: pgtype.Timestamptz{Time: now, Valid: true}})
		if e != nil {
			return fmt.Errorf("claim authorized build intent: %w", e)
		}
		allowed = affected == 1
		return nil
	})
	return allowed, err
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
