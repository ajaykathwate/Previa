package workspaces

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/workspaces/dbgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrEmailInvitation = errors.New("use a valid email address and the Reviewer role; technical roles require a GitHub account")

func (s *Service) inviteEmail(ctx context.Context, c InvitationCommand) (string, error) {
	c.Login = strings.ToLower(strings.TrimSpace(c.Login))
	address, err := mail.ParseAddress(c.Login)
	if err != nil || address.Address != c.Login || len(c.Login) > 254 || c.Role != "reviewer" {
		return "", ErrEmailInvitation
	}
	if c.Revision <= 0 || len(c.Repositories) > 500 {
		return "", ErrInput
	}
	token, hash, err := invitationToken()
	if err != nil {
		return "", err
	}
	err = s.repo.mutation(ctx, c.ActorID, c.WorkspaceID, c.Revision, func(_ pgx.Tx, q *dbgen.Queries, w Workspace) error {
		if !w.Manage() {
			return ErrUnavailable
		}
		if e := q.ExpireEmailInvitation(ctx, dbgen.ExpireEmailInvitationParams{WorkspaceID: w.ID, Email: c.Login}); e != nil {
			return fmt.Errorf("expire email invitation: %w", e)
		}
		id, e := q.CreateEmailInvitation(ctx, dbgen.CreateEmailInvitationParams{WorkspaceID: w.ID, Email: c.Login, TokenHash: hash, ExpiresAt: pgtype.Timestamptz{Time: s.now().Add(7 * 24 * time.Hour), Valid: true}, CreatedBy: c.ActorID})
		if e != nil {
			var pgerr *pgconn.PgError
			if errors.As(e, &pgerr) && pgerr.Code == "23505" {
				return ErrPendingInvitation
			}
			return fmt.Errorf("save email invitation: %w", e)
		}
		return saveInvitationGrants(ctx, q, w.ID, id, c.Repositories)
	})
	if err != nil {
		return "", err
	}
	return token, nil
}
