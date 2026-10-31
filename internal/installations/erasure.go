package installations

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// EraseRepositoryTx removes repository-owned records through their foreign-key
// ownership. Callers must first verify route, runtime and baseline cleanup.
func (r *Repository) EraseRepositoryTx(ctx context.Context, tx pgx.Tx, repo int64) error {
	if _, err := tx.Exec(ctx, `DELETE FROM repos WHERE id=$1 AND disconnected_at IS NOT NULL`, repo); err != nil {
		return fmt.Errorf("erase disconnected repository: %w", err)
	}
	var remains bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM repos WHERE id=$1)`, repo).Scan(&remains); err != nil {
		return fmt.Errorf("verify repository erasure: %w", err)
	}
	if remains {
		return fmt.Errorf("repository is not fenced for erasure")
	}
	return nil
}
func (r *Repository) EraseInstallationTx(ctx context.Context, tx pgx.Tx, installation int64) error {
	if _, err := tx.Exec(ctx, `DELETE FROM installations WHERE id=$1 AND disconnected_at IS NOT NULL AND NOT EXISTS(SELECT 1 FROM repos WHERE installation_id=$1)`, installation); err != nil {
		return fmt.Errorf("erase disconnected installation: %w", err)
	}
	var remains bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM installations WHERE id=$1)`, installation).Scan(&remains); err != nil {
		return fmt.Errorf("verify installation erasure: %w", err)
	}
	if remains {
		return fmt.Errorf("installation still owns repository data")
	}
	return nil
}
