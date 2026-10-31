package auth

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"github.com/ajaykathwatee/previa/internal/auth/dbgen"
	"github.com/jackc/pgx/v5/pgtype"
)

// SaveGoogle uses immutable sub, never email, as account identity. Linking is an
// explicit authenticated action; unique identities cannot be stolen or merged.
func (r *Repository) SaveGoogle(ctx context.Context, i Identity, user int64) (User, error) {
	i.Email = strings.ToLower(strings.TrimSpace(i.Email))
	address, err := mail.ParseAddress(i.Email)
	if err != nil || address.Address != i.Email || len(i.Email) > 254 || i.GoogleSubject == "" || len(i.GoogleSubject) > 255 {
		return User{}, errors.New("invalid Google identity")
	}
	subject := pgtype.Text{String: i.GoogleSubject, Valid: true}
	var u dbgen.User
	if user > 0 {
		u, err = r.queries.LinkGoogleUser(ctx, dbgen.LinkGoogleUserParams{ID: user, GoogleSubject: subject, GoogleEmail: i.Email})
	} else {
		name := strings.TrimSpace(i.Name)
		if name == "" || len(name) > 80 {
			name = "Reviewer"
		}
		u, err = r.queries.UpsertGoogleUser(ctx, dbgen.UpsertGoogleUserParams{GoogleSubject: subject, GoogleEmail: i.Email, Username: name, FirstName: name})
	}
	if err != nil {
		return User{}, fmt.Errorf("save Google identity: %w", err)
	}
	return userFromRow(u), nil
}
func (r *Repository) LinkGitHub(ctx context.Context, i Identity, user int64) (User, error) {
	if i.ID <= 0 || i.Username == "" {
		return User{}, errors.New("invalid GitHub identity")
	}
	u, err := r.queries.LinkGitHubUser(ctx, dbgen.LinkGitHubUserParams{ID: user, GithubID: pgtype.Int8{Int64: i.ID, Valid: true}, Username: i.Username, AvatarUrl: i.AvatarURL, GithubName: i.Name, Email: i.Email, EmailSyncError: i.EmailSyncError, GithubProfileUrl: i.ProfileURL})
	if err != nil {
		return User{}, fmt.Errorf("link GitHub identity: %w", err)
	}
	return userFromRow(u), nil
}
