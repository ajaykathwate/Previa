package workspaces

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/workspaces/dbgen"
	"github.com/ajaykathwatee/previa/web/components"
	pages "github.com/ajaykathwatee/previa/web/pages/workspaces"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	ErrDeleteMembers      = errors.New("a workspace can be deleted only when its owner is the only member")
	ErrDeleteInstallation = errors.New("uninstall the connected GitHub App first; Previa then removes its previews")
	ErrDeleteConfirmation = errors.New("type DELETE in capital letters to confirm")
)

func stamp(t pgtype.Timestamptz) string {
	if !t.Valid {
		return ""
	}
	return t.Time.UTC().Format(time.RFC3339)
}

// Activity lists a person's recent actions that the viewer may see. A zero
// workspace spans every workspace the two share.
func (s *Service) Activity(ctx context.Context, viewer, subject, workspace int64) ([]components.ActivityItem, error) {
	rows, err := dbgen.New(s.repo.pool).MemberActivity(ctx, dbgen.MemberActivityParams{SubjectID: subject, WorkspaceID: workspace, ViewerID: viewer})
	if err != nil {
		return nil, fmt.Errorf("read member activity: %w", err)
	}
	items := make([]components.ActivityItem, 0, len(rows))
	for _, v := range rows {
		items = append(items, components.ActivityItem{Kind: v.Kind, Detail: v.Detail, Login: v.Login, Workspace: v.WorkspaceName, Repo: v.RepoName,
			PRTitle: v.PrTitle, At: stamp(v.At), WorkspaceID: v.WorkspaceID, RepoID: v.RepoID, PRNumber: v.PrNumber})
	}
	return items, nil
}

// RecentActivity is the signed-in person's own feed, consumed by auth's profile.
func (s *Service) RecentActivity(ctx context.Context, user int64) ([]components.ActivityItem, error) {
	return s.Activity(ctx, user, user, 0)
}

// Member is a teammate's profile within one workspace. Deactivated members are
// visible only to managers, who may re-invite them.
func (s *Service) Member(ctx context.Context, viewer, wid, subject int64) (pages.MemberDetail, error) {
	p, err := s.Page(ctx, viewer, wid, "members", PageQuery{Page: 1})
	if err != nil {
		return pages.MemberDetail{}, err
	}
	v, err := dbgen.New(s.repo.pool).WorkspaceMember(ctx, dbgen.WorkspaceMemberParams{WorkspaceID: wid, UserID: subject})
	if err != nil {
		return pages.MemberDetail{}, accessError(err)
	}
	active := v.State == "active"
	if !active && !p.Manage() {
		return pages.MemberDetail{}, ErrUnavailable
	}
	m := pages.Member{ID: v.UserID, Revision: v.Revision, Login: v.Username, Avatar: v.AvatarUrl, Role: v.Role, FirstName: v.FirstName, LastName: v.LastName, Repositories: v.RepositoryIds}
	if v.CustomAvatar {
		m.PhotoVersion = v.AvatarVersion
	}
	d := pages.MemberDetail{Page: p, Member: m, Active: active, Left: v.LeftWorkspace, Joined: stamp(v.JoinedAt), Removed: stamp(v.RemovedAt), RemovedBy: v.RemovedByName,
		ProfileURL: v.GithubProfileUrl, Company: v.GithubCompany, Location: v.GithubLocation, Bio: v.GithubBio, Previous: v.RemovedRepositoryIds}
	if d.Activity, err = s.Activity(ctx, viewer, subject, wid); err != nil {
		return pages.MemberDetail{}, err
	}
	return d, nil
}

// ReinviteForm prefills an invitation with a deactivated member's previous
// role and repositories. Only the owner may offer the admin role again.
func (s *Service) ReinviteForm(ctx context.Context, viewer, wid, subject int64) (pages.InviteForm, error) {
	w, err := s.Read(ctx, viewer, wid)
	if err != nil {
		return pages.InviteForm{}, err
	}
	if !w.Manage() {
		return pages.InviteForm{}, ErrUnavailable
	}
	v, err := dbgen.New(s.repo.pool).WorkspaceMember(ctx, dbgen.WorkspaceMemberParams{WorkspaceID: wid, UserID: subject})
	if err != nil {
		return pages.InviteForm{}, accessError(err)
	}
	if v.State != "removed" {
		return pages.InviteForm{}, ErrConflict
	}
	role := v.Role
	if role == "owner" || (role == "admin" && w.Role != "owner") {
		role = "developer"
	}
	recipient, e := dbgen.New(s.repo.pool).InvitationRecipientForUser(ctx, subject)
	if e != nil {
		return pages.InviteForm{}, accessError(e)
	}
	return pages.InviteForm{Login: recipient, Role: role, Repositories: v.RemovedRepositoryIds, Failed: true, Reinvite: true}, nil
}

// ReceivedInvitations lists pending invitations addressed to a GitHub account.
func (s *Service) ReceivedInvitations(ctx context.Context, githubID int64) ([]pages.ReceivedInvitation, error) {
	return s.receivedInvitations(ctx, 0, githubID)
}
func (s *Service) ReceivedForUser(ctx context.Context, user int64) ([]pages.ReceivedInvitation, error) {
	return s.receivedInvitations(ctx, user, 0)
}
func (s *Service) receivedInvitations(ctx context.Context, user, githubID int64) ([]pages.ReceivedInvitation, error) {
	if githubID <= 0 && user <= 0 {
		return nil, nil
	}
	rows, err := dbgen.New(s.repo.pool).ReceivedInvitations(ctx, dbgen.ReceivedInvitationsParams{GithubID: githubID, UserID: user})
	if err != nil {
		return nil, fmt.Errorf("list received invitations: %w", err)
	}
	out := make([]pages.ReceivedInvitation, 0, len(rows))
	for _, v := range rows {
		out = append(out, pages.ReceivedInvitation{ID: v.ID, Workspace: v.WorkspaceName, Role: v.Role, Inviter: v.Inviter, Expires: stamp(v.ExpiresAt), Repositories: v.Repositories})
	}
	return out, nil
}

// CountReceivedInvitations feeds the workspace switcher's invitation badge.
func (s *Service) CountReceivedInvitations(ctx context.Context, githubID int64) (int64, error) {
	return s.countReceived(ctx, 0, githubID)
}
func (s *Service) CountReceivedForUser(ctx context.Context, user int64) (int64, error) {
	return s.countReceived(ctx, user, 0)
}
func (s *Service) countReceived(ctx context.Context, user, githubID int64) (int64, error) {
	if githubID <= 0 && user <= 0 {
		return 0, nil
	}
	n, err := dbgen.New(s.repo.pool).CountReceivedInvitations(ctx, dbgen.CountReceivedInvitationsParams{GithubID: githubID, UserID: user})
	if err != nil {
		return 0, fmt.Errorf("count received invitations: %w", err)
	}
	return n, nil
}

// Delete soft-deletes a workspace its owner no longer shares with anyone. All
// access functions require an active workspace, so access ends at once.
// Connected installations must be uninstalled first, because preview teardown
// follows installation lifecycle events.
func (s *Service) Delete(ctx context.Context, user, wid, revision int64, confirm string) error {
	if s.deletion != nil {
		return s.deletion.DeleteWorkspace(ctx, user, wid, revision, confirm)
	}
	if revision <= 0 {
		return ErrInput
	}
	return s.repo.mutation(ctx, user, wid, revision, func(_ pgx.Tx, q *dbgen.Queries, w Workspace) error {
		if w.Role != "owner" {
			return ErrUnavailable
		}
		if strings.TrimSpace(confirm) != pages.DeleteConfirmation {
			return ErrDeleteConfirmation
		}
		counts, err := q.WorkspaceCounts(ctx, dbgen.WorkspaceCountsParams{WorkspaceID: wid, UserID: user})
		if err != nil {
			return fmt.Errorf("count workspace before delete: %w", err)
		}
		if counts.Members != 1 {
			return ErrDeleteMembers
		}
		if counts.Installations > 0 {
			return ErrDeleteInstallation
		}
		n, err := q.DeleteWorkspace(ctx, wid)
		if err != nil {
			return fmt.Errorf("delete workspace: %w", err)
		}
		if n != 1 {
			return ErrConflict
		}
		for name, step := range map[string]func(context.Context, int64) error{
			"revoke invitations": q.RevokeWorkspaceInvitations, "cancel ownership transfer": q.DeleteWorkspaceTransfer,
			"remove logo": q.DeleteWorkspaceLogo,
		} {
			if err := step(ctx, wid); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
		if err := q.ClearWorkspacePreferences(ctx, nullableID(wid)); err != nil {
			return fmt.Errorf("clear workspace selections: %w", err)
		}
		return nil
	})
}
