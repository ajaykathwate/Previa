package workspaces

import (
	"context"
	"fmt"

	"github.com/ajaykathwatee/previa/internal/workspaces/dbgen"
	"github.com/jackc/pgx/v5"
)

type WorkspaceDeleter interface {
	DeleteWorkspace(context.Context, int64, int64, int64, string) error
}

// WithDeletion replaces the old solo-workspace deletion with durable erasure.
func (s *Service) WithDeletion(d WorkspaceDeleter) *Service { s.deletion = d; return s }

func (s *Service) EraseWorkspaceTx(ctx context.Context, tx pgx.Tx, workspace int64) error {
	for _, query := range []string{
		`DELETE FROM workspace_invitations WHERE workspace_id=$1`,
		`DELETE FROM workspace_transfers WHERE workspace_id=$1`,
		`DELETE FROM workspace_logos WHERE workspace_id=$1`,
		`DELETE FROM workspace_memberships WHERE workspace_id=$1`,
		`DELETE FROM workspace_entitlements WHERE workspace_id=$1`,
		`DELETE FROM workspace_build_usage WHERE workspace_id=$1`,
		`DELETE FROM activity_events WHERE workspace_id=$1`,
		`UPDATE workspaces SET state='deleted',name='Deleted workspace',description='',deleted_at=clock_timestamp() WHERE id=$1 AND state='deleting'`,
	} {
		if _, err := tx.Exec(ctx, query, workspace); err != nil {
			return fmt.Errorf("erase workspace data: %w", err)
		}
	}
	return nil
}

func (s *Service) EraseAccountTx(ctx context.Context, tx pgx.Tx, user int64) error {
	for _, query := range []string{
		`DELETE FROM repository_memberships WHERE user_id=$1`,
		`DELETE FROM workspace_memberships WHERE user_id=$1`,
		`DELETE FROM workspace_preferences WHERE user_id=$1`,
		`DELETE FROM workspace_invitations WHERE github_id=(SELECT github_id FROM users WHERE id=$1)`,
	} {
		if _, err := tx.Exec(ctx, query, user); err != nil {
			return fmt.Errorf("erase personal workspace access: %w", err)
		}
	}
	return nil
}

// FenceAccountTx repairs outstanding manual requests in workspaces the deleted
// identity left. Serving deployments belong to the team and remain intact.
func (s *Service) FenceAccountTx(ctx context.Context, tx pgx.Tx, user int64) error {
	rows, err := tx.Query(ctx, `SELECT workspace_id FROM workspace_memberships WHERE user_id=$1 ORDER BY workspace_id`, user)
	if err != nil {
		return fmt.Errorf("list former account workspaces: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return fmt.Errorf("read former account workspaces: %w", err)
	}
	for _, id := range ids {
		if err := s.cancelRevoked(ctx, tx, dbgen.New(tx), id); err != nil {
			return err
		}
	}
	return nil
}
