package secrets

import (
	"context"
	"errors"
	"fmt"

	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/secrets/dbgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository is the PostgreSQL store for repository secrets.
type Repository struct{ pool *pgxpool.Pool }

func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

var (
	_ Store         = (*Repository)(nil)
	_ SealedStore   = (*Repository)(nil)
	_ RotationStore = (*Repository)(nil)
)

func owner(id int64) pgtype.Int8 { return pgtype.Int8{Int64: id, Valid: true} }

func ownedError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUnavailable
	}
	return fmt.Errorf("read repository ownership: %w", err)
}

func (r *Repository) Repository(ctx context.Context, userID, repoID int64) (RepositoryView, error) {
	row, err := dbgen.New(r.pool).OwnedRepository(ctx, dbgen.OwnedRepositoryParams{ID: repoID, OwnerUserID: owner(userID)})
	if err != nil {
		return RepositoryView{}, ownedError(err)
	}
	return RepositoryView{ID: row.ID, FullName: row.FullName, Available: row.Available}, nil
}

func (r *Repository) List(ctx context.Context, userID, repoID int64) ([]Summary, error) {
	q := dbgen.New(r.pool)
	if _, err := q.OwnedRepository(ctx, dbgen.OwnedRepositoryParams{ID: repoID, OwnerUserID: owner(userID)}); err != nil {
		return nil, ownedError(err)
	}
	rows, err := q.ListSecrets(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	result := make([]Summary, 0, len(rows))
	for _, row := range rows {
		result = append(result, Summary{Name: row.Key, UpdatedAt: row.UpdatedAt.Time})
	}
	return result, nil
}

// PutMany stores several sealed values in one transaction: all or nothing.
// It locks only the repository row, which serializes the per-repository limit
// without a second lock, so it cannot form a cycle with the installation →
// repository → PR order used elsewhere. It reports how many names were new.
func (r *Repository) PutMany(ctx context.Context, userID, repoID int64, items []SealedSecret) (created int, err error) {
	err = database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		repo, err := q.LockOwnedRepository(ctx, dbgen.LockOwnedRepositoryParams{ID: repoID, OwnerUserID: owner(userID)})
		if err != nil {
			return ownedError(err)
		}
		if !repo.Available {
			return ErrUnavailable
		}
		names := make([]string, len(items))
		for i, item := range items {
			names[i] = item.Name
		}
		existing, err := q.ExistingSecretNames(ctx, dbgen.ExistingSecretNamesParams{RepoID: repoID, Keys: names})
		if err != nil {
			return fmt.Errorf("check secrets: %w", err)
		}
		fresh := len(items) - len(existing)
		if fresh > 0 {
			count, err := q.CountSecrets(ctx, repoID)
			if err != nil {
				return fmt.Errorf("count secrets: %w", err)
			}
			if count+int64(fresh) > MaxSecretsPerRepository {
				return ErrLimit
			}
		}
		for _, item := range items {
			if err := q.UpsertSecret(ctx, dbgen.UpsertSecretParams{RepoID: repoID, Key: item.Name, EncryptedValue: item.Value, KeyVersion: item.Version}); err != nil {
				return fmt.Errorf("store secret: %w", err)
			}
		}
		created = fresh
		return nil
	})
	return created, err
}

// DeleteMany removes the named secrets and reports how many existed.
func (r *Repository) DeleteMany(ctx context.Context, userID, repoID int64, names []string) (int, error) {
	q := dbgen.New(r.pool)
	if _, err := q.OwnedRepository(ctx, dbgen.OwnedRepositoryParams{ID: repoID, OwnerUserID: owner(userID)}); err != nil {
		return 0, ownedError(err)
	}
	n, err := q.DeleteOwnedSecrets(ctx, dbgen.DeleteOwnedSecretsParams{ID: repoID, OwnerUserID: owner(userID), Keys: names})
	if err != nil {
		return 0, fmt.Errorf("delete secrets: %w", err)
	}
	return int(n), nil
}

func (r *Repository) SealedSecrets(ctx context.Context, repoID int64) ([]SealedSecret, error) {
	rows, err := dbgen.New(r.pool).SealedSecrets(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("read sealed secrets: %w", err)
	}
	result := make([]SealedSecret, 0, len(rows))
	for _, row := range rows {
		result = append(result, SealedSecret{Name: row.Key, Value: row.EncryptedValue, Version: row.KeyVersion})
	}
	return result, nil
}

func (r *Repository) NeedingRotation(ctx context.Context, activeVersion int32, afterID int64, limit int32) ([]StoredSecret, error) {
	rows, err := dbgen.New(r.pool).SecretsNeedingRotation(ctx, dbgen.SecretsNeedingRotationParams{ActiveVersion: activeVersion, AfterID: afterID, BatchSize: limit})
	if err != nil {
		return nil, fmt.Errorf("list secrets needing rotation: %w", err)
	}
	result := make([]StoredSecret, 0, len(rows))
	for _, row := range rows {
		result = append(result, StoredSecret{ID: row.ID, RepoID: row.RepoID, SealedSecret: SealedSecret{Name: row.Key, Value: row.EncryptedValue, Version: row.KeyVersion}})
	}
	return result, nil
}

func (r *Repository) Rotate(ctx context.Context, s StoredSecret, sealed []byte, version int32) (bool, error) {
	n, err := dbgen.New(r.pool).RotateSecret(ctx, dbgen.RotateSecretParams{NewValue: sealed, NewVersion: version, ID: s.ID, OldVersion: s.Version, OldValue: s.Value})
	if err != nil {
		return false, fmt.Errorf("rotate secret: %w", err)
	}
	return n == 1, nil
}

func (r *Repository) CountByVersion(ctx context.Context) (map[int32]int64, error) {
	rows, err := dbgen.New(r.pool).CountSecretsByKeyVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("count secrets by key version: %w", err)
	}
	counts := make(map[int32]int64, len(rows))
	for _, row := range rows {
		counts[row.KeyVersion] = row.Secrets
	}
	return counts, nil
}
