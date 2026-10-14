package workspaces

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ajaykathwatee/previa/internal/workspaces/dbgen"
	pages "github.com/ajaykathwatee/previa/web/pages/workspaces"
	"github.com/jackc/pgx/v5"
)

func (s *Service) Page(ctx context.Context, user, wid int64, tab, domain string, page int64) (result pages.Page, resultErr error) {
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
	p := pages.Page{Workspace: pages.Workspace{ID: w.ID, Revision: w.Revision, Name: w.Name, Role: w.Role}, ActorID: user, Tab: tab}
	counts, err := q.WorkspaceCounts(ctx, dbgen.WorkspaceCountsParams{WorkspaceID: wid, UserID: user})
	if err != nil {
		return p, fmt.Errorf("count workspace page: %w", err)
	}
	total := counts.Members
	if w.Role == "owner" || w.Role == "admin" {
		total = max(total, counts.Invitations)
	}
	switch tab {
	case "previews":
		total = counts.Previews
	case "settings":
		total = 0
	}
	p.PageCount = max(int64(1), (total+19)/20)
	p.PageNumber = max(int64(1), min(page, p.PageCount))
	p.MemberCount = counts.Members
	offset := (p.PageNumber - 1) * 20
	members, err := q.WorkspaceMembers(ctx, dbgen.WorkspaceMembersParams{WorkspaceID: wid, PageOffset: offset})
	if err != nil {
		return p, fmt.Errorf("list workspace members: %w", err)
	}
	for _, v := range members {
		p.Members = append(p.Members, pages.Member{ID: v.UserID, Revision: v.Revision, Login: v.Username, Avatar: v.AvatarUrl, Role: v.Role, Repositories: v.RepositoryIds})
	}
	if w.Role == "owner" || w.Role == "admin" {
		if err := s.managementPage(ctx, q, &p); err != nil {
			return p, err
		}
	}
	if t, err := q.WorkspaceTransfer(ctx, wid); err == nil && s.now().Before(t.ExpiresAt.Time) {
		p.Transfer = &pages.Transfer{TargetID: t.ToUserID, Revision: t.Revision, Login: t.TargetLogin, Expires: t.ExpiresAt.Time.UTC().Format(time.RFC3339), Accept: t.ToUserID == user}
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return p, fmt.Errorf("read ownership transfer: %w", err)
	}
	if tab == "previews" {
		if err := permittedPreviews(ctx, q, &p, user, domain, offset); err != nil {
			return p, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return p, fmt.Errorf("complete workspace view: %w", err)
	}
	return p, nil
}

func permittedPreviews(ctx context.Context, q *dbgen.Queries, p *pages.Page, user int64, domain string, offset int64) error {
	rows, err := q.ReviewerPreviews(ctx, dbgen.ReviewerPreviewsParams{WorkspaceID: p.ID, UserID: user, PageOffset: offset})
	if err != nil {
		return fmt.Errorf("list permitted previews: %w", err)
	}
	for _, v := range rows {
		out := pages.Preview{RepoID: v.RepoID, Number: v.Number, Repository: v.FullName, Title: v.Title, Latest: v.LatestSha, Deployed: v.DeployedSha.String, Status: v.Status.String}
		if domain != "" && v.Available && previewServing(v.Status.String) && v.DeployedSha.String != "" && v.Subdomain.String != "" {
			out.URL = "https://" + v.Subdomain.String + "." + domain
		}
		if v.ExpiresAt.Valid {
			out.Expires = v.ExpiresAt.Time.UTC().Format(time.RFC3339)
		}
		p.Previews = append(p.Previews, out)
	}
	return nil
}

func previewServing(status string) bool {
	return status == "running" || status == "publishing" || status == "failed"
}

func (s *Service) managementPage(ctx context.Context, q *dbgen.Queries, p *pages.Page) error {
	repos, err := q.WorkspaceRepositories(ctx, p.ID)
	if err != nil {
		return fmt.Errorf("list workspace repositories: %w", err)
	}
	for _, v := range repos {
		p.Repositories = append(p.Repositories, pages.Repository{ID: v.ID, Name: v.FullName, Available: v.Accessible && v.Status == "active"})
	}
	invites, err := q.WorkspaceInvitations(ctx, dbgen.WorkspaceInvitationsParams{WorkspaceID: p.ID, PageOffset: (p.PageNumber - 1) * 20})
	if err != nil {
		return fmt.Errorf("list workspace invitations: %w", err)
	}
	for _, v := range invites {
		p.Invitations = append(p.Invitations, pages.Invitation{ID: v.ID, Revision: v.Revision, Login: v.Login, Role: v.Role, Expires: v.ExpiresAt.Time.UTC().Format(time.RFC3339), Expired: !s.now().Before(v.ExpiresAt.Time)})
	}
	admins, err := q.WorkspaceAdmins(ctx, p.ID)
	if err != nil {
		return fmt.Errorf("list ownership candidates: %w", err)
	}
	for _, v := range admins {
		p.Admins = append(p.Admins, pages.Member{ID: v.UserID, Login: v.Username, Role: "admin"})
	}
	return nil
}
