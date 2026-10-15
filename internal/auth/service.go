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

// Identity holds the GitHub profile refreshed during sign-in; OAuth tokens are never persisted.
type Identity struct {
	ID                                              int64
	Username                                        string
	AvatarURL                                       string
	Name, Email, ProfileURL, Bio, Company, Location string
	GitHubCreatedAt                                 time.Time
	EmailSyncError                                  bool
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

func NewGitHubOAuth(cfg config.ServerConfig) *GitHubOAuth {
	return &GitHubOAuth{
		oauth: oauth2.Config{
			ClientID: cfg.GitHubOAuthClientID, ClientSecret: cfg.GitHubOAuthClientSecret,
			RedirectURL: strings.TrimSuffix(cfg.AppBaseURL, "/") + "/auth/github/callback",
			Endpoint:    githuboauth.Endpoint,
			// Read the signed-in account's verified primary email, without repository scopes.
			Scopes: []string{"user:email"},
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
	identity := Identity{ID: u.GetID(), Username: u.GetLogin(), AvatarURL: u.GetAvatarURL(), Name: u.GetName(),
		ProfileURL: "https://github.com/" + u.GetLogin(), Bio: u.GetBio(), Company: u.GetCompany(), Location: u.GetLocation(), GitHubCreatedAt: u.GetCreatedAt().Time}
	identity.Email, err = primaryEmail(ctx, client)
	identity.EmailSyncError = err != nil
	return identity, nil
}

func primaryEmail(ctx context.Context, client *github.Client) (string, error) {
	opts := &github.ListOptions{PerPage: 100}
	for {
		emails, response, err := client.Users.ListEmails(ctx, opts)
		if err != nil {
			return "", fmt.Errorf("fetch primary email: %w", err)
		}
		for _, email := range emails {
			if email.GetPrimary() && email.GetVerified() {
				return email.GetEmail(), nil
			}
		}
		if response == nil || response.NextPage == 0 {
			return "", nil
		}
		opts.Page = response.NextPage
	}
}
