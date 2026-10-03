package installations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/installations/dbgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const RepositoryPageSize int64 = 25

type WorkspaceSummary struct{ Installations, Repositories, Available int64 }
type RepositoryFilter struct {
	Page           int64
	Search, Access string
}
type RepositoryRow struct {
	ID, InstallationID        int64
	FullName, Account, Status string
	Accessible                bool
}
type RepositoryPage struct {
	Rows               []RepositoryRow
	Total, Page, Pages int64
	Filter             RepositoryFilter
}

func (s *Service) Summary(ctx context.Context, userID int64) (WorkspaceSummary, error) {
	return s.store.Summary(ctx, userID)
}
func (s *Service) Page(ctx context.Context, userID int64, filter RepositoryFilter) (RepositoryPage, error) {
	filter.Search = strings.TrimSpace(filter.Search)
	if len([]rune(filter.Search)) > 100 {
		return RepositoryPage{}, errors.New("search must be at most 100 characters")
	}
	if filter.Access == "" {
		filter.Access = "all"
	}
	if filter.Access != "all" && filter.Access != "available" && filter.Access != "unavailable" {
		return RepositoryPage{}, errors.New("invalid repository access filter")
	}
	if filter.Page < 1 {
		return RepositoryPage{}, errors.New("page must be positive")
	}
	return s.store.Page(ctx, userID, filter)
}
func (r *Repository) Summary(ctx context.Context, userID int64) (WorkspaceSummary, error) {
	row, err := dbgen.New(r.db).GetWorkspaceSummary(ctx, pgtype.Int8{Int64: userID, Valid: true})
	if err != nil {
		return WorkspaceSummary{}, fmt.Errorf("read workspace summary: %w", err)
	}
	return WorkspaceSummary{Installations: row.Installations, Repositories: row.Repositories, Available: row.Available}, nil
}
func (r *Repository) Page(ctx context.Context, userID int64, filter RepositoryFilter) (page RepositoryPage, err error) {
	// A repeatable-read snapshot keeps count, clamped page and rows consistent
	// while installation callbacks/events update repository access concurrently.
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return page, fmt.Errorf("begin repository page snapshot: %w", err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if rollbackErr := tx.Rollback(rollbackCtx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			err = errors.Join(err, fmt.Errorf("rollback repository page: %w", rollbackErr))
		}
	}()
	q := dbgen.New(tx)
	total, err := q.CountRepositoryPage(ctx, dbgen.CountRepositoryPageParams{UserID: pgtype.Int8{Int64: userID, Valid: true}, Search: filter.Search, Access: filter.Access})
	if err != nil {
		return page, fmt.Errorf("count repository page: %w", err)
	}
	pages := int64(1)
	if total > 0 {
		pages = (total-1)/RepositoryPageSize + 1
	}
	current := min(filter.Page, pages)
	rows, err := q.ListRepositoryPage(ctx, dbgen.ListRepositoryPageParams{UserID: pgtype.Int8{Int64: userID, Valid: true}, Search: filter.Search, Access: filter.Access, PageSize: RepositoryPageSize, PageOffset: (current - 1) * RepositoryPageSize})
	if err != nil {
		return page, fmt.Errorf("list repository page: %w", err)
	}
	page = RepositoryPage{Total: total, Page: current, Pages: pages, Filter: filter}
	for _, row := range rows {
		page.Rows = append(page.Rows, RepositoryRow{ID: row.ID, InstallationID: row.GithubInstallationID, FullName: row.FullName, Account: row.AccountLogin, Status: row.Status, Accessible: row.Accessible && row.Status == "active"})
	}
	if err := tx.Commit(ctx); err != nil {
		return RepositoryPage{}, fmt.Errorf("commit repository page snapshot: %w", err)
	}
	return page, nil
}
