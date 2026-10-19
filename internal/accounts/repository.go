package accounts

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type Workspace struct{ ID, Name, Role, Revision string }
type Connection struct{ ID, Name, Revision, Workspace string }
type OperationSummary struct{ ID, Kind, Stage, Created string }
type Catalog struct {
	Workspaces                  []Workspace
	Installations, Repositories []Connection
	Operations                  []OperationSummary
}

func (s *Service) Catalog(ctx context.Context, u int64) (Catalog, error) {
	var out Catalog
	rows, err := s.pool.Query(ctx, `SELECT w.public_id::text,w.name,m.role,w.revision::text FROM workspaces w JOIN workspace_memberships m ON m.workspace_id=w.id WHERE m.user_id=$1 AND m.state='active' AND w.state='active' ORDER BY w.name,w.id`, u)
	if err != nil {
		return out, fmt.Errorf("list account workspaces: %w", err)
	}
	out.Workspaces, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Workspace])
	if err != nil {
		return out, fmt.Errorf("read account workspaces: %w", err)
	}
	rows, err = s.pool.Query(ctx, `SELECT i.public_id::text,i.account_login,i.revision::text,w.name FROM installations i JOIN workspaces w ON w.id=i.workspace_id JOIN workspace_memberships m ON m.workspace_id=w.id AND m.user_id=$1 AND m.state='active' AND m.role IN ('owner','admin') WHERE w.state='active' AND i.disconnected_at IS NULL ORDER BY i.id`, u)
	if err != nil {
		return out, fmt.Errorf("list account installations: %w", err)
	}
	out.Installations, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Connection])
	if err != nil {
		return out, fmt.Errorf("read account installations: %w", err)
	}
	rows, err = s.pool.Query(ctx, `SELECT r.public_id::text,r.full_name,i.revision::text,w.name FROM repos r JOIN installations i ON i.id=r.installation_id JOIN workspaces w ON w.id=i.workspace_id JOIN workspace_memberships m ON m.workspace_id=w.id AND m.user_id=$1 AND m.state='active' AND m.role IN ('owner','admin') WHERE w.state='active' AND r.disconnected_at IS NULL AND i.disconnected_at IS NULL ORDER BY r.full_name,r.id`, u)
	if err != nil {
		return out, fmt.Errorf("list account repositories: %w", err)
	}
	out.Repositories, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Connection])
	if err != nil {
		return out, fmt.Errorf("read account repositories: %w", err)
	}
	rows, err = s.pool.Query(ctx, `SELECT id::text,kind,stage,created_at::text FROM lifecycle_operations WHERE actor_id=$1 ORDER BY created_at DESC LIMIT 100`, u)
	if err != nil {
		return out, fmt.Errorf("list account operations: %w", err)
	}
	out.Operations, err = pgx.CollectRows(rows, pgx.RowToStructByPos[OperationSummary])
	if err != nil {
		return out, fmt.Errorf("read account operations: %w", err)
	}

	return out, nil
}
