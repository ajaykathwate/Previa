package workspaces

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/workspaces/dbgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrUnavailable       = errors.New("workspace access is unavailable")
	ErrConflict          = errors.New("workspace changed; reload and try again")
	ErrInput             = errors.New("invalid workspace input")
	ErrOwner             = errors.New("transfer ownership before leaving or removing the owner")
	ErrInvitation        = errors.New("invitation is expired, revoked, or addressed to another GitHub account")
	ErrSourceUnavailable = errors.New("GitHub source permission could not be verified")
)

type Workspace struct {
	ID, OwnerID, Revision, MembershipRevision int64
	ProfileRevision, LogoVersion              int64
	Name, Role, Description, OwnerLogin       string
	CreatedAt                                 time.Time
}

func (w Workspace) Manage() bool { return w.Role == "owner" || w.Role == "admin" }

type Repository struct{ pool *pgxpool.Pool }

func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

func accessError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUnavailable
	}
	return fmt.Errorf("read workspace: %w", err)
}
func (r *Repository) List(ctx context.Context, user int64) ([]Workspace, error) {
	rows, err := dbgen.New(r.pool).ListWorkspaces(ctx, user)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	out := make([]Workspace, 0, len(rows))
	for _, v := range rows {
		out = append(out, Workspace{ID: v.ID, OwnerID: v.OwnerUserID, Revision: v.Revision, MembershipRevision: v.MembershipRevision,
			Name: v.Name, Role: v.Role, Description: v.Description, ProfileRevision: v.ProfileRevision, LogoVersion: v.LogoVersion, OwnerLogin: v.OwnerLogin, CreatedAt: v.CreatedAt.Time})
	}
	return out, nil
}
func (r *Repository) Read(ctx context.Context, user, id int64) (Workspace, error) {
	v, err := dbgen.New(r.pool).ReadWorkspace(ctx, dbgen.ReadWorkspaceParams{WorkspaceID: id, UserID: user})
	if err != nil {
		return Workspace{}, accessError(err)
	}
	return workspaceFromRow(v), nil
}
func (r *Repository) Preference(ctx context.Context, user int64) (int64, error) {
	id, err := dbgen.New(r.pool).WorkspacePreference(ctx, user)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read workspace selection: %w", err)
	}
	return id, nil
}
func (r *Repository) Select(ctx context.Context, user, id int64) error {
	return database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, err := q.LockWorkspace(ctx, id); err != nil {
			return accessError(err)
		}
		if _, err := q.ReadWorkspace(ctx, dbgen.ReadWorkspaceParams{WorkspaceID: id, UserID: user}); err != nil {
			return accessError(err)
		}
		return q.SaveWorkspacePreference(ctx, dbgen.SaveWorkspacePreferenceParams{UserID: user, WorkspaceID: nullableID(id)})
	})
}

// Mutation serializes access changes with all existing installation → PR locks.
// No GitHub call is performed while these locks are held.
func (r *Repository) mutation(ctx context.Context, user, id, revision int64, fn func(pgx.Tx, *dbgen.Queries, Workspace) error) error {
	return database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, err := q.LockWorkspace(ctx, id); err != nil {
			return accessError(err)
		}
		actor, err := q.ReadWorkspace(ctx, dbgen.ReadWorkspaceParams{WorkspaceID: id, UserID: user})
		if err != nil {
			return accessError(err)
		}
		if revision > 0 && actor.Revision != revision {
			return ErrConflict
		}
		if _, err := q.LockWorkspaceInstallations(ctx, nullableID(id)); err != nil {
			return fmt.Errorf("lock workspace installations: %w", err)
		}
		return fn(tx, q, workspaceFromRow(actor))
	})
}

func workspaceFromRow(v dbgen.ReadWorkspaceRow) Workspace {
	return Workspace{ID: v.ID, OwnerID: v.OwnerUserID, Revision: v.Revision, MembershipRevision: v.MembershipRevision,
		Name: v.Name, Role: v.Role, Description: v.Description, ProfileRevision: v.ProfileRevision, LogoVersion: v.LogoVersion, OwnerLogin: v.OwnerLogin, CreatedAt: v.CreatedAt.Time}
}

func (r *Repository) mutationForInvitation(ctx context.Context, wid int64, fn func(*dbgen.Queries) error) error {
	return database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, err := q.LockWorkspace(ctx, wid); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrInvitation
			}
			return fmt.Errorf("lock invitation workspace: %w", err)
		}
		if _, err := q.LockWorkspaceInstallations(ctx, nullableID(wid)); err != nil {
			return fmt.Errorf("lock invited workspace: %w", err)
		}
		return fn(q)
	})
}

type ReportRecorder interface {
	RecordReportTx(context.Context, pgx.Tx, int64) error
}
type Directory interface {
	LookupGitHubUser(context.Context, string) (int64, string, error)
}
type Permissions interface {
	RepositoryUserPermission(context.Context, int64, string, string, int64) (string, error)
}
type Service struct {
	deletion    WorkspaceDeleter
	repo        *Repository
	directory   Directory
	permissions Permissions
	reports     ReportRecorder
	now         func() time.Time
}

func NewService(repo *Repository, d Directory, p Permissions, reports ReportRecorder) *Service {
	return &Service{repo: repo, directory: d, permissions: p, reports: reports, now: time.Now}
}
func (s *Service) List(ctx context.Context, user int64) ([]Workspace, error) {
	return s.repo.List(ctx, user)
}
func (s *Service) Read(ctx context.Context, user, id int64) (Workspace, error) {
	return s.repo.Read(ctx, user, id)
}
func (s *Service) Select(ctx context.Context, user, id int64) error {
	return s.repo.Select(ctx, user, id)
}
