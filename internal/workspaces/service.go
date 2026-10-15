package workspaces

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/workspaces/dbgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func nullableID(id int64) pgtype.Int8 { return pgtype.Int8{Int64: id, Valid: id > 0} }
func validName(name string) bool {
	return name != "" && utf8.RuneCountInString(name) <= 80 && !strings.ContainsAny(name, "\x00\r\n")
}
func validRole(role string) bool { return role == "admin" || role == "developer" || role == "reviewer" }

func (s *Service) Create(ctx context.Context, user int64, name string) (id int64, err error) {
	name = strings.TrimSpace(name)
	if user <= 0 || !validName(name) {
		return 0, ErrInput
	}
	err = database.InTransaction(ctx, s.repo.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		var e error
		id, e = q.CreateWorkspace(ctx, dbgen.CreateWorkspaceParams{Name: name, OwnerUserID: user})
		if e != nil {
			return fmt.Errorf("create workspace: %w", e)
		}
		if e = q.CreateOwnerMembership(ctx, dbgen.CreateOwnerMembershipParams{WorkspaceID: id, UserID: user}); e != nil {
			return fmt.Errorf("create owner membership: %w", e)
		}
		return q.SaveWorkspacePreference(ctx, dbgen.SaveWorkspacePreferenceParams{UserID: user, WorkspaceID: nullableID(id)})
	})
	return id, err
}

// Authorize derives tenant and permission from the resource, never the selector.
func (s *Service) Authorize(ctx context.Context, user, repo int64, ability string, source bool) (dbgen.ReadRepositoryAccessRow, error) {
	v, err := dbgen.New(s.repo.pool).ReadRepositoryAccess(ctx, dbgen.ReadRepositoryAccessParams{RepoID: repo, UserID: user, Ability: ability})
	if err != nil {
		return v, accessError(err)
	}
	if !v.Allowed {
		return v, ErrUnavailable
	}
	if !source {
		return v, nil
	}
	if s.permissions == nil {
		return v, ErrSourceUnavailable
	}
	role, err := s.permissions.RepositoryUserPermission(ctx, v.GithubInstallationID, v.FullName, v.Username, v.GithubID)
	if err != nil {
		return v, fmt.Errorf("verify source permission: %w", errors.Join(ErrSourceUnavailable, err))
	}
	if role != "read" && role != "triage" && role != "write" && role != "maintain" && role != "admin" {
		return v, ErrUnavailable
	}
	return v, nil
}

// AuthorizeBuild is consumed by the runtime at protected execution boundaries.
func (s *Service) AuthorizeBuild(ctx context.Context, prID int64) (bool, error) {
	v, err := dbgen.New(s.repo.pool).BuildRequestActor(ctx, prID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, accessError(err)
	}
	if !v.Authorized {
		return false, nil
	}
	if v.Source == "automatic" {
		return true, nil
	}
	_, err = s.Authorize(ctx, v.RequestedBy.Int64, v.RepoID, "operate", true)
	if errors.Is(err, ErrUnavailable) {
		return false, nil
	}
	return err == nil, err
}

func (s *Service) cancelRevoked(ctx context.Context, tx pgx.Tx, q *dbgen.Queries, id int64) error {
	if err := q.RevokeUnauthorizedInvitations(ctx, id); err != nil {
		return fmt.Errorf("revoke unauthorized invitations: %w", err)
	}
	prs, err := q.CancelRevokedRequests(ctx, nullableID(id))
	if err != nil {
		return fmt.Errorf("cancel revoked build requests: %w", err)
	}
	if s.reports != nil {
		for _, pr := range prs {
			if err := s.reports.RecordReportTx(ctx, tx, pr); err != nil {
				return err
			}
		}
	}
	return q.AdvanceWorkspaceRevision(ctx, id)
}

func replaceGrants(ctx context.Context, q *dbgen.Queries, wid, user int64, repos []int64) error {
	if len(repos) > 500 {
		return ErrInput
	}
	if err := q.ClearRepositoryGrants(ctx, dbgen.ClearRepositoryGrantsParams{WorkspaceID: wid, UserID: user}); err != nil {
		return fmt.Errorf("clear repository grants: %w", err)
	}
	seen := make(map[int64]bool)
	for _, repo := range repos {
		if repo <= 0 || seen[repo] {
			continue
		}
		seen[repo] = true
		n, err := q.PutRepositoryGrant(ctx, dbgen.PutRepositoryGrantParams{RepoID: repo, WorkspaceID: wid, UserID: user})
		if err != nil {
			return fmt.Errorf("save repository grant: %w", err)
		}
		if n != 1 {
			return ErrUnavailable
		}
	}
	return nil
}

func (s *Service) CanReadSource(ctx context.Context, user, repo int64) (bool, error) {
	_, err := s.Authorize(ctx, user, repo, "operate", true)
	if errors.Is(err, ErrUnavailable) {
		return false, nil
	}
	return err == nil, err
}

// SharesWorkspace reports whether two people are active members of a common
// workspace; auth uses it to show uploaded profile pictures to teammates.
func (s *Service) SharesWorkspace(ctx context.Context, viewer, subject int64) (bool, error) {
	shared, err := dbgen.New(s.repo.pool).SharesWorkspace(ctx, dbgen.SharesWorkspaceParams{ViewerID: viewer, SubjectID: subject})
	if err != nil {
		return false, fmt.Errorf("check shared workspace: %w", err)
	}
	return shared, nil
}
