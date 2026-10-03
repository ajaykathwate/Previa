package pullrequests

import (
	"context"
	"errors"

	"github.com/ajaykathwatee/previa/internal/platform/database"
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
		result.Rows, err = q.ListPRs(ctx, dbgen.ListPRsParams{ID: repoID, OwnerUserID: owner(userID), Column3: f.State, Lower: f.Search, Offset: int32((result.Page - 1) * 10)})
		return err
	})
	return
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

func (r *Repository) Activity(ctx context.Context, userID, repoID, page int64, environments bool) (result ActivityPage, err error) {
	err = database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY"); err != nil {
			return err
		}
		q := dbgen.New(tx)
		if repoID > 0 {
			if _, err := q.OwnedRepository(ctx, dbgen.OwnedRepositoryParams{ID: repoID, OwnerUserID: owner(userID)}); err != nil {
				return scopeError(err)
			}
		}
		var err error
		result.Total, err = q.CountActivity(ctx, dbgen.CountActivityParams{OwnerUserID: owner(userID), Column2: repoID, Column3: environments})
		if err != nil {
			return err
		}
		result.Pages = max(int64(1), (result.Total+9)/10)
		result.Page = min(max(int64(1), page), result.Pages)
		result.Rows, err = q.Activity(ctx, dbgen.ActivityParams{OwnerUserID: owner(userID), Column2: repoID, Column3: environments, Offset: int32((result.Page - 1) * 10)})
		return err
	})
	return
}
