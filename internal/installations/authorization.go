package installations

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/google/go-github/v91/github"
	"golang.org/x/oauth2"
	githuboauth "golang.org/x/oauth2/github"
)

// GitHubAuthorization is separate from dashboard OAuth. Its user token exists
// only during linking and is discarded after verifying identity/access.
type GitHubAuthorization struct {
	oauth  oauth2.Config
	client *http.Client
}

func NewGitHubAuthorization(cfg config.ServerConfig) *GitHubAuthorization {
	return &GitHubAuthorization{oauth: oauth2.Config{ClientID: cfg.GitHubAppClientID, ClientSecret: cfg.GitHubAppClientSecret,
		RedirectURL: strings.TrimSuffix(cfg.AppBaseURL, "/") + "/installations/authorize/callback", Endpoint: githuboauth.Endpoint}, client: &http.Client{Timeout: 10 * time.Second}}
}
func (g *GitHubAuthorization) AuthCodeURL(state string) string { return g.oauth.AuthCodeURL(state) }
func (g *GitHubAuthorization) Verify(ctx context.Context, code string, githubUserID, installationID int64) ([]RepositoryInfo, error) {
	token, err := g.oauth.Exchange(context.WithValue(ctx, oauth2.HTTPClient, g.client), code)
	if err != nil {
		return nil, fmt.Errorf("exchange App user authorization: %w", err)
	}
	client, err := github.NewClient(github.WithHTTPClient(g.client), github.WithAuthToken(token.AccessToken))
	if err != nil {
		return nil, fmt.Errorf("construct App user client: %w", err)
	}
	user, _, err := client.Users.Get(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("verify App user identity: %w", err)
	}
	if user.GetID() != githubUserID {
		return nil, ErrForbidden
	}
	found := false
	for page := 1; page != 0; {
		installations, response, err := client.Apps.ListUserInstallations(ctx, &github.ListOptions{Page: page, PerPage: 100})
		if err != nil {
			return nil, fmt.Errorf("list user installations: %w", err)
		}
		for _, installation := range installations {
			if installation.GetID() == installationID {
				found = true
			}
		}
		if found {
			break
		}
		page = response.NextPage
	}
	if !found {
		return nil, ErrForbidden
	}
	var repos []RepositoryInfo
	for page := 1; page != 0; {
		result, response, err := client.Apps.ListUserRepos(ctx, installationID, &github.ListOptions{Page: page, PerPage: 100})
		if err != nil {
			return nil, fmt.Errorf("list user installation repositories: %w", err)
		}
		for _, repo := range result.Repositories {
			if repo.GetID() <= 0 || repo.GetFullName() == "" {
				return nil, ErrForbidden
			}
			repos = append(repos, RepositoryInfo{ID: repo.GetID(), FullName: repo.GetFullName(), Accessible: true})
		}
		page = response.NextPage
	}
	return repos, nil
}
