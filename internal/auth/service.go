package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/google/go-github/v91/github"
	"golang.org/x/oauth2"
	githuboauth "golang.org/x/oauth2/github"
)

// Identity contains only the public GitHub profile needed for local sign-in.
type Identity struct {
	ID        int64
	Username  string
	AvatarURL string
}

type identityProvider interface {
	AuthCodeURL(state string) string
	Authenticate(context.Context, string) (Identity, error)
}

// GitHubOAuth exchanges authorization codes and fetches the authenticated profile.
type GitHubOAuth struct {
	oauth  oauth2.Config
	client *http.Client
}

func NewGitHubOAuth(cfg config.Config) *GitHubOAuth {
	return &GitHubOAuth{
		oauth: oauth2.Config{
			ClientID: cfg.GitHubOAuthClientID, ClientSecret: cfg.GitHubOAuthClientSecret,
			RedirectURL: strings.TrimSuffix(cfg.AppBaseURL, "/") + "/auth/github/callback",
			Endpoint:    githuboauth.Endpoint,
			// No extra scopes: GitHub's public profile supplies ID, login, and avatar.
		},
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (g *GitHubOAuth) AuthCodeURL(state string) string { return g.oauth.AuthCodeURL(state) }

func (g *GitHubOAuth) Authenticate(ctx context.Context, code string) (Identity, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, g.client)
	token, err := g.oauth.Exchange(ctx, code)
	if err != nil {
		return Identity{}, fmt.Errorf("exchange GitHub authorization code: %w", err)
	}
	client, err := github.NewClient(github.WithHTTPClient(g.client), github.WithAuthToken(token.AccessToken))
	if err != nil {
		return Identity{}, fmt.Errorf("create GitHub identity client: %w", err)
	}
	u, _, err := client.Users.Get(ctx, "")
	if err != nil {
		return Identity{}, fmt.Errorf("fetch GitHub identity: %w", err)
	}
	if u.GetID() <= 0 || strings.TrimSpace(u.GetLogin()) == "" {
		return Identity{}, errors.New("GitHub returned an invalid identity")
	}
	// The access token and authenticated client are discarded after this request.
	return Identity{ID: u.GetID(), Username: u.GetLogin(), AvatarURL: u.GetAvatarURL()}, nil
}
