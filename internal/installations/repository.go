package installations

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ajaykathwatee/previa/internal/installations/dbgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type database interface {
	dbgen.DBTX
	Begin(context.Context) (pgx.Tx, error)
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}
type InitialDiscoverer interface {
	DiscoverInstallationTx(context.Context, pgx.Tx, int64) error
}
type Repository struct {
	db        database
	discovery InitialDiscoverer
}

func NewRepository(db database, discoverers ...InitialDiscoverer) *Repository {
	r := &Repository{db: db}
	if len(discoverers) > 0 {
		r.discovery = discoverers[0]
	}
	return r
}
func (r *Repository) Prepare(ctx context.Context, id int64) (int64, error) {
	q := dbgen.New(r.db)
	revision, err := q.GetInstallationRevision(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read installation revision: %w", err)
	}
	return revision, nil
}
func (r *Repository) transaction(ctx context.Context, operation func(pgx.Tx, *dbgen.Queries) error) (err error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin installation transaction: %w", err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if rollbackErr := tx.Rollback(rollbackCtx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			err = errors.Join(err, fmt.Errorf("rollback installation transaction: %w", rollbackErr))
		}
	}()
	if err := operation(tx, dbgen.New(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit installation transaction: %w", err)
	}
	return nil
}
func (r *Repository) Link(ctx context.Context, id, userID, revision int64, account string, repos []RepositoryInfo) error {
	return r.transaction(ctx, func(tx pgx.Tx, q *dbgen.Queries) error {
		if err := q.EnsureInstallation(ctx, id); err != nil {
			return fmt.Errorf("ensure verified installation: %w", err)
		}
		row, err := q.LockInstallation(ctx, id)
		if err != nil {
			return fmt.Errorf("lock installation: %w", err)
		}
		if row.OwnerUserID.Valid && row.OwnerUserID.Int64 != userID {
			return ErrForbidden
		}
		if row.Revision != revision || row.Status != "active" {
			return ErrSuperseded
		}
		if err := q.LinkInstallation(ctx, dbgen.LinkInstallationParams{ID: row.ID, OwnerUserID: pgtype.Int8{Int64: userID, Valid: true}, AccountLogin: account, Status: "active"}); err != nil {
			return fmt.Errorf("set installation owner: %w", err)
		}
		if err := q.MarkRepositoriesUnavailable(ctx, row.ID); err != nil {
			return fmt.Errorf("reset installation repository access: %w", err)
		}
		if err := upsertRepositories(ctx, q, row.ID, repos, false); err != nil {
			return err
		}
		return r.discover(ctx, tx, id)
	})
}
func upsertRepositories(ctx context.Context, q *dbgen.Queries, id int64, repos []RepositoryInfo, deferConflicts bool) error {
	for _, repo := range repos {
		existing, err := q.GetRepositoryInstallation(ctx, pgtype.Int8{Int64: repo.ID, Valid: true})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("look up repository installation: %w", err)
		}
		if err == nil && existing != id {
			// An unlinked lifecycle event cannot transfer another installation's history.
			// Its verified callback will re-fetch the repository list before linking.
			if deferConflicts {
				continue
			}
			count, err := q.ReassignInactiveOwnedRepository(ctx, dbgen.ReassignInactiveOwnedRepositoryParams{NewInstallationID: id, GithubRepoID: pgtype.Int8{Int64: repo.ID, Valid: true}, FullName: repo.FullName})
			if err != nil {
				return fmt.Errorf("relink inactive owned repository: %w", err)
			}
			if count == 0 {
				return ErrForbidden
			}
			continue
		}
		params := dbgen.UpdateRepositoryByGitHubIDParams{InstallationID: id, GithubRepoID: pgtype.Int8{Int64: repo.ID, Valid: true}, FullName: repo.FullName}
		count, err := q.UpdateRepositoryByGitHubID(ctx, params)
		if err != nil {
			return fmt.Errorf("update stable repository identity: %w", err)
		}
		if count == 0 {
			count, err := q.UpsertLinkedRepository(ctx, dbgen.UpsertLinkedRepositoryParams{InstallationID: id, GithubRepoID: params.GithubRepoID, FullName: repo.FullName})
			if err != nil {
				return fmt.Errorf("record installation repository: %w", err)
			}
			if count == 0 {
				return ErrForbidden
			}
		}
	}
	return nil
}
func (r *Repository) ApplyEvent(ctx context.Context, id int64, status, account string, added, removed []RepositoryInfo, replace bool) error {
	return r.transaction(ctx, func(tx pgx.Tx, q *dbgen.Queries) error {
		if err := applyEvent(ctx, q, id, status, account, added, removed, replace); err != nil {
			return err
		}
		return r.discover(ctx, tx, id)
	})
}

// ApplyEventTx composes lifecycle mutation with the authenticated delivery receipt.
func (r *Repository) ApplyEventTx(ctx context.Context, tx pgx.Tx, change LifecycleChange) error {
	if err := applyEvent(ctx, dbgen.New(tx), change.ID, change.Status, change.Account, change.Added, change.Removed, change.Replace); err != nil {
		return err
	}
	return r.discover(ctx, tx, change.ID)
}
func applyEvent(ctx context.Context, q *dbgen.Queries, id int64, status, account string, added, removed []RepositoryInfo, replace bool) error {
	if err := q.EnsureInstallation(ctx, id); err != nil {
		return fmt.Errorf("ensure lifecycle installation: %w", err)
	}
	row, err := q.LockInstallation(ctx, id)
	if err != nil {
		return fmt.Errorf("lock lifecycle installation: %w", err)
	}
	// Deletion is terminal for a GitHub installation ID. A fresh install has a new ID.
	if row.Status == "inactive" {
		status = "inactive"
	} else if status == "" {
		status = row.Status
	}
	if account == "" {
		account = row.AccountLogin
	}
	if err := q.UpdateInstallationLifecycle(ctx, dbgen.UpdateInstallationLifecycleParams{ID: row.ID, Status: status, AccountLogin: account}); err != nil {
		return fmt.Errorf("update installation lifecycle: %w", err)
	}
	if replace || status == "inactive" {
		if err := q.MarkRepositoriesUnavailable(ctx, row.ID); err != nil {
			return fmt.Errorf("revoke installation repositories: %w", err)
		}
	}
	if status == "active" {
		if err := upsertRepositories(ctx, q, row.ID, added, !row.OwnerUserID.Valid); err != nil {
			return err
		}
	}
	for _, repo := range removed {
		if err := q.RemoveRepositoryAccess(ctx, dbgen.RemoveRepositoryAccessParams{InstallationID: row.ID, GithubRepoID: pgtype.Int8{Int64: repo.ID, Valid: true}}); err != nil {
			return fmt.Errorf("remove repository access: %w", err)
		}
	}
	return nil
}
func (r *Repository) List(ctx context.Context, userID int64) ([]InstallationInfo, error) {
	rows, err := dbgen.New(r.db).ListLinkedInstallations(ctx, pgtype.Int8{Int64: userID, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("list linked installations: %w", err)
	}
	result := make([]InstallationInfo, 0)
	for _, row := range rows {
		if len(result) == 0 || result[len(result)-1].ID != row.GithubInstallationID {
			result = append(result, InstallationInfo{ID: row.GithubInstallationID, Account: row.AccountLogin, Status: row.Status})
		}
		if row.FullName.Valid {
			i := len(result) - 1
			result[i].Repositories = append(result[i].Repositories, RepositoryInfo{ID: row.GithubRepoID.Int64, FullName: row.FullName.String, Accessible: row.Accessible.Bool && row.Status == "active"})
		}
	}
	return result, nil
}

func (r *Repository) discover(ctx context.Context, tx pgx.Tx, id int64) error {
	if r.discovery == nil {
		return nil
	}
	return r.discovery.DiscoverInstallationTx(ctx, tx, id)
}
