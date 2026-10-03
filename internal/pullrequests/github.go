package pullrequests

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/go-github/v91/github"
)

type installationClient interface {
	InstallationClient(int64) (*github.Client, error)
}
type GitHubSource struct{ app installationClient }

func NewGitHubSource(app installationClient) *GitHubSource { return &GitHubSource{app} }

type RetryAfterError struct{ Delay time.Duration }

func (e *RetryAfterError) Error() string { return "GitHub rate limit; retry later" }
func sourceError(err error) error {
	var rate *github.RateLimitError
	if errors.As(err, &rate) {
		return &RetryAfterError{time.Until(rate.Rate.Reset.Time)}
	}
	var abuse *github.AbuseRateLimitError
	if errors.As(err, &abuse) {
		delay := time.Minute
		if abuse.RetryAfter != nil {
			delay = *abuse.RetryAfter
		}
		return &RetryAfterError{delay}
	}
	return errors.New("GitHub PR discovery request failed")
}
func (s *GitHubSource) Page(ctx context.Context, installationID, repoID int64, page int32) ([]Observation, int32, error) {
	if installationID <= 0 || repoID <= 0 || page <= 0 {
		return nil, 0, errors.New("invalid discovery source identity")
	}
	client, err := s.app.InstallationClient(installationID)
	if err != nil {
		return nil, 0, sourceError(err)
	}
	repo, _, err := client.Repositories.GetByID(ctx, repoID)
	if err != nil {
		return nil, 0, sourceError(err)
	}
	if repo.GetID() != repoID {
		return nil, 0, errors.New("GitHub repository identity changed")
	}
	prs, response, err := client.PullRequests.List(ctx, repo.GetOwner().GetLogin(), repo.GetName(), &github.PullRequestListOptions{State: "all", Sort: "created", Direction: "asc", ListOptions: github.ListOptions{Page: int(page), PerPage: 100}})
	if err != nil {
		return nil, 0, sourceError(err)
	}
	result := make([]Observation, 0, len(prs))
	for _, pr := range prs {
		o, err := FromGitHub(pr, repoID)
		if err != nil {
			return nil, 0, fmt.Errorf("read discovered PR: %w", err)
		}
		result = append(result, o)
	}
	if response.NextPage > 2147483647 {
		return nil, 0, errors.New("GitHub pagination exceeds supported range")
	}
	return result, int32(response.NextPage), nil
}
