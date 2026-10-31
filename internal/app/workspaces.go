package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-github/v91/github"
)

// Workspace GitHub access uses App credentials but verifies the human's numeric
// identity too: a renamed/reclaimed username cannot inherit another user's access.
type workspaceGitHub struct{ app GitHubApp }

func (g workspaceGitHub) LookupGitHubUser(ctx context.Context, login string) (int64, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client, err := github.NewClient(github.WithHTTPClient(&http.Client{Timeout: 10 * time.Second}))
	if err != nil {
		return 0, "", fmt.Errorf("create GitHub directory client: %w", err)
	}
	user, response, err := client.Users.Get(ctx, login)
	if response != nil && response.StatusCode == http.StatusNotFound {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("resolve GitHub user: %w", err)
	}
	if user.GetType() != "User" || user.GetID() <= 0 {
		return 0, "", nil
	}
	return user.GetID(), user.GetLogin(), nil
}
func (g workspaceGitHub) RepositoryUserPermission(ctx context.Context, id int64, fullName, login string, userID int64) (string, error) {
	if g.app == nil {
		return "", fmt.Errorf("GitHub App is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client, err := g.app.InstallationClient(id)
	if err != nil {
		return "", fmt.Errorf("create workspace permission client: %w", err)
	}
	owner, repo, _ := strings.Cut(fullName, "/")
	level, response, err := client.Repositories.GetPermissionLevel(ctx, owner, repo, login)
	if response != nil && response.StatusCode == http.StatusNotFound {
		return "none", nil
	}
	if err != nil {
		return "", fmt.Errorf("verify GitHub repository membership: %w", err)
	}
	if level.GetUser().GetID() != userID {
		return "none", nil
	}
	return level.GetPermission(), nil
}
