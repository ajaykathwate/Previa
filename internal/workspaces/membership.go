package workspaces

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/ajaykathwatee/previa/internal/workspaces/dbgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type MemberChange struct {
	ActorID, WorkspaceID, WorkspaceRevision, UserID, Revision int64
	Role                                                      string
	Repositories                                              []int64
	Remove                                                    bool
}

func (s *Service) ChangeMember(ctx context.Context, c MemberChange) error {
	if c.Revision <= 0 || c.WorkspaceRevision <= 0 || (!c.Remove && !validRole(c.Role)) {
		return ErrInput
	}
	return s.repo.mutation(ctx, c.ActorID, c.WorkspaceID, c.WorkspaceRevision, func(tx pgx.Tx, q *dbgen.Queries, actor Workspace) error {
		return s.changeMemberTx(ctx, tx, q, actor, c)
	})
}

func (s *Service) changeMemberTx(ctx context.Context, tx pgx.Tx, q *dbgen.Queries, actor Workspace, c MemberChange) error {
	member, err := q.LockMembership(ctx, dbgen.LockMembershipParams{WorkspaceID: c.WorkspaceID, UserID: c.UserID})
	if err != nil {
		return accessError(err)
	}
	if member.State != "active" || member.Revision != c.Revision {
		return ErrConflict
	}
	if member.Role == "owner" {
		return ErrOwner
	}
	selfLeave := c.Remove && c.ActorID == c.UserID
	if !selfLeave && (!actor.Manage() || (actor.Role != "owner" && (member.Role == "admin" || c.Role == "admin"))) {
		return ErrUnavailable
	}
	if !c.Remove && c.Role != "reviewer" {
		var linked bool
		if e := tx.QueryRow(ctx, `SELECT github_id IS NOT NULL FROM users WHERE id=$1 AND deleted_at IS NULL`, c.UserID).Scan(&linked); e != nil {
			return fmt.Errorf("check technical role identity: %w", e)
		}
		if !linked {
			return ErrGitHubRequired
		}
	}
	if c.Role == "admin" {
		c.Repositories = nil
	}
	if !c.Remove && c.Role == member.Role {
		unchanged, err := grantsUnchanged(ctx, q, c.WorkspaceID, c.UserID, c.Repositories)
		if err != nil {
			return err
		}
		if unchanged {
			return nil
		}
	}
	role, state := c.Role, "active"
	if c.Remove {
		role, state = member.Role, "removed"
		// Keep who removed them and what they had, for the Deactivated list and re-invitation.
		if err := q.RecordMemberRemoval(ctx, dbgen.RecordMemberRemovalParams{RemovedBy: c.ActorID, WorkspaceID: c.WorkspaceID, UserID: c.UserID}); err != nil {
			return fmt.Errorf("record member removal: %w", err)
		}
	}
	if err := q.ChangeMembership(ctx, dbgen.ChangeMembershipParams{WorkspaceID: c.WorkspaceID, UserID: c.UserID, Role: role, State: state}); err != nil {
		return fmt.Errorf("change member: %w", err)
	}
	if c.Remove {
		c.Repositories = nil
	}
	if err := replaceGrants(ctx, q, c.WorkspaceID, c.UserID, c.Repositories); err != nil {
		return err
	}
	if err := q.DeleteWorkspaceTransfer(ctx, c.WorkspaceID); err != nil {
		return fmt.Errorf("invalidate ownership transfer: %w", err)
	}
	return s.cancelRevoked(ctx, tx, q, c.WorkspaceID)
}

func (s *Service) NominateOwner(ctx context.Context, user, wid, revision, target int64) error {
	if revision <= 0 || target <= 0 || target == user {
		return ErrInput
	}
	return s.repo.mutation(ctx, user, wid, revision, func(_ pgx.Tx, q *dbgen.Queries, actor Workspace) error {
		if actor.Role != "owner" {
			return ErrUnavailable
		}
		m, err := q.LockMembership(ctx, dbgen.LockMembershipParams{WorkspaceID: wid, UserID: target})
		if err != nil {
			return accessError(err)
		}
		if m.Role != "admin" || m.State != "active" {
			return ErrUnavailable
		}
		if err := q.SaveWorkspaceTransfer(ctx, dbgen.SaveWorkspaceTransferParams{WorkspaceID: wid, FromUserID: user, ToUserID: target, Revision: revision + 1, ExpiresAt: pgtype.Timestamptz{Time: s.now().Add(7 * 24 * time.Hour), Valid: true}}); err != nil {
			return fmt.Errorf("nominate workspace owner: %w", err)
		}
		return q.AdvanceWorkspaceRevision(ctx, wid)
	})
}
func (s *Service) AcceptOwnership(ctx context.Context, user, wid, revision, transferRevision int64) error {
	if revision <= 0 || transferRevision <= 0 {
		return ErrInput
	}
	return s.repo.mutation(ctx, user, wid, revision, func(tx pgx.Tx, q *dbgen.Queries, actor Workspace) error {
		t, err := q.WorkspaceTransfer(ctx, wid)
		if err != nil {
			return accessError(err)
		}
		if t.ToUserID != user || t.FromUserID != actor.OwnerID || t.Revision != transferRevision || !s.now().Before(t.ExpiresAt.Time) || actor.Role != "admin" {
			return ErrConflict
		}
		if err := q.ChangeMembership(ctx, dbgen.ChangeMembershipParams{WorkspaceID: wid, UserID: t.FromUserID, Role: "admin", State: "active"}); err != nil {
			return fmt.Errorf("release previous owner: %w", err)
		}
		if err := q.ChangeMembership(ctx, dbgen.ChangeMembershipParams{WorkspaceID: wid, UserID: user, Role: "owner", State: "active"}); err != nil {
			return fmt.Errorf("assign workspace owner: %w", err)
		}
		if err := q.SetWorkspaceOwner(ctx, dbgen.SetWorkspaceOwnerParams{ID: wid, OwnerUserID: user}); err != nil {
			return fmt.Errorf("save workspace ownership: %w", err)
		}
		if err := q.DeleteWorkspaceTransfer(ctx, wid); err != nil {
			return fmt.Errorf("complete ownership transfer: %w", err)
		}
		return s.cancelRevoked(ctx, tx, q, wid)
	})
}
func (s *Service) CancelOwnership(ctx context.Context, user, wid, revision int64) error {
	if revision <= 0 {
		return ErrInput
	}
	return s.repo.mutation(ctx, user, wid, revision, func(_ pgx.Tx, q *dbgen.Queries, actor Workspace) error {
		if actor.Role != "owner" {
			return ErrUnavailable
		}
		return q.DeleteWorkspaceTransfer(ctx, wid)
	})
}

func grantsUnchanged(ctx context.Context, q *dbgen.Queries, wid, user int64, repos []int64) (bool, error) {
	current, err := q.CurrentRepositoryGrants(ctx, dbgen.CurrentRepositoryGrantsParams{WorkspaceID: wid, UserID: user})
	if err != nil {
		return false, fmt.Errorf("read current repository grants: %w", err)
	}
	next := uniqueIDs(repos)
	slices.Sort(next)
	return slices.Equal(current, next), nil
}
