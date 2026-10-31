package outbound

import (
	"context"
	"errors"
	"fmt"

	"github.com/ajaykathwatee/previa/internal/outbound/dbgen"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/platform/egress"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// jobInserter wakes the worker's refresh inside a transaction; River's client
// implements it.
type jobInserter interface {
	InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// Repository is the PostgreSQL store of approvals.
type Repository struct {
	pool *pgxpool.Pool
	jobs jobInserter // nil: changes wake nothing (the periodic refresh still runs)
}

func NewRepository(pool *pgxpool.Pool, jobs jobInserter) *Repository {
	return &Repository{pool: pool, jobs: jobs}
}

func owner(id int64) pgtype.Int8 { return pgtype.Int8{Int64: id, Valid: true} }

func (r *Repository) Repository(ctx context.Context, userID, repoID int64) (RepositoryView, error) {
	row, err := dbgen.New(r.pool).OwnedRepository(ctx, dbgen.OwnedRepositoryParams{RepoID: repoID, UserID: owner(userID)})
	if errors.Is(err, pgx.ErrNoRows) {
		return RepositoryView{}, ErrUnavailable
	}
	if err != nil {
		return RepositoryView{}, fmt.Errorf("read repository: %w", err)
	}
	return RepositoryView{ID: row.ID, FullName: row.FullName, Available: row.Available}, nil
}

func (r *Repository) List(ctx context.Context, userID, repoID int64) ([]Approved, error) {
	rows, err := dbgen.New(r.pool).ListDestinations(ctx, dbgen.ListDestinationsParams{RepoID: repoID, UserID: owner(userID)})
	if err != nil {
		return nil, fmt.Errorf("list approved destinations: %w", err)
	}
	approved := make([]Approved, len(rows))
	for i, row := range rows {
		approved[i] = Approved{ID: row.ID, Host: row.Host, Port: int(row.Port), CreatedAt: row.CreatedAt.Time}
	}
	return approved, nil
}

// Add approves d under the repository's row lock: ownership, current access,
// the limit and the one-per-port rule hold together.
func (r *Repository) Add(ctx context.Context, userID, repoID int64, d egress.Destination, limit int) error {
	return database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, err := q.LockOutboundInstallation(ctx, repoID); errors.Is(err, pgx.ErrNoRows) {
			return ErrUnavailable
		} else if err != nil {
			return fmt.Errorf("lock outbound installation: %w", err)
		}
		if _, err := q.LockOwnedRepository(ctx, dbgen.LockOwnedRepositoryParams{RepoID: repoID, UserID: owner(userID)}); errors.Is(err, pgx.ErrNoRows) {
			if _, ownErr := q.OwnedRepository(ctx, dbgen.OwnedRepositoryParams{RepoID: repoID, UserID: owner(userID)}); ownErr == nil {
				return ErrAccessRemoved
			}
			return ErrUnavailable
		} else if err != nil {
			return fmt.Errorf("lock repository: %w", err)
		}
		count, err := q.CountDestinations(ctx, repoID)
		if err != nil {
			return fmt.Errorf("count approved destinations: %w", err)
		}
		if count >= int64(limit) {
			return ErrLimit
		}
		added, err := q.AddDestination(ctx, dbgen.AddDestinationParams{RepoID: repoID, Host: d.Host, Port: int32(d.Port), CreatedBy: owner(userID)})
		if err != nil {
			return fmt.Errorf("approve destination: %w", err)
		}
		if added == 0 {
			return ErrPortTaken
		}
		return r.wake(ctx, tx)
	})
}

// Remove withdraws one approval of the owner's repository.
func (r *Repository) Remove(ctx context.Context, userID, repoID, id int64) (egress.Destination, error) {
	var removed egress.Destination
	err := database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		row, err := dbgen.New(tx).RemoveDestination(ctx, dbgen.RemoveDestinationParams{ID: id, RepoID: repoID, UserID: owner(userID)})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnavailable
		}
		if err != nil {
			return fmt.Errorf("withdraw destination: %w", err)
		}
		removed = egress.Destination{Host: row.Host, Port: int(row.Port)}
		return r.wake(ctx, tx)
	})
	return removed, err
}

func (r *Repository) wake(ctx context.Context, tx pgx.Tx) error {
	if r.jobs == nil {
		return nil
	}
	if _, err := r.jobs.InsertTx(ctx, tx, RefreshArgs{}, &river.InsertOpts{Queue: Queue, MaxAttempts: 3}); err != nil {
		return fmt.Errorf("wake the outbound refresh: %w", err)
	}
	return nil
}

// Destinations returns the approvals of each repository, for the worker. It
// is not owner-scoped: the build it serves was authorized already.
func (r *Repository) Destinations(ctx context.Context, repoIDs ...int64) (map[int64][]egress.Destination, error) {
	rows, err := dbgen.New(r.pool).DestinationsForRepositories(ctx, repoIDs)
	if err != nil {
		return nil, fmt.Errorf("read approved destinations: %w", err)
	}
	approved := map[int64][]egress.Destination{}
	for _, row := range rows {
		approved[row.RepoID] = append(approved[row.RepoID], egress.Destination{Host: row.Host, Port: int(row.Port)})
	}
	return approved, nil
}
