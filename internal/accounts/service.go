// Package accounts coordinates durable local disconnection and erasure.
// Feature owners fence and erase their own data through small contracts.
package accounts

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrUnavailable  = errors.New("this account action is unavailable")
	ErrConflict     = errors.New("this resource changed; refresh before confirming again")
	ErrConfirmation = errors.New("type DELETE to delete, or DISCONNECT to disconnect")
	ErrOwnership    = errors.New("transfer or finish deleting every workspace you own before deleting your account")
)

type RepositoryLifecycle interface {
	FenceRepositoryTx(context.Context, pgx.Tx, int64) error
	RepositoryDrained(context.Context, int64) (bool, error)
}
type RepositoryEraser interface {
	EraseRepositoryTx(context.Context, pgx.Tx, int64) error
}
type RepositoryCleaner interface {
	RepositoryDrained(context.Context, int64) (bool, error)
}
type InstallationEraser interface {
	EraseInstallationTx(context.Context, pgx.Tx, int64) error
}
type WorkspaceEraser interface {
	EraseWorkspaceTx(context.Context, pgx.Tx, int64) error
}
type AccountEraser interface {
	EraseAccountTx(context.Context, pgx.Tx, int64) error
}
type AccountFence interface {
	FenceAccountTx(context.Context, pgx.Tx, int64) error
}
type FinalComment interface {
	Disconnected(context.Context, int64) (bool, error)
}

type Service struct {
	pool          *pgxpool.Pool
	previews      RepositoryLifecycle
	repositories  []RepositoryEraser
	people        []AccountEraser
	accountFences []AccountFence
	comments      FinalComment
	cleaners      []RepositoryCleaner
	installations InstallationEraser
	workspaces    []WorkspaceEraser
}

func NewService(pool *pgxpool.Pool) *Service                            { return &Service{pool: pool} }
func (s *Service) WithPreviews(p RepositoryLifecycle) *Service          { s.previews = p; return s }
func (s *Service) WithRepositoryErasers(e ...RepositoryEraser) *Service { s.repositories = e; return s }
func (s *Service) WithAccountErasers(e ...AccountEraser) *Service       { s.people = e; return s }
func (s *Service) WithAccountFences(e ...AccountFence) *Service         { s.accountFences = e; return s }
func (s *Service) WithComments(c FinalComment) *Service                 { s.comments = c; return s }
func (s *Service) WithCleaners(c ...RepositoryCleaner) *Service         { s.cleaners = c; return s }
func (s *Service) WithInstallations(e InstallationEraser) *Service      { s.installations = e; return s }
func (s *Service) WithWorkspaces(e ...WorkspaceEraser) *Service         { s.workspaces = e; return s }

type Operation struct {
	ID, Kind, TargetPublicID, Stage, Error string
	Actor, Target, Workspace               int64
	Created, Available                     time.Time
	Completed, JournalAcknowledged         *time.Time
}

func (s *Service) operation(ctx context.Context, tx pgx.Tx, kind string, target, workspace, actor int64, public string, parent *string) (Operation, error) {
	var o Operation
	err := tx.QueryRow(ctx, `INSERT INTO lifecycle_operations(kind,target_id,target_public_id,workspace_id,actor_id,parent_id)
		VALUES($1,$2,$3,nullif($4,0),$5,$6) ON CONFLICT(kind,target_public_id) DO UPDATE SET parent_id=coalesce(lifecycle_operations.parent_id,EXCLUDED.parent_id)
		RETURNING id::text,kind,target_id,target_public_id::text,coalesce(workspace_id,0),actor_id,stage,last_error,created_at,available_at,completed_at,journal_ack_at`, kind, target, public, workspace, actor, parent).
		Scan(&o.ID, &o.Kind, &o.Target, &o.TargetPublicID, &o.Workspace, &o.Actor, &o.Stage, &o.Error, &o.Created, &o.Available, &o.Completed, &o.JournalAcknowledged)
	if err != nil {
		return o, fmt.Errorf("record account operation: %w", err)
	}
	return o, nil
}

func (s *Service) Read(ctx context.Context, actor int64, id string) (Operation, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Operation{}, ErrUnavailable
	}
	var o Operation
	err := s.pool.QueryRow(ctx, `SELECT id::text,kind,target_id,target_public_id::text,coalesce(workspace_id,0),actor_id,stage,last_error,created_at,available_at,completed_at,journal_ack_at
		FROM lifecycle_operations o WHERE id=$1 AND (actor_id=$2 OR EXISTS(SELECT 1 FROM workspaces w JOIN workspace_memberships m ON m.workspace_id=w.id
		WHERE w.id=o.workspace_id AND m.user_id=$2 AND m.state='active' AND m.role IN ('owner','admin')))`, id, actor).
		Scan(&o.ID, &o.Kind, &o.Target, &o.TargetPublicID, &o.Workspace, &o.Actor, &o.Stage, &o.Error, &o.Created, &o.Available, &o.Completed, &o.JournalAcknowledged)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, ErrUnavailable
	}
	if err != nil {
		return o, fmt.Errorf("read account operation: %w", err)
	}
	return o, nil
}

func (s *Service) transaction(ctx context.Context, fn func(pgx.Tx) error) error {
	return database.InTransaction(ctx, s.pool, fn)
}

func unavailableOrError(operation string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUnavailable
	}
	return fmt.Errorf("%s: %w", operation, err)
}
