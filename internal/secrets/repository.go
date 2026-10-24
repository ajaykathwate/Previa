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
	_ Store             = (*Repository)(nil)
	_ SealedStore       = (*Repository)(nil)
	_ RotationStore     = (*Repository)(nil)
	_ FileRotationStore = (*Repository)(nil)
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
		result = append(result, Summary{Name: row.Key, UpdatedAt: row.UpdatedAt.Time, Build: row.Build})
	}
	return result, nil
}

// EnvFile reads an owned repository's sealed file (Revision 0 when none was
// saved) and its sealed secrets.
func (r *Repository) EnvFile(ctx context.Context, userID, repoID int64) (SealedFile, []SealedSecret, error) {
	q := dbgen.New(r.pool)
	if _, err := q.OwnedRepository(ctx, dbgen.OwnedRepositoryParams{ID: repoID, OwnerUserID: owner(userID)}); err != nil {
		return SealedFile{}, nil, ownedError(err)
	}
	var file SealedFile
	row, err := q.EnvFile(ctx, repoID)
	switch {
	case err == nil:
		file = SealedFile{Value: row.EncryptedText, Version: row.KeyVersion, Revision: row.Revision, UpdatedAt: row.UpdatedAt.Time, UpdatedBy: row.UpdatedBy}
	case !errors.Is(err, pgx.ErrNoRows):
		return SealedFile{}, nil, fmt.Errorf("read .env file: %w", err)
	}
	sealed, err := r.SealedSecrets(ctx, repoID)
	return file, sealed, err
}

// SaveEnvFile replaces the repository's secrets with save.Keep, writes the
// new and changed values and stores the file, in one transaction.
// Installation then repository locks serialize access revocation in the same
// order as other protected mutations.
func (r *Repository) SaveEnvFile(ctx context.Context, userID, repoID int64, save FileSave) (revision int64, err error) {
	err = database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, err := q.LockSecretInstallation(ctx, repoID); err != nil {
			return ownedError(err)
		}
		repo, err := q.LockOwnedRepository(ctx, dbgen.LockOwnedRepositoryParams{ID: repoID, OwnerUserID: owner(userID)})
		if err != nil {
			return ownedError(err)
		}
		if !repo.Available && len(save.Upsert) > 0 {
			return ErrUnavailable
		}
		if save.Expected == 0 {
			revision, err = q.CreateEnvFile(ctx, dbgen.CreateEnvFileParams{RepoID: repoID, EncryptedText: save.File.Value, KeyVersion: save.File.Version, UpdatedBy: owner(userID)})
		} else {
			revision, err = q.UpdateEnvFile(ctx, dbgen.UpdateEnvFileParams{EncryptedText: save.File.Value, KeyVersion: save.File.Version, UpdatedBy: owner(userID), RepoID: repoID, ExpectedRevision: save.Expected})
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrChanged
		}
		if err != nil {
			return fmt.Errorf("store .env file: %w", err)
		}
		if err := q.DeleteSecretsNotIn(ctx, dbgen.DeleteSecretsNotInParams{RepoID: repoID, Keys: append([]string{}, save.Keep...)}); err != nil {
			return fmt.Errorf("remove secrets: %w", err)
		}
		for _, item := range save.Upsert {
			if err := q.UpsertSecret(ctx, dbgen.UpsertSecretParams{RepoID: repoID, Key: item.Name, EncryptedValue: item.Value, KeyVersion: item.Version}); err != nil {
				return fmt.Errorf("store secret: %w", err)
			}
		}
		return nil
	})
	return revision, err
}

// SetBuild lets Dockerfile steps of trusted builds read a secret, or stops
// it; false when the secret does not exist or already had that setting.
func (r *Repository) SetBuild(ctx context.Context, userID, repoID int64, name string, build bool) (bool, error) {
	q := dbgen.New(r.pool)
	if _, err := q.OwnedRepository(ctx, dbgen.OwnedRepositoryParams{ID: repoID, OwnerUserID: owner(userID)}); err != nil {
		return false, ownedError(err)
	}
	n, err := q.SetSecretBuild(ctx, dbgen.SetSecretBuildParams{RepoID: repoID, OwnerUserID: owner(userID), Key: name, Build: build})
	if err != nil {
		return false, fmt.Errorf("set build use of secret: %w", err)
	}
	return n == 1, nil
}

func (r *Repository) SealedSecrets(ctx context.Context, repoID int64) ([]SealedSecret, error) {
	rows, err := dbgen.New(r.pool).SealedSecrets(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("read sealed secrets: %w", err)
	}
	result := make([]SealedSecret, 0, len(rows))
	for _, row := range rows {
		result = append(result, SealedSecret{Name: row.Key, Value: row.EncryptedValue, Version: row.KeyVersion, Build: row.Build})
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

func (r *Repository) FilesNeedingRotation(ctx context.Context, activeVersion int32, afterRepoID int64, limit int32) ([]StoredFile, error) {
	rows, err := dbgen.New(r.pool).EnvFilesNeedingRotation(ctx, dbgen.EnvFilesNeedingRotationParams{ActiveVersion: activeVersion, AfterRepoID: afterRepoID, BatchSize: limit})
	if err != nil {
		return nil, fmt.Errorf("list .env files needing rotation: %w", err)
	}
	result := make([]StoredFile, 0, len(rows))
	for _, row := range rows {
		result = append(result, StoredFile{RepoID: row.RepoID, Value: row.EncryptedText, Version: row.KeyVersion})
	}
	return result, nil
}

func (r *Repository) RotateFile(ctx context.Context, f StoredFile, sealed []byte, version int32) (bool, error) {
	n, err := dbgen.New(r.pool).RotateEnvFile(ctx, dbgen.RotateEnvFileParams{NewText: sealed, NewVersion: version, RepoID: f.RepoID, OldVersion: f.Version, OldText: f.Value})
	if err != nil {
		return false, fmt.Errorf("rotate .env file: %w", err)
	}
	return n == 1, nil
}

// CountByVersion counts secrets and .env files together by key version.
func (r *Repository) CountByVersion(ctx context.Context) (map[int32]int64, error) {
	q := dbgen.New(r.pool)
	rows, err := q.CountSecretsByKeyVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("count secrets by key version: %w", err)
	}
	counts := make(map[int32]int64, len(rows))
	for _, row := range rows {
		counts[row.KeyVersion] = row.Secrets
	}
	files, err := q.CountEnvFilesByKeyVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("count .env files by key version: %w", err)
	}
	for _, row := range files {
		counts[row.KeyVersion] += row.Files
	}
	return counts, nil
}
