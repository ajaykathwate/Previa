package webhooks

import (
	"context"
	"errors"
	"fmt"

	"github.com/ajaykathwatee/previa/internal/installations"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/google/go-github/v91/github"
	"github.com/jackc/pgx/v5"
)

// Reconcile shares lifecycle(), including source tombstones, RevokeTx, cleanup
// pending and the teardown sweep. All external IO happened before this call.
func (s *DeliveryService) Reconcile(ctx context.Context, changes []installations.ReconciliationChange) ([]installations.ReconciliationChange, error) {
	if s.revoker == nil {
		return nil, errors.New("installation reconciliation requires the lifecycle revoker")
	}
	var applied []installations.ReconciliationChange
	err := database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		for _, c := range changes {
			fresh, err := s.reconcileOne(ctx, tx, c)
			if err != nil {
				return err
			}
			if fresh {
				applied = append(applied, c)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if s.invalidate != nil {
		for _, c := range applied {
			if c.Status != "" || len(c.Removed) > 0 {
				s.invalidate(c.ID)
			}
		}
	}
	return applied, nil
}

func (s *DeliveryService) reconcileOne(ctx context.Context, tx pgx.Tx, c installations.ReconciliationChange) (bool, error) {
	var revision int64
	err := tx.QueryRow(ctx, `SELECT revision FROM installations WHERE github_installation_id=$1 FOR UPDATE`, c.ID).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock reconciliation revision: %w", err)
	}
	if revision != c.Revision {
		return false, nil
	}
	if event := reconciliationEvent(c); event != nil {
		if err = s.lifecycle(ctx, tx, event, c.Observed); err != nil {
			return false, err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE installations SET reconciled_at=$2 WHERE github_installation_id=$1`, c.ID, c.Observed); err != nil {
		return false, fmt.Errorf("record installation reconciliation time: %w", err)
	}
	return true, nil
}
func reconciliationEvent(c installations.ReconciliationChange) any {
	i := &github.Installation{ID: new(c.ID), UpdatedAt: &github.Timestamp{Time: c.Observed}}
	if c.Status != "" {
		action := "suspend"
		if c.Status == "inactive" {
			action = "deleted"
		}
		return &github.InstallationEvent{Action: new(action), Installation: i}
	}
	if len(c.Removed) == 0 {
		return nil
	}
	repos := make([]*github.Repository, 0, len(c.Removed))
	for _, repo := range c.Removed {
		repos = append(repos, &github.Repository{ID: new(repo.ID), FullName: new(repo.FullName)})
	}
	return &github.InstallationRepositoriesEvent{Action: new("removed"), Installation: i, RepositoriesRemoved: repos}
}
