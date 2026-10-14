package pullrequests

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/database"
	workspacescope "github.com/ajaykathwatee/previa/internal/platform/scope"
	"github.com/ajaykathwatee/previa/internal/pullrequests/dbgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct{ pool *pgxpool.Pool }

func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool} }

type Filter struct {
	Page          int64
	State, Search string
}
type RepositoryPage struct {
	Repo               dbgen.OwnedRepositoryRow
	Sync               dbgen.PrSync
	Rows               []dbgen.ListPRsRow
	Total, Page, Pages int64
}

func owner(id int64) pgtype.Int8 { return pgtype.Int8{Int64: id, Valid: true} }
func (r *Repository) Page(ctx context.Context, userID, repoID int64, f Filter) (result RepositoryPage, err error) {
	err = database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY"); err != nil {
			return err
		}
		q := dbgen.New(tx)
		var err error
		result.Repo, err = q.OwnedRepository(ctx, dbgen.OwnedRepositoryParams{ID: repoID, OwnerUserID: owner(userID)})
		if err != nil {
			return scopeError(err)
		}
		result.Sync, err = q.ReadSync(ctx, repoID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		result.Total, err = q.CountPRs(ctx, dbgen.CountPRsParams{ID: repoID, OwnerUserID: owner(userID), Column3: f.State, Lower: f.Search})
		if err != nil {
			return err
		}
		result.Pages = max(int64(1), (result.Total+9)/10)
		result.Page = min(max(int64(1), f.Page), result.Pages)
		result.Rows, err = q.ListPRs(ctx, dbgen.ListPRsParams{ID: repoID, OwnerUserID: owner(userID), Column3: f.State, Lower: f.Search, RowOffset: int32((result.Page - 1) * 10)})
		return err
	})
	return
}

// Owns reports ErrUnavailable unless userID owns the repository's installation.
func (r *Repository) Owns(ctx context.Context, userID, repoID int64) error {
	if _, err := dbgen.New(r.pool).OwnedRepository(ctx, dbgen.OwnedRepositoryParams{ID: repoID, OwnerUserID: owner(userID)}); err != nil {
		return scopeError(err)
	}
	return nil
}
func (r *Repository) Detail(ctx context.Context, userID, repoID int64, number int32) (dbgen.OwnedPRRow, error) {
	row, err := dbgen.New(r.pool).OwnedPR(ctx, dbgen.OwnedPRParams{ID: repoID, OwnerUserID: owner(userID), Number: number})
	if err != nil {
		return row, scopeError(err)
	}
	return row, nil
}

type ActivityPage struct {
	Rows               []dbgen.ActivityRow
	Total, Page, Pages int64
}

// ActivityQuery selects a page of Builds or Previews. Failures narrows
// Builds to FailuresAttention, FailuresOlder or FailuresDismissed; a failure
// needs attention when it finished at or after Since.
type ActivityQuery struct {
	RepoID, Page int64
	Previews     bool
	Failures     string
	Since        time.Time
}

func (r *Repository) Activity(ctx context.Context, userID int64, aq ActivityQuery) (result ActivityPage, err error) {
	err = database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY"); err != nil {
			return err
		}
		q := dbgen.New(tx)
		if aq.RepoID > 0 {
			if _, err := q.OwnedRepository(ctx, dbgen.OwnedRepositoryParams{ID: aq.RepoID, OwnerUserID: owner(userID)}); err != nil {
				return scopeError(err)
			}
		}
		since := pgtype.Timestamptz{Time: aq.Since, Valid: true}
		var err error
		result.Total, err = q.CountActivity(ctx, dbgen.CountActivityParams{WorkspaceID: workspacescope.WorkspaceID(ctx), OwnerUserID: userID, RepoID: aq.RepoID, Previews: aq.Previews, Failures: aq.Failures, AttentionSince: since})
		if err != nil {
			return err
		}
		result.Pages = max(int64(1), (result.Total+9)/10)
		result.Page = min(max(int64(1), aq.Page), result.Pages)
		result.Rows, err = q.Activity(ctx, dbgen.ActivityParams{WorkspaceID: workspacescope.WorkspaceID(ctx), OwnerUserID: userID, RepoID: aq.RepoID, Previews: aq.Previews, Failures: aq.Failures,
			AttentionSince: since, RowOffset: int32((result.Page - 1) * 10)})
		return err
	})
	return
}

// Delivery counts the owner's current requests and live previews on open PRs;
// failures that finished before since and were not dismissed count as older.
func (r *Repository) Delivery(ctx context.Context, userID int64, since time.Time) (dbgen.DeliverySummaryRow, error) {
	row, err := dbgen.New(r.pool).DeliverySummary(ctx, dbgen.DeliverySummaryParams{WorkspaceID: workspacescope.WorkspaceID(ctx), OwnerUserID: userID, AttentionSince: pgtype.Timestamptz{Time: since, Valid: true}})
	if err != nil {
		return row, fmt.Errorf("count delivery: %w", err)
	}
	return row, nil
}

// DismissFailure records that userID dismissed the PR's failed request at
// revision. It refuses (ErrUnavailable) when that is no longer the PR's
// current, failed request: a newer request replaced it.
func (r *Repository) DismissFailure(ctx context.Context, userID, pullRequestID, revision int64) error {
	n, err := dbgen.New(r.pool).DismissFailure(ctx, dbgen.DismissFailureParams{UserID: userID, PullRequestID: pullRequestID, Revision: revision})
	if err != nil {
		return fmt.Errorf("dismiss failure: %w", err)
	}
	if n == 0 {
		return ErrFailureChanged
	}
	return nil
}

// RestoreFailure undoes userID's dismissal of the PR's failed request.
func (r *Repository) RestoreFailure(ctx context.Context, userID, pullRequestID int64) error {
	if err := dbgen.New(r.pool).RestoreFailure(ctx, dbgen.RestoreFailureParams{UserID: userID, PullRequestID: pullRequestID}); err != nil {
		return fmt.Errorf("restore failure: %w", err)
	}
	return nil
}
