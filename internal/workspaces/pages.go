package workspaces

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ajaykathwatee/previa/internal/workspaces/dbgen"
	pages "github.com/ajaykathwatee/previa/web/pages/workspaces"
	"github.com/jackc/pgx/v5"
)

type PageQuery struct {
	Page                     int64
	Search                   string
	Invitations, Deactivated bool
}

func (s *Service) Page(ctx context.Context, user, wid int64, tab string, filter PageQuery) (result pages.Page, resultErr error) {
	filter.Search = strings.TrimSpace(filter.Search)
	if utf8.RuneCountInString(filter.Search) > 100 {
		return pages.Page{}, ErrInput
	}
	if tab != "members" && tab != "settings" && tab != "setup" {
		return pages.Page{}, ErrInput
	}
	tx, err := s.repo.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return pages.Page{}, fmt.Errorf("open workspace view: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := tx.Rollback(cleanupCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			resultErr = errors.Join(resultErr, fmt.Errorf("close workspace view: %w", err))
		}
	}()
	q := dbgen.New(tx)
	w, err := q.ReadWorkspace(ctx, dbgen.ReadWorkspaceParams{UserID: user, WorkspaceID: wid})
	if err != nil {
		return pages.Page{}, accessError(err)
	}
	p := pages.Page{Workspace: displayWorkspace(workspaceFromRow(w)), ActorID: user, Tab: tab, FormName: w.Name, FormDescription: w.Description, Search: filter.Search, InvitationView: filter.Invitations, DeactivatedView: filter.Deactivated && !filter.Invitations}
	if (tab == "settings" || tab == "setup") && !p.Manage() {
		return p, ErrUnavailable
	}
	if (filter.Invitations || filter.Deactivated) && !p.Manage() {
		return p, ErrUnavailable
	}
	counts, err := q.WorkspaceCounts(ctx, dbgen.WorkspaceCountsParams{WorkspaceID: wid, UserID: user})
	if err != nil {
		return p, fmt.Errorf("count workspace page: %w", err)
	}
	total, err := workspacePeopleCount(ctx, q, p)
	if err != nil {
		return p, err
	}
	if tab != "members" {
		total = 0
	}
	p.PageCount = max(int64(1), (total+19)/20)
	p.PageNumber = max(int64(1), min(filter.Page, p.PageCount))
	p.MemberCount = counts.Members
	p.RepositoryCount, p.InvitationCount = counts.Repositories, counts.Invitations
	p.DeactivatedCount, p.InstallationCount = counts.Deactivated, counts.Installations
	if err := s.tabContent(ctx, q, &p, (p.PageNumber-1)*20); err != nil {
		return p, err
	}
	if err := s.pendingTransfer(ctx, q, &p, user); err != nil {
		return p, err
	}
	if err := tx.Commit(ctx); err != nil {
		return p, fmt.Errorf("complete workspace view: %w", err)
	}
	return p, nil
}

// tabContent loads what only the selected tab shows.
func (s *Service) tabContent(ctx context.Context, q *dbgen.Queries, p *pages.Page, offset int64) error {
	switch {
	case p.Tab == "members":
		return s.membersPage(ctx, q, p, offset)
	case p.Tab == "setup":
		return s.setupPage(ctx, q, p)
	case p.Tab == "settings" && p.Owner():
		return ownershipCandidates(ctx, q, p)
	}
	return nil
}

func (s *Service) pendingTransfer(ctx context.Context, q *dbgen.Queries, p *pages.Page, user int64) error {
	t, err := q.WorkspaceTransfer(ctx, p.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read ownership transfer: %w", err)
	}
	if s.now().Before(t.ExpiresAt.Time) {
		p.Transfer = &pages.Transfer{TargetID: t.ToUserID, Revision: t.Revision, Login: t.TargetLogin, Expires: t.ExpiresAt.Time.UTC().Format(time.RFC3339), Accept: t.ToUserID == user}
	}
	return nil
}

func displayWorkspace(w Workspace) pages.Workspace {
	return pages.Workspace{ID: w.ID, Revision: w.Revision, Name: w.Name, Role: w.Role, Description: w.Description, OwnerName: w.OwnerLogin,
		Created: w.CreatedAt.UTC().Format(time.RFC3339), ProfileRevision: w.ProfileRevision, LogoVersion: w.LogoVersion}
}

func (s *Service) membersPage(ctx context.Context, q *dbgen.Queries, p *pages.Page, offset int64) error {
	switch {
	case p.InvitationView:
		if err := s.invitationRows(ctx, q, p, offset); err != nil {
			return err
		}
	case p.DeactivatedView:
		if err := deactivatedRows(ctx, q, p, offset); err != nil {
			return err
		}
	default:
		members, err := q.WorkspaceMembers(ctx, dbgen.WorkspaceMembersParams{WorkspaceID: p.ID, PageOffset: offset, Search: p.Search})
		if err != nil {
			return fmt.Errorf("list workspace members: %w", err)
		}
		for _, v := range members {
			m := pages.Member{ID: v.UserID, Revision: v.Revision, Login: v.Username, Avatar: v.AvatarUrl, Role: v.Role, FirstName: v.FirstName, LastName: v.LastName, Repositories: v.RepositoryIds}
			if v.CustomAvatar {
				m.PhotoVersion = v.AvatarVersion
			}
			p.Members = append(p.Members, m)
		}
	}
	if !p.Manage() {
		return nil
	}
	repos, err := q.WorkspaceRepositories(ctx, p.ID)
	if err != nil {
		return fmt.Errorf("list workspace repositories: %w", err)
	}
	for _, v := range repos {
		p.Repositories = append(p.Repositories, pages.Repository{ID: v.ID, Name: v.FullName, Available: v.Accessible && v.Status == "active"})
	}
	return nil
}

func (s *Service) invitationRows(ctx context.Context, q *dbgen.Queries, p *pages.Page, offset int64) error {
	invites, err := q.WorkspaceInvitations(ctx, dbgen.WorkspaceInvitationsParams{WorkspaceID: p.ID, PageOffset: offset, Search: p.Search})
	if err != nil {
		return fmt.Errorf("list workspace invitations: %w", err)
	}
	for _, v := range invites {
		p.Invitations = append(p.Invitations, pages.Invitation{ID: v.ID, Revision: v.Revision, Login: v.Login, Role: v.Role, Expires: v.ExpiresAt.Time.UTC().Format(time.RFC3339), Expired: !s.now().Before(v.ExpiresAt.Time)})
	}
	return nil
}

func workspacePeopleCount(ctx context.Context, q *dbgen.Queries, p pages.Page) (int64, error) {
	if p.Tab != "members" {
		return 0, nil
	}
	var n int64
	var err error
	switch {
	case p.InvitationView:
		n, err = q.CountWorkspaceInvitations(ctx, dbgen.CountWorkspaceInvitationsParams{WorkspaceID: p.ID, Search: p.Search})
	case p.DeactivatedView:
		n, err = q.CountDeactivatedMembers(ctx, dbgen.CountDeactivatedMembersParams{WorkspaceID: p.ID, Search: p.Search})
	default:
		n, err = q.CountWorkspaceMembers(ctx, dbgen.CountWorkspaceMembersParams{WorkspaceID: p.ID, Search: p.Search})
	}
	if err != nil {
		return 0, fmt.Errorf("count matching workspace people: %w", err)
	}
	return n, nil
}

func ownershipCandidates(ctx context.Context, q *dbgen.Queries, p *pages.Page) error {
	admins, err := q.WorkspaceAdmins(ctx, p.ID)
	if err != nil {
		return fmt.Errorf("list ownership candidates: %w", err)
	}
	for _, v := range admins {
		p.Admins = append(p.Admins, pages.Member{ID: v.UserID, Login: v.Username, Role: "admin"})
	}
	return nil
}

func deactivatedRows(ctx context.Context, q *dbgen.Queries, p *pages.Page, offset int64) error {
	rows, err := q.DeactivatedMembers(ctx, dbgen.DeactivatedMembersParams{WorkspaceID: p.ID, PageOffset: offset, Search: p.Search})
	if err != nil {
		return fmt.Errorf("list deactivated members: %w", err)
	}
	for _, v := range rows {
		m := pages.Member{ID: v.UserID, Login: v.Username, Avatar: v.AvatarUrl, Role: v.Role, FirstName: v.FirstName, LastName: v.LastName, Repositories: v.RemovedRepositoryIds}
		if v.CustomAvatar {
			m.PhotoVersion = v.AvatarVersion
		}
		p.Deactivated = append(p.Deactivated, pages.DeactivatedMember{Member: m, RemovedAt: stamp(v.RemovedAt), RemovedBy: v.RemovedByName, Left: v.LeftWorkspace, Invited: v.Invited})
	}
	return nil
}

// setupPage loads what a new workspace's checklist needs: repositories for the
// invitation form and the invitations already sent.
func (s *Service) setupPage(ctx context.Context, q *dbgen.Queries, p *pages.Page) error {
	repos, err := q.WorkspaceRepositories(ctx, p.ID)
	if err != nil {
		return fmt.Errorf("list workspace repositories: %w", err)
	}
	for _, v := range repos {
		p.Repositories = append(p.Repositories, pages.Repository{ID: v.ID, Name: v.FullName, Available: v.Accessible && v.Status == "active"})
	}
	return s.invitationRows(ctx, q, p, 0)
}
