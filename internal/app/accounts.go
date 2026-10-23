package app

import (
	"context"
	"errors"

	"github.com/ajaykathwatee/previa/internal/accounts"
	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/sampledata"
	"github.com/ajaykathwatee/previa/internal/support"
	"github.com/ajaykathwatee/previa/internal/workspaces"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Each feature owns its fence. The composition root only sequences them in
// the same transaction so previews and baseline refreshes stop together.
type repositoryLifecycle struct {
	previews *environments.Lifecycle
	data     *sampledata.Lifecycle
}

func (l repositoryLifecycle) FenceRepositoryTx(ctx context.Context, tx pgx.Tx, repo int64) error {
	if err := l.previews.FenceRepositoryTx(ctx, tx, repo); err != nil {
		return err
	}
	return l.data.FenceRepositoryTx(ctx, tx, repo)
}
func (l repositoryLifecycle) RepositoryDrained(ctx context.Context, repo int64) (bool, error) {
	return l.previews.RepositoryDrained(ctx, repo)
}

// workspaceDeletion lets the workspace route delete through the account
// lifecycle while answering with the workspace feature's own refusals: a
// non-owner gets 404 and a wrong confirmation or stale revision 409, not an
// unexplained 503.
type workspaceDeletion struct{ lifecycle *accounts.Service }

func (d workspaceDeletion) DeleteWorkspace(ctx context.Context, user, workspace, revision int64, confirm string) error {
	err := d.lifecycle.DeleteWorkspace(ctx, user, workspace, revision, confirm)
	switch {
	case errors.Is(err, accounts.ErrUnavailable):
		return workspaces.ErrUnavailable
	case errors.Is(err, accounts.ErrConfirmation):
		return workspaces.ErrDeleteConfirmation
	case errors.Is(err, accounts.ErrConflict):
		return workspaces.ErrConflict
	}
	return err
}
func newSupport(pool *pgxpool.Pool, c config.CustomerConfig) *support.Service {
	ids, _ := c.OperatorIDs() // LoadServer/LoadWorker already reject malformed IDs.
	return support.NewService(pool, support.Settings{Email: c.SupportEmail, Operator: c.OperatorName, Operators: ids, MaxBytes: c.SupportMessageMaxBytes, PerDay: c.SupportRequestsPerDay, RetentionDays: c.SupportRetentionDays, BackupDays: c.BackupRetentionDays, JournalDays: c.DeletionJournalDays})
}
