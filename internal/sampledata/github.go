package sampledata

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/go-github/v91/github"
)

var (
	// ErrBranchNotFound: the baseline branch does not exist (any more).
	ErrBranchNotFound = errors.New("baseline branch not found")
	// ErrGitHubUnavailable: GitHub could not answer now; try again later.
	ErrGitHubUnavailable = errors.New("GitHub is unavailable")
)

// InstallationClients is the GitHub App access branch lookups need;
// *githubapp.Client implements it.
type InstallationClients interface {
	InstallationClient(int64) (*github.Client, error)
}

// GitHubBranches reads a repository's branch heads with its installation.
type GitHubBranches struct{ clients InstallationClients }

// NewGitHubBranches needs the App's installation clients.
func NewGitHubBranches(clients InstallationClients) (*GitHubBranches, error) {
	if clients == nil {
		return nil, errors.New("baseline refreshes require the GitHub App")
	}
	return &GitHubBranches{clients: clients}, nil
}

// BranchHead resolves branch ("" for the repository's GitHub default branch,
// read now, so a renamed default is followed) to its head commit. The
// repository is found by its immutable ID, so a rename cannot point the
// lookup elsewhere.
func (g *GitHubBranches) BranchHead(ctx context.Context, installationID, repoID int64, branch string) (string, string, error) {
	client, err := g.clients.InstallationClient(installationID)
	if err != nil || client == nil {
		return "", "", fmt.Errorf("%w: no installation client", ErrGitHubUnavailable)
	}
	repo, response, err := client.Repositories.GetByID(ctx, repoID)
	if err != nil {
		return "", "", classify(response, err, false)
	}
	if repo.GetID() != repoID || repo.GetOwner().GetLogin() == "" || repo.GetName() == "" {
		return "", "", fmt.Errorf("%w: unexpected repository", ErrGitHubUnavailable)
	}
	if branch == "" {
		branch = repo.GetDefaultBranch()
	}
	if branch == "" {
		return "", "", ErrBranchNotFound
	}
	head, response, err := client.Repositories.GetBranch(ctx, repo.GetOwner().GetLogin(), repo.GetName(), branch, 0)
	if err != nil {
		return branch, "", classify(response, err, true)
	}
	sha := head.GetCommit().GetSHA()
	if !commitSHA(sha) {
		return branch, "", fmt.Errorf("%w: branch head is not a commit", ErrGitHubUnavailable)
	}
	return branch, sha, nil
}

// classify keeps a missing branch apart from everything else, which may
// succeed later.
func classify(response *github.Response, err error, branch bool) error {
	if branch && response != nil && response.StatusCode == http.StatusNotFound {
		return ErrBranchNotFound
	}
	return fmt.Errorf("%w: %v", ErrGitHubUnavailable, err)
}

func commitSHA(sha string) bool {
	if len(sha) != 40 {
		return false
	}
	for _, r := range sha {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
