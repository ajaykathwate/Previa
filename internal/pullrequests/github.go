package pullrequests

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
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
	prs, response, err := client.PullRequests.List(ctx, repo.GetOwner().GetLogin(), repo.GetName(), &github.PullRequestListOptions{State: "open", Sort: "created", Direction: "asc", ListOptions: github.ListOptions{Page: int(page), PerPage: 100}})
	if err != nil {
		return nil, 0, sourceError(err)
	}
	result := make([]Observation, 0, len(prs))
	for _, pr := range prs {
		if pr.GetState() != "open" || pr.GetMerged() || pr.MergedAt != nil {
			continue
		}
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

// Get fetches only a PR already tracked locally, never historical discovery.
func (s *GitHubSource) Get(ctx context.Context, installationID, repoID int64, number int32) (Observation, error) {
	client, err := s.app.InstallationClient(installationID)
	if err != nil {
		return Observation{}, sourceError(err)
	}
	repo, _, err := client.Repositories.GetByID(ctx, repoID)
	if err != nil {
		return Observation{}, sourceError(err)
	}
	if repo.GetID() != repoID {
		return Observation{}, errors.New("GitHub repository identity changed")
	}
	pr, _, err := client.PullRequests.Get(ctx, repo.GetOwner().GetLogin(), repo.GetName(), int(number))
	if err != nil {
		return Observation{}, sourceError(err)
	}
	if pr.GetNumber() != int(number) {
		return Observation{}, errors.New("GitHub PR identity changed")
	}
	return FromGitHub(pr, repoID)
}

// Upstream is a targeted read of one tracked PR together with the base
// repository's current API names (identity stays numeric).
type Upstream struct {
	Observation
	Owner, Repository string
}

// Inspect reads one tracked PR for fresh verification. Failures are classified
// so callers fail closed with a precise reason; it never touches the database.
func (s *GitHubSource) Inspect(ctx context.Context, installationID, repoID int64, number int32) (Upstream, error) {
	if installationID <= 0 || repoID <= 0 || number <= 0 {
		return Upstream{}, environments.ErrUpstreamInaccessible
	}
	client, err := s.app.InstallationClient(installationID)
	if err != nil {
		return Upstream{}, environments.ErrUpstreamUnavailable
	}
	repo, response, err := client.Repositories.GetByID(ctx, repoID)
	if err != nil {
		return Upstream{}, upstreamError(ctx, response, err)
	}
	if repo.GetID() != repoID || repo.GetOwner().GetLogin() == "" || repo.GetName() == "" {
		return Upstream{}, environments.ErrUpstreamChanged
	}
	pr, response, err := client.PullRequests.Get(ctx, repo.GetOwner().GetLogin(), repo.GetName(), int(number))
	if err != nil {
		return Upstream{}, upstreamError(ctx, response, err)
	}
	if pr.GetNumber() != int(number) {
		return Upstream{}, environments.ErrUpstreamChanged
	}
	o, err := FromGitHub(pr, repoID)
	if err != nil {
		return Upstream{}, environments.ErrUpstreamUnavailable
	}
	return Upstream{Observation: o, Owner: repo.GetOwner().GetLogin(), Repository: repo.GetName()}, nil
}

// upstreamError maps GitHub failures without exposing response bodies or URLs.
func upstreamError(ctx context.Context, response *github.Response, err error) error {
	if ctx.Err() != nil {
		return errors.Join(environments.ErrUpstreamUnavailable, ctx.Err())
	}
	var rate *github.RateLimitError
	if errors.As(err, &rate) {
		return &environments.UpstreamRetryAfter{Delay: max(time.Second, time.Until(rate.Rate.Reset.Time)), Err: environments.ErrUpstreamUnavailable}
	}
	var abuse *github.AbuseRateLimitError
	if errors.As(err, &abuse) {
		delay := time.Minute
		if abuse.RetryAfter != nil {
			delay = *abuse.RetryAfter
		}
		return &environments.UpstreamRetryAfter{Delay: delay, Err: environments.ErrUpstreamUnavailable}
	}
	if response != nil && (response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusGone) {
		return environments.ErrUpstreamInaccessible
	}
	return environments.ErrUpstreamUnavailable
}
