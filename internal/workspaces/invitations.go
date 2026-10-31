package workspaces

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/workspaces/dbgen"
	pages "github.com/ajaykathwatee/previa/web/pages/workspaces"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	ErrInvalidLogin          = errors.New("enter a valid GitHub username, without @ or a profile URL")
	ErrGitHubAccountNotFound = errors.New("no personal GitHub account was found with that username; check the spelling")
	ErrAlreadyMember         = errors.New("this GitHub account is already a workspace member; update their access from Members")
	ErrPendingInvitation     = errors.New("this GitHub account already has a pending invitation; manage it from Invitations")
)

var githubLoginPattern = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,37}[a-zA-Z0-9])?$`)

type InvitationCommand struct {
	ActorID, WorkspaceID, Revision int64
	Login, Role                    string
	Repositories                   []int64
}

func invitationToken() (string, []byte, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", nil, fmt.Errorf("create invitation token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(b[:])
	h := sha256.Sum256([]byte(token))
	return token, h[:], nil
}
func tokenHash(token string) ([]byte, error) {
	if len(token) != 43 {
		return nil, ErrInvitation
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(b) != 32 {
		return nil, ErrInvitation
	}
	h := sha256.Sum256([]byte(token))
	return h[:], nil
}
func (s *Service) Invite(ctx context.Context, c InvitationCommand) (string, error) {
	c.Login = strings.TrimSpace(c.Login)
	if strings.Contains(c.Login, "@") && !strings.HasPrefix(c.Login, "@") {
		return s.inviteEmail(ctx, c)
	}
	return s.inviteGitHub(ctx, c)
}
func (s *Service) inviteGitHub(ctx context.Context, c InvitationCommand) (token string, err error) {
	if !githubLoginPattern.MatchString(c.Login) || strings.Contains(c.Login, "--") {
		return "", ErrInvalidLogin
	}
	if !validRole(c.Role) || c.Revision <= 0 || len(c.Repositories) > 500 {
		return "", ErrInput
	}
	actor, err := s.Read(ctx, c.ActorID, c.WorkspaceID)
	if err != nil {
		return "", err
	}
	if !actor.Manage() || (c.Role == "admin" && actor.Role != "owner") {
		return "", ErrUnavailable
	}
	if s.directory == nil {
		return "", ErrSourceUnavailable
	}
	id, login, err := s.directory.LookupGitHubUser(ctx, c.Login)
	if err != nil {
		return "", fmt.Errorf("resolve invitation identity: %w", errors.Join(ErrSourceUnavailable, err))
	}
	if id <= 0 || login == "" {
		return "", ErrGitHubAccountNotFound
	}
	token, hash, err := invitationToken()
	if err != nil {
		return "", err
	}
	err = s.repo.mutation(ctx, c.ActorID, c.WorkspaceID, c.Revision, func(_ pgx.Tx, q *dbgen.Queries, w Workspace) error {
		if !w.Manage() || (c.Role == "admin" && w.Role != "owner") {
			return ErrUnavailable
		}
		if err := checkInvitable(ctx, q, w.ID, id); err != nil {
			return err
		}
		if err := checkPendingInvitation(ctx, q, w.ID, id); err != nil {
			return err
		}
		if err := q.ExpirePendingInvitation(ctx, dbgen.ExpirePendingInvitationParams{WorkspaceID: w.ID, GithubID: pgtype.Int8{Int64: id, Valid: true}}); err != nil {
			return fmt.Errorf("expire prior invitation: %w", err)
		}
		inv, e := q.CreateInvitation(ctx, dbgen.CreateInvitationParams{WorkspaceID: w.ID, GithubID: pgtype.Int8{Int64: id, Valid: true}, Login: login, Role: c.Role, TokenHash: hash,
			ExpiresAt: pgtype.Timestamptz{Time: s.now().Add(7 * 24 * time.Hour), Valid: true}, CreatedBy: c.ActorID})
		if e != nil {
			var pgerr *pgconn.PgError
			if errors.As(e, &pgerr) && pgerr.Code == "23505" {
				return ErrPendingInvitation
			}
			return fmt.Errorf("save invitation: %w", e)
		}
		return saveInvitationGrants(ctx, q, w.ID, inv, c.Repositories)
	})
	if err != nil {
		return "", err
	}
	return token, nil
}
func uniqueIDs(ids []int64) []int64 {
	out := make([]int64, 0, len(ids))
	seen := map[int64]bool{}
	for _, id := range ids {
		if !seen[id] {
			out = append(out, id)
			seen[id] = true
		}
	}
	return out
}
func (s *Service) Invitation(ctx context.Context, githubID int64, token string) (dbgen.ReadInvitationRow, error) {
	return s.InvitationForUser(ctx, 0, githubID, token)
}
func (s *Service) InvitationForUser(ctx context.Context, user, githubID int64, token string) (dbgen.ReadInvitationRow, error) {
	hash, err := tokenHash(token)
	if err != nil {
		return dbgen.ReadInvitationRow{}, err
	}
	v, err := dbgen.New(s.repo.pool).ReadInvitation(ctx, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrInvitation
	}
	if err != nil {
		return v, fmt.Errorf("read invitation: %w", err)
	}
	matches, matchErr := invitationMatches(ctx, dbgen.New(s.repo.pool), v.ID, user, githubID)
	if matchErr != nil {
		return v, matchErr
	}
	if !matches || v.WorkspaceState != "active" || v.State != "pending" || !s.now().Before(v.ExpiresAt.Time) {
		return v, ErrInvitation
	}
	return v, nil
}
func (s *Service) RespondInvitation(ctx context.Context, user, githubID int64, token string, accept bool) (wid int64, err error) {
	hash, err := tokenHash(token)
	if err != nil {
		return 0, err
	}
	v, err := dbgen.New(s.repo.pool).ReadInvitation(ctx, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrInvitation
	}
	if err != nil {
		return 0, fmt.Errorf("read invitation response: %w", err)
	}
	if matches, e := invitationMatches(ctx, dbgen.New(s.repo.pool), v.ID, user, githubID); e != nil || !matches {
		return 0, ErrInvitation
	}
	wid = v.WorkspaceID
	err = s.repo.mutationForInvitation(ctx, wid, func(q *dbgen.Queries) error {
		inv, e := q.LockInvitation(ctx, v.ID)
		if e != nil && !errors.Is(e, pgx.ErrNoRows) {
			return fmt.Errorf("lock invitation: %w", e)
		}
		if e != nil || !hmac.Equal(inv.TokenHash, hash) {
			return ErrInvitation
		}
		return s.respondLocked(ctx, q, inv, user, githubID, accept)
	})
	return wid, err
}

// RespondReceivedInvitation answers an invitation from inside Previa. The
// invitation is addressed to a GitHub account, so the signed-in identity
// replaces the link token; all other acceptance checks are the same.
func (s *Service) RespondReceivedInvitation(ctx context.Context, user, githubID, id int64, accept bool) (wid int64, err error) {
	if id <= 0 || user <= 0 {
		return 0, ErrInvitation
	}
	v, err := dbgen.New(s.repo.pool).LockInvitation(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrInvitation
	}
	if err != nil {
		return 0, fmt.Errorf("read received invitation: %w", err)
	}
	if matches, e := invitationMatches(ctx, dbgen.New(s.repo.pool), v.ID, user, githubID); e != nil || !matches {
		return 0, ErrInvitation
	}
	wid = v.WorkspaceID
	err = s.repo.mutationForInvitation(ctx, wid, func(q *dbgen.Queries) error {
		inv, e := q.LockInvitation(ctx, id)
		if e != nil && !errors.Is(e, pgx.ErrNoRows) {
			return fmt.Errorf("lock invitation: %w", e)
		}
		if e != nil {
			return ErrInvitation
		}
		return s.respondLocked(ctx, q, inv, user, githubID, accept)
	})
	return wid, err
}

// respondLocked applies the recipient's answer to a locked invitation.
func (s *Service) respondLocked(ctx context.Context, q *dbgen.Queries, inv dbgen.WorkspaceInvitation, user, githubID int64, accept bool) error {
	wid := inv.WorkspaceID
	if user <= 0 {
		return ErrInvitation
	}
	if matches, err := invitationMatches(ctx, q, inv.ID, user, githubID); err != nil {
		return err
	} else if !matches {
		return ErrInvitation
	}
	if inv.State == "accepted" && accept {
		return acceptedInvitationMembership(ctx, q, wid, user)
	}
	if inv.State != "pending" || !s.now().Before(inv.ExpiresAt.Time) {
		return ErrInvitation
	}
	allowed, e := q.InvitationCreatorAuthorized(ctx, dbgen.InvitationCreatorAuthorizedParams{WorkspaceID: wid, UserID: inv.CreatedBy, Column3: inv.Role})
	if e != nil {
		return fmt.Errorf("verify invitation creator: %w", e)
	}
	if !allowed {
		return ErrInvitation
	}
	state := "declined"
	if accept {
		if err := s.acceptInvitation(ctx, q, inv, user); err != nil {
			return err
		}
		state = "accepted"
	}
	if e = q.UpdateInvitationState(ctx, dbgen.UpdateInvitationStateParams{ID: inv.ID, State: state}); e != nil {
		return e
	}
	return q.AdvanceWorkspaceRevision(ctx, wid)
}

func acceptedInvitationMembership(ctx context.Context, q *dbgen.Queries, wid, user int64) error {
	_, err := q.ReadWorkspace(ctx, dbgen.ReadWorkspaceParams{WorkspaceID: wid, UserID: user})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvitation
	}
	if err != nil {
		return fmt.Errorf("verify accepted invitation membership: %w", err)
	}
	return nil
}

func (s *Service) ChangeInvitation(ctx context.Context, user, wid, revision, invID, invRevision int64, regenerate bool) (string, error) {
	if revision <= 0 || invRevision <= 0 {
		return "", ErrInput
	}
	token, hash, err := invitationToken()
	if err != nil {
		return "", err
	}
	err = s.repo.mutation(ctx, user, wid, revision, func(_ pgx.Tx, q *dbgen.Queries, actor Workspace) error {
		if !actor.Manage() {
			return ErrUnavailable
		}
		v, e := q.LockInvitation(ctx, invID)
		if e != nil {
			return accessError(e)
		}
		if v.WorkspaceID != wid || (v.Role == "admin" && actor.Role != "owner") {
			return ErrUnavailable
		}
		if v.Revision != invRevision || v.State != "pending" {
			return ErrConflict
		}
		if !regenerate {
			return q.UpdateInvitationState(ctx, dbgen.UpdateInvitationStateParams{ID: invID, State: "revoked"})
		}
		return q.RegenerateInvitation(ctx, dbgen.RegenerateInvitationParams{ID: invID, TokenHash: hash, ExpiresAt: pgtype.Timestamptz{Time: s.now().Add(7 * 24 * time.Hour), Valid: true}})
	})
	if err != nil || !regenerate {
		return "", err
	}
	return token, nil
}

func (s *Service) InvitationScope(ctx context.Context, githubID int64, token string) ([]pages.Repository, error) {
	return s.InvitationScopeForUser(ctx, 0, githubID, token)
}
func (s *Service) InvitationScopeForUser(ctx context.Context, user, githubID int64, token string) ([]pages.Repository, error) {
	v, err := s.InvitationForUser(ctx, user, githubID, token)
	if err != nil {
		return nil, err
	}
	rows, err := dbgen.New(s.repo.pool).InvitationScope(ctx, v.ID)
	if err != nil {
		return nil, fmt.Errorf("read invitation scope: %w", err)
	}
	repos := make([]pages.Repository, 0, len(rows))
	for _, r := range rows {
		repos = append(repos, pages.Repository{ID: r.ID, Name: r.FullName})
	}
	return repos, nil
}

func checkInvitable(ctx context.Context, q *dbgen.Queries, wid, githubID int64) error {
	user, err := q.UserByGitHubID(ctx, pgtype.Int8{Int64: githubID, Valid: githubID > 0})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("find invited identity: %w", err)
	}
	m, err := q.LockMembership(ctx, dbgen.LockMembershipParams{WorkspaceID: wid, UserID: user})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read existing invited member: %w", err)
	}
	if m.State == "active" {
		return errors.Join(ErrConflict, ErrAlreadyMember)
	}
	return nil
}
func saveInvitationGrants(ctx context.Context, q *dbgen.Queries, wid, inv int64, repos []int64) error {
	for _, repo := range uniqueIDs(repos) {
		n, err := q.PutInvitationRepository(ctx, dbgen.PutInvitationRepositoryParams{InvitationID: inv, RepoID: repo, WorkspaceID: wid})
		if err != nil {
			return fmt.Errorf("save invitation repository: %w", err)
		}
		if n != 1 {
			return ErrUnavailable
		}
	}
	return nil
}
func (s *Service) acceptInvitation(ctx context.Context, q *dbgen.Queries, inv dbgen.WorkspaceInvitation, user int64) error {
	// Another pending invitation cannot demote an already active member.
	member, e := q.LockMembership(ctx, dbgen.LockMembershipParams{WorkspaceID: inv.WorkspaceID, UserID: user})
	if e != nil && !errors.Is(e, pgx.ErrNoRows) {
		return fmt.Errorf("check invited member: %w", e)
	}
	if e == nil && member.State == "active" {
		return ErrAlreadyMember
	}
	if err := q.PutMembership(ctx, dbgen.PutMembershipParams{WorkspaceID: inv.WorkspaceID, UserID: user, Role: inv.Role}); err != nil {
		return fmt.Errorf("accept workspace membership: %w", err)
	}
	repos, err := q.InvitationRepositories(ctx, inv.ID)
	if err != nil {
		return fmt.Errorf("read invited repository grants: %w", err)
	}
	if err := replaceGrants(ctx, q, inv.WorkspaceID, user, repos); err != nil {
		return err
	}
	if err := q.SaveWorkspacePreference(ctx, dbgen.SaveWorkspacePreferenceParams{UserID: user, WorkspaceID: nullableID(inv.WorkspaceID)}); err != nil {
		return fmt.Errorf("select joined workspace: %w", err)
	}
	return nil
}

func checkPendingInvitation(ctx context.Context, q *dbgen.Queries, workspaceID, githubID int64) error {
	pending, err := q.HasPendingInvitation(ctx, dbgen.HasPendingInvitationParams{WorkspaceID: workspaceID, GithubID: pgtype.Int8{Int64: githubID, Valid: githubID > 0}})
	if err != nil {
		return fmt.Errorf("check pending invitation: %w", err)
	}
	if pending {
		return ErrPendingInvitation
	}
	return nil
}

func invitationMatches(ctx context.Context, q *dbgen.Queries, inv, user, github int64) (bool, error) {
	ok, err := q.InvitationMatches(ctx, dbgen.InvitationMatchesParams{InvitationID: inv, UserID: user, GithubID: github})
	if err != nil {
		return false, fmt.Errorf("verify invitation recipient: %w", err)
	}
	return ok, nil
}
