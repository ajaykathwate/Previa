package usage

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// reconcileTx bounds unobserved crash consumption by the final reservation.
// An admitted attempt that never began execution consumes zero. No recovery
// can infer the exact instant a process died; those attempts stay provisional.
func (s *Store) reconcileTx(ctx context.Context, tx pgx.Tx, workspace int64, now time.Time) error {
	rows, err := tx.Query(ctx, `SELECT a.attempt_key::text,a.state FROM workspace_usage_attempts a
        WHERE a.workspace_id=$1 AND a.state IN ('reserved','running') AND NOT EXISTS (
        SELECT 1 FROM workspace_usage_segments s WHERE s.attempt_key=a.attempt_key AND s.ended_at IS NULL AND s.reserved_until>$2)`, workspace, now)
	if err != nil {
		return fmt.Errorf("find expired execution reservations: %w", err)
	}
	type expired struct{ key, state string }
	var all []expired
	for rows.Next() {
		var a expired
		if err = rows.Scan(&a.key, &a.state); err != nil {
			rows.Close()
			return fmt.Errorf("scan expired reservation: %w", err)
		}
		all = append(all, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("read expired reservations: %w", err)
	}
	for _, a := range all {
		if err = settle(ctx, tx, a.key, now, a.state == "running"); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE workspace_usage_attempts SET state='reconciled',outcome='crash',provisional=($2='running'),finished_at=$3 WHERE attempt_key=$1`, a.key, a.state, now)
		if err != nil {
			return fmt.Errorf("reconcile crashed execution: %w", err)
		}
	}
	return nil
}

// Reconcile is bounded and safe alongside workers. Admission also reconciles
// its workspace, so a stopped periodic worker cannot leave an expired hold.
func (s *Store) Reconcile(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT workspace_id FROM workspace_usage_attempts a WHERE state IN ('reserved','running')
        AND NOT EXISTS (SELECT 1 FROM workspace_usage_segments s WHERE s.attempt_key=a.attempt_key AND s.ended_at IS NULL AND s.reserved_until>clock_timestamp())
        ORDER BY workspace_id LIMIT 100`)
	if err != nil {
		return fmt.Errorf("list usage recovery workspaces: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan usage workspace: %w", err)
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("read usage workspaces: %w", err)
	}
	for _, id := range ids {
		if err = s.transaction(ctx, func(tx pgx.Tx) error {
			if _, err := lock(ctx, tx, id); err != nil {
				return err
			}
			return s.reconcileTx(ctx, tx, id, s.now().UTC())
		}); err != nil {
			return err
		}
	}
	return nil
}
