package environments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/ajaykathwatee/previa/internal/environments/dbgen"
)

type ResourceInventory interface {
	Resources(context.Context, int64) ([]Resource, error)
}

// Reconciler repairs lost operation wake-ups and schedules cleanup for labeled
// runtime resources that are not referenced by an active or pending environment.
// Inventory calls run outside database transactions. Route observation and route
// repair are intentionally left to the future generation-fenced router adapter.
type Reconciler struct {
	repo      *Repository
	inventory ResourceInventory
	logger    *slog.Logger
	batchSize int32
	afterID   int64
}

func NewReconciler(repo *Repository, inventory ResourceInventory, logger *slog.Logger, batchSize int32) (*Reconciler, error) {
	if repo == nil || inventory == nil || logger == nil {
		return nil, errors.New("environment reconciler requires repository, inventory, and logger")
	}
	if batchSize <= 0 {
		return nil, errors.New("environment reconciler batch size must be positive")
	}
	return &Reconciler{repo: repo, inventory: inventory, logger: logger, batchSize: batchSize}, nil
}

// RunOnce is bounded: it recovers a batch of outbox operations and inventories a
// batch of PRs. Call it periodically; the in-memory cursor only avoids repeatedly
// scanning the first page during one process lifetime, while durable operation
// rows make restarts safe.
func (r *Reconciler) RunOnce(ctx context.Context) error {
	if _, err := r.repo.RecoverOperations(ctx, r.batchSize); err != nil {
		return err
	}
	rows, err := dbgen.New(r.repo.pool).ListPullRequestsForReconciliation(ctx, dbgen.ListPullRequestsForReconciliationParams{
		AfterID: r.afterID, BatchSize: r.batchSize,
	})
	if err != nil {
		return fmt.Errorf("list PRs for resource reconciliation: %w", err)
	}
	if len(rows) == 0 {
		r.afterID = 0
		return nil
	}
	for _, row := range rows {
		request := NewRequest(row.ID, row.Generation, row.HeadSha, row.State)
		resources, err := r.inventory.Resources(ctx, row.ID)
		if err != nil {
			return fmt.Errorf("inspect resources for PR %d: %w", row.ID, err)
		}
		if err := r.repo.ScheduleResourceCleanup(ctx, request, resources); err != nil {
			return fmt.Errorf("schedule orphan cleanup for PR %d: %w", row.ID, err)
		}
		r.afterID = row.ID
	}
	r.logger.InfoContext(ctx, "environment reconciliation pass completed", "last_pr_id", r.afterID, "prs_inspected", len(rows))
	return nil
}
