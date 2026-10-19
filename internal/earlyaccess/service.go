// Package earlyaccess owns operator approval of new workspaces, independently
// of customer roles. Existing workspaces are grandfathered by the migration.
package earlyaccess

import (
	"context"
	"errors"
	"fmt"

	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrUnavailable = errors.New("early-access request is unavailable or changed; refresh and try again")

type Service struct {
	pool      *pgxpool.Pool
	enabled   bool
	operators map[int64]bool
}

func NewService(pool *pgxpool.Pool, enabled bool, operators []int64) *Service {
	s := &Service{pool: pool, enabled: enabled, operators: map[int64]bool{}}
	for _, id := range operators {
		if id > 0 {
			s.operators[id] = true
		}
	}
	return s
}
func (s *Service) Operator(id int64) bool { return s.operators[id] }

// ApprovedTx runs under the caller's scope/usage locks. It never locks a
// workspace after a usage lock (the opposite order is used by deletion).
func (s *Service) ApprovedTx(ctx context.Context, tx pgx.Tx, workspace int64) (bool, error) {
	if !s.enabled {
		return true, nil
	}
	var approved bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE id=$1 AND state='active' AND early_access_state='approved')`, workspace).Scan(&approved); err != nil {
		return false, fmt.Errorf("read workspace approval: %w", err)
	}
	return approved, nil
}
func (s *Service) Allowed(ctx context.Context, workspace int64) (bool, error) {
	if !s.enabled {
		return true, nil
	}
	var approved bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE id=$1 AND state='active' AND early_access_state='approved')`, workspace).Scan(&approved); err != nil {
		return false, fmt.Errorf("read early-access admission: %w", err)
	}
	return approved, nil
}
func (s *Service) Approve(ctx context.Context, operator int64, public string, revision int64) error {
	if !s.Operator(operator) || revision < 1 {
		return ErrUnavailable
	}
	return database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		var id int64
		var current int64
		var state string
		err := tx.QueryRow(ctx, `SELECT id,revision,early_access_state FROM workspaces WHERE public_id::text=$1 AND state='active' FOR UPDATE`, public).Scan(&id, &current, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnavailable
		}
		if err != nil {
			return fmt.Errorf("lock workspace approval: %w", err)
		}
		if state == "approved" {
			return nil
		}
		if current != revision {
			return ErrUnavailable
		}
		if _, err = tx.Exec(ctx, `INSERT INTO workspace_approvals(workspace_id,operator_github_id) VALUES($1,$2)`, id, operator); err != nil {
			return fmt.Errorf("audit workspace approval: %w", err)
		}
		if _, err = tx.Exec(ctx, `UPDATE workspaces SET early_access_state='approved',revision=revision+1 WHERE id=$1`, id); err != nil {
			return fmt.Errorf("approve workspace: %w", err)
		}
		return nil
	})
}
