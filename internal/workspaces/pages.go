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
	Page        int64
	Search      string
	Invitations bool
}

func (s *Service) Page(ctx context.Context, user, wid int64, tab string, filter PageQuery) (result pages.Page, resultErr error) {
	filter.Search = strings.TrimSpace(filter.Search)
	if utf8.RuneCountInString(filter.Search) > 100 {
		return pages.Page{}, ErrInput
	}
	if tab != "members" && tab != "settings" {
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
	p := pages.Page{Workspace: displayWorkspace(workspaceFromRow(w)), ActorID: user, Tab: tab, FormName: w.Name, FormDescription: w.Description, Search: filter.Search, InvitationView: filter.Invitations}
	if tab == "settings" && !p.Manage() {
		return p, ErrUnavailable
	}
	if filter.Invitations && !p.Manage() {
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
	if tab == "settings" {
		total = 0
	}
	p.PageCount = max(int64(1), (total+19)/20)
	p.PageNumber = max(int64(1), min(filter.Page, p.PageCount))
	p.MemberCount = counts.Members
	p.RepositoryCount, p.InvitationCount = counts.Repositories, counts.Invitations
	offset := (p.PageNumber - 1) * 20
	if tab == "members" {
		if err := s.membersPage(ctx, q, &p, offset); err != nil {
			return p, err
		}
	}
	if tab == "settings" && p.Owner() {
		if err := ownershipCandidates(ctx, q, &p); err != nil {
			return p, err
		}
	}
	if t, err := q.WorkspaceTransfer(ctx, wid); err == nil && s.now().Before(t.ExpiresAt.Time) {
		p.Transfer = &pages.Transfer{TargetID: t.ToUserID, Revision: t.Revision, Login: t.TargetLogin, Expires: t.ExpiresAt.Time.UTC().Format(time.RFC3339), Accept: t.ToUserID == user}
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return p, fmt.Errorf("read ownership transfer: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return p, fmt.Errorf("complete workspace view: %w", err)
	}
	return p, nil
}

func displayWorkspace(w Workspace) pages.Workspace {
	return pages.Workspace{ID: w.ID, Revision: w.Revision, Name: w.Name, Role: w.Role, Description: w.Description, OwnerName: w.OwnerLogin,
		Created: w.CreatedAt.UTC().Format(time.RFC3339), ProfileRevision: w.ProfileRevision, LogoVersion: w.LogoVersion}
}

func (s *Service) membersPage(ctx context.Context, q *dbgen.Queries, p *pages.Page, offset int64) error {
	if p.InvitationView {
		if err := s.invitationRows(ctx, q, p, offset); err != nil {
			return err
		}
	} else {
		members, err := q.WorkspaceMembers(ctx, dbgen.WorkspaceMembersParams{WorkspaceID: p.ID, PageOffset: offset, Search: p.Search})
		if err != nil {
			return fmt.Errorf("list workspace members: %w", err)
		}
		for _, v := range members {
			p.Members = append(p.Members, pages.Member{ID: v.UserID, Revision: v.Revision, Login: v.Username, Avatar: v.AvatarUrl, Role: v.Role, Repositories: v.RepositoryIds})
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
	if p.Tab == "settings" {
		return 0, nil
	}
	var n int64
	var err error
	if p.InvitationView {
		n, err = q.CountWorkspaceInvitations(ctx, dbgen.CountWorkspaceInvitationsParams{WorkspaceID: p.ID, Search: p.Search})
	} else {
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
