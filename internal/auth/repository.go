package auth

import (
	"context"
	"fmt"

	"github.com/ajaykathwatee/previa/internal/auth/dbgen"
)

// User is the local identity attached to an authenticated request.
type User struct {
	ID        int64
	GitHubID  int64
	Username  string
	AvatarURL string
}

// Repository persists GitHub identities using feature-owned sqlc queries.
type Repository struct{ queries *dbgen.Queries }

func NewRepository(db dbgen.DBTX) *Repository {
	return &Repository{queries: dbgen.New(db)}
}

func (r *Repository) UpsertUser(ctx context.Context, identity Identity) (User, error) {
	u, err := r.queries.UpsertUser(ctx, dbgen.UpsertUserParams{
		GithubID: identity.ID, Username: identity.Username, AvatarUrl: identity.AvatarURL,
	})
	if err != nil {
		return User{}, fmt.Errorf("upsert GitHub user: %w", err)
	}
	return User{ID: u.ID, GitHubID: u.GithubID, Username: u.Username, AvatarURL: u.AvatarUrl}, nil
}

func (r *Repository) GetUser(ctx context.Context, id int64) (User, error) {
	u, err := r.queries.GetUser(ctx, id)
	if err != nil {
		return User{}, fmt.Errorf("get session user: %w", err)
	}
	return User{ID: u.ID, GitHubID: u.GithubID, Username: u.Username, AvatarURL: u.AvatarUrl}, nil
}
