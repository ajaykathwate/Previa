package earlyaccess

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
)

type Workspace struct {
	ID, Name, Owner, State string
	Revision               int64
}

func (s *Service) Workspace(ctx context.Context, user, id int64) (Workspace, error) {
	var w Workspace
	err := s.pool.QueryRow(ctx, `SELECT w.public_id::text,w.name,u.username,w.early_access_state,w.revision FROM workspaces w
	 JOIN workspace_memberships m ON m.workspace_id=w.id JOIN users u ON u.id=w.owner_user_id
	 WHERE w.id=$1 AND m.user_id=$2 AND m.state='active' AND w.state='active'`, id, user).Scan(&w.ID, &w.Name, &w.Owner, &w.State, &w.Revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return w, ErrUnavailable
	}
	if err != nil {
		return w, fmt.Errorf("read workspace admission: %w", err)
	}
	return w, nil
}
func (s *Service) Pending(ctx context.Context, operator int64, offset int) ([]Workspace, bool, error) {
	if !s.Operator(operator) {
		return nil, false, ErrUnavailable
	}
	rows, err := s.pool.Query(ctx, `SELECT w.public_id::text,w.name,u.username,w.early_access_state,w.revision FROM workspaces w JOIN users u ON u.id=w.owner_user_id
	 WHERE w.state='active' AND w.early_access_state='pending' ORDER BY w.created_at,w.id LIMIT 26 OFFSET $1`, offset)
	if err != nil {
		return nil, false, fmt.Errorf("list pending workspace approvals: %w", err)
	}
	values, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Workspace])
	if err != nil {
		return nil, false, fmt.Errorf("read pending workspace approvals: %w", err)
	}
	more := len(values) > 25
	if more {
		values = values[:25]
	}
	return values, more, nil
}
