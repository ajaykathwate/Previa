package auth

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ajaykathwatee/previa/internal/auth/dbgen"
)

// User is the local identity attached to an authenticated request.
type User struct {
	ID                                                                                   int64
	GitHubID                                                                             int64
	GoogleSubject, GoogleEmail                                                           string
	Username                                                                             string
	AvatarURL                                                                            string
	PublicID, FirstName, LastName, GitHubName, Email, ProfileURL, Bio, Company, Location string
	ProfileRevision, AvatarVersion                                                       int64
	SessionEpoch                                                                         int64
	EmailSyncError, CustomAvatar                                                         bool
	JoinedAt, GitHubCreatedAt, ProfileSyncedAt                                           time.Time
}

// Repository persists GitHub identities using feature-owned sqlc queries.
type Repository struct{ queries *dbgen.Queries }

func NewRepository(db dbgen.DBTX) *Repository {
	return &Repository{queries: dbgen.New(db)}
}

func (r *Repository) UpsertUser(ctx context.Context, identity Identity) (User, error) {
	parts := strings.Fields(identity.Name)
	first, last := "", ""
	if len(parts) > 0 {
		first = parts[0]
		last = strings.Join(parts[1:], " ")
	}
	if utf8.RuneCountInString(first) > 80 || utf8.RuneCountInString(last) > 80 {
		first, last = "", ""
	}
	u, err := r.queries.UpsertUser(ctx, dbgen.UpsertUserParams{
		GithubID: pgtype.Int8{Int64: identity.ID, Valid: identity.ID > 0}, Username: identity.Username, AvatarUrl: identity.AvatarURL, FirstName: first, LastName: last, GithubName: identity.Name,
		Email: identity.Email, EmailSyncError: identity.EmailSyncError, GithubProfileUrl: identity.ProfileURL, GithubBio: identity.Bio, GithubCompany: identity.Company, GithubLocation: identity.Location,
		GithubCreatedAt: pgtype.Timestamptz{Time: identity.GitHubCreatedAt, Valid: !identity.GitHubCreatedAt.IsZero()},
	})
	if err != nil {
		return User{}, fmt.Errorf("upsert GitHub user: %w", err)
	}
	return userFromRow(u), nil
}

func (r *Repository) GetUser(ctx context.Context, id int64) (User, error) {
	u, err := r.queries.GetUser(ctx, id)
	if err != nil {
		return User{}, fmt.Errorf("get session user: %w", err)
	}
	return userFromRow(u), nil
}

func userFromRow(u dbgen.User) User {
	return User{ID: u.ID, GitHubID: u.GithubID.Int64, GoogleSubject: u.GoogleSubject.String, GoogleEmail: u.GoogleEmail, Username: u.Username, AvatarURL: u.AvatarUrl, PublicID: uuid.UUID(u.PublicID.Bytes).String(), SessionEpoch: u.SessionEpoch,
		FirstName: u.FirstName, LastName: u.LastName, GitHubName: u.GithubName, Email: u.Email, EmailSyncError: u.EmailSyncError, ProfileURL: u.GithubProfileUrl, Bio: u.GithubBio, Company: u.GithubCompany, Location: u.GithubLocation,
		ProfileRevision: u.ProfileRevision, AvatarVersion: u.AvatarVersion, CustomAvatar: u.CustomAvatar, JoinedAt: u.CreatedAt.Time, GitHubCreatedAt: u.GithubCreatedAt.Time, ProfileSyncedAt: u.ProfileSyncedAt.Time}
}
func (u User) DisplayName() string {
	if name := strings.TrimSpace(u.FirstName + " " + u.LastName); name != "" {
		return name
	}
	return u.Username
}

var ErrName = errors.New("use up to 80 characters for each name, without control characters")
var ErrProfileConflict = errors.New("your profile changed in another window; review the current name before saving again")

func (r *Repository) UpdatePersonalName(ctx context.Context, id, revision int64, first, last string) error {
	first, last = strings.TrimSpace(first), strings.TrimSpace(last)
	for _, v := range []string{first, last} {
		if !utf8.ValidString(v) || utf8.RuneCountInString(v) > 80 || strings.ContainsFunc(v, unicode.IsControl) {
			return ErrName
		}
	}
	if revision <= 0 {
		return ErrProfileConflict
	}
	n, err := r.queries.UpdatePersonalName(ctx, dbgen.UpdatePersonalNameParams{ID: id, FirstName: first, LastName: last, ProfileRevision: revision})
	if err != nil {
		return fmt.Errorf("save personal name: %w", err)
	}
	if n != 1 {
		return ErrProfileConflict
	}
	return nil
}
