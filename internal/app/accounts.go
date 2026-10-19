package app

import (
	"context"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/sampledata"
	"github.com/ajaykathwatee/previa/internal/support"
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
func newSupport(pool *pgxpool.Pool, c config.CustomerConfig) *support.Service {
	ids, _ := c.OperatorIDs() // LoadServer/LoadWorker already reject malformed IDs.
	return support.NewService(pool, support.Settings{Email: c.SupportEmail, Operator: c.OperatorName, Operators: ids, MaxBytes: c.SupportMessageMaxBytes, PerDay: c.SupportRequestsPerDay, RetentionDays: c.SupportRetentionDays, BackupDays: c.BackupRetentionDays, JournalDays: c.DeletionJournalDays})
}
