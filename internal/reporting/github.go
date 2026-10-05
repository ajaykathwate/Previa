package reporting

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-github/v91/github"
)

// Failure categories of GitHub calls. They are stable and contain no
// response text.
var (
	ErrNotFound         = errors.New("github_not_found")
	ErrPermissionDenied = errors.New("github_permission_denied")
	ErrRejected         = errors.New("github_rejected")
	ErrUnavailable      = errors.New("github_unavailable")
)

// RateLimited is GitHub's request to wait.
type RateLimited struct{ RetryAfter time.Duration }

func (e *RateLimited) Error() string { return "github_rate_limited" }

// Repo is a repository's current API names; identity stays numeric.
type Repo struct{ Owner, Name string }

// Comment is an issue comment on the PR.
type Comment struct {
	ID       int64
	Body     string
	AppOwned bool // written by this GitHub App
}

// CheckRun is the content of one check run.
type CheckRun struct {
	Name, HeadSHA, ExternalID                   string
	Status, Conclusion, Title, Summary, Details string
}

// GitHub is everything the reporter needs from GitHub.
type GitHub interface {
	Repository(ctx context.Context, installationID, repoID int64) (Repo, error)
	GetComment(ctx context.Context, installationID int64, repo Repo, id int64) (Comment, error)
	UpdateComment(ctx context.Context, installationID int64, repo Repo, id int64, body string) error
	CreateComment(ctx context.Context, installationID int64, repo Repo, number int32, body string) (int64, error)
	// FindComment looks for this App's comment carrying marker, reading at
	// most maxPages pages of the PR's comments.
	FindComment(ctx context.Context, installationID int64, repo Repo, number int32, marker string, maxPages int) (Comment, bool, error)
	// FindCheckRun returns this App's check run with externalID on headSHA.
	FindCheckRun(ctx context.Context, installationID int64, repo Repo, headSHA, name, externalID string) (int64, bool, error)
	CreateCheckRun(ctx context.Context, installationID int64, repo Repo, run CheckRun) (int64, error)
	UpdateCheckRun(ctx context.Context, installationID int64, repo Repo, id int64, run CheckRun) error
}

// InstallationClients returns an installation-authenticated client;
// *githubapp.Client implements it.
type InstallationClients interface {
	InstallationClient(int64) (*github.Client, error)
}

// GitHubAPI implements GitHub with go-github and installation tokens.
type GitHubAPI struct {
	clients  InstallationClients
	appID    int64
	botLogin string // <slug>[bot]
	now      func() time.Time
}

// NewGitHubAPI needs the App's ID and slug (githubapp.Client.App) to tell
// its own comments apart from anyone else's.
func NewGitHubAPI(clients InstallationClients, appID int64, slug string) (*GitHubAPI, error) {
	if clients == nil || appID <= 0 || slug == "" {
		return nil, errors.New("GitHub reporting needs installation clients and the App's ID and slug")
	}
	return &GitHubAPI{clients: clients, appID: appID, botLogin: slug + "[bot]", now: time.Now}, nil
}

func (g *GitHubAPI) client(installationID int64) (*github.Client, error) {
	client, err := g.clients.InstallationClient(installationID)
	if err != nil {
		return nil, fmt.Errorf("%w: installation client", ErrUnavailable)
	}
	return client, nil
}

func (g *GitHubAPI) Repository(ctx context.Context, installationID, repoID int64) (Repo, error) {
	client, err := g.client(installationID)
	if err != nil {
		return Repo{}, err
	}
	repo, response, err := client.Repositories.GetByID(ctx, repoID)
	if err != nil {
		return Repo{}, classify(response, err)
	}
	if repo.GetID() != repoID || repo.GetOwner().GetLogin() == "" || repo.GetName() == "" {
		return Repo{}, fmt.Errorf("%w: repository identity changed", ErrRejected)
	}
	return Repo{Owner: repo.GetOwner().GetLogin(), Name: repo.GetName()}, nil
}

func (g *GitHubAPI) owned(c *github.IssueComment) bool {
	return c.GetPerformedViaGithubApp().GetID() == g.appID || c.GetUser().GetLogin() == g.botLogin
}

func (g *GitHubAPI) GetComment(ctx context.Context, installationID int64, repo Repo, id int64) (Comment, error) {
	client, err := g.client(installationID)
	if err != nil {
		return Comment{}, err
	}
	comment, response, err := client.Issues.GetComment(ctx, repo.Owner, repo.Name, id)
	if err != nil {
		return Comment{}, classify(response, err)
	}
	return Comment{ID: comment.GetID(), Body: comment.GetBody(), AppOwned: g.owned(comment)}, nil
}

func (g *GitHubAPI) UpdateComment(ctx context.Context, installationID int64, repo Repo, id int64, body string) error {
	client, err := g.client(installationID)
	if err != nil {
		return err
	}
	_, response, err := client.Issues.UpdateComment(ctx, repo.Owner, repo.Name, id, github.IssueCommentRequest{Body: body})
	if err != nil {
		return classify(response, err)
	}
	return nil
}

func (g *GitHubAPI) CreateComment(ctx context.Context, installationID int64, repo Repo, number int32, body string) (int64, error) {
	client, err := g.client(installationID)
	if err != nil {
		return 0, err
	}
	comment, response, err := client.Issues.CreateComment(ctx, repo.Owner, repo.Name, int(number), github.IssueCommentRequest{Body: body})
	if err != nil {
		return 0, classify(response, err)
	}
	return comment.GetID(), nil
}

func (g *GitHubAPI) FindComment(ctx context.Context, installationID int64, repo Repo, number int32, marker string, maxPages int) (Comment, bool, error) {
	client, err := g.client(installationID)
	if err != nil {
		return Comment{}, false, err
	}
	options := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100, Page: 1}}
	for page := 0; page < maxPages; page++ {
		comments, response, err := client.Issues.ListComments(ctx, repo.Owner, repo.Name, int(number), options)
		if err != nil {
			return Comment{}, false, classify(response, err)
		}
		for _, c := range comments {
			if g.owned(c) && strings.Contains(c.GetBody(), marker) {
				return Comment{ID: c.GetID(), Body: c.GetBody(), AppOwned: true}, true, nil
			}
		}
		if response.NextPage == 0 {
			break
		}
		options.Page = response.NextPage
	}
	return Comment{}, false, nil
}

func (g *GitHubAPI) FindCheckRun(ctx context.Context, installationID int64, repo Repo, headSHA, name, externalID string) (int64, bool, error) {
	client, err := g.client(installationID)
	if err != nil {
		return 0, false, err
	}
	filter := "all"
	options := &github.ListCheckRunsOptions{CheckName: &name, AppID: &g.appID, Filter: &filter, ListOptions: github.ListOptions{PerPage: 100, Page: 1}}
	for page := 0; page < 5; page++ {
		result, response, err := client.Checks.ListCheckRunsForRef(ctx, repo.Owner, repo.Name, headSHA, options)
		if err != nil {
			return 0, false, classify(response, err)
		}
		for _, run := range result.CheckRuns {
			if run.GetExternalID() == externalID {
				return run.GetID(), true, nil
			}
		}
		if response.NextPage == 0 {
			break
		}
		options.Page = response.NextPage
	}
	return 0, false, nil
}

func (g *GitHubAPI) output(run CheckRun) *github.CheckRunOutput {
	title, summary := run.Title, run.Summary
	return &github.CheckRunOutput{Title: &title, Summary: &summary}
}

func optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func (g *GitHubAPI) CreateCheckRun(ctx context.Context, installationID int64, repo Repo, run CheckRun) (int64, error) {
	client, err := g.client(installationID)
	if err != nil {
		return 0, err
	}
	options := github.CreateCheckRunOptions{
		Name: run.Name, HeadSHA: run.HeadSHA, ExternalID: optional(run.ExternalID), DetailsURL: optional(run.Details),
		Status: optional(run.Status), Conclusion: optional(run.Conclusion), Output: g.output(run),
	}
	if run.Status == "completed" {
		options.CompletedAt = &github.Timestamp{Time: g.now()}
	}
	created, response, err := client.Checks.CreateCheckRun(ctx, repo.Owner, repo.Name, options)
	if err != nil {
		return 0, classify(response, err)
	}
	return created.GetID(), nil
}

func (g *GitHubAPI) UpdateCheckRun(ctx context.Context, installationID int64, repo Repo, id int64, run CheckRun) error {
	client, err := g.client(installationID)
	if err != nil {
		return err
	}
	options := github.UpdateCheckRunOptions{
		Name: run.Name, ExternalID: optional(run.ExternalID), DetailsURL: optional(run.Details),
		Status: optional(run.Status), Conclusion: optional(run.Conclusion), Output: g.output(run),
	}
	if run.Status == "completed" {
		options.CompletedAt = &github.Timestamp{Time: g.now()}
	}
	_, response, err := client.Checks.UpdateCheckRun(ctx, repo.Owner, repo.Name, id, options)
	if err != nil {
		return classify(response, err)
	}
	return nil
}

// classify maps a GitHub failure to a stable category. Rate limits carry
// GitHub's own delay.
func classify(response *github.Response, err error) error {
	var rate *github.RateLimitError
	if errors.As(err, &rate) {
		return &RateLimited{RetryAfter: max(time.Until(rate.Rate.Reset.Time), time.Second)}
	}
	var abuse *github.AbuseRateLimitError
	if errors.As(err, &abuse) {
		delay := time.Minute
		if abuse.RetryAfter != nil {
			delay = *abuse.RetryAfter
		}
		return &RateLimited{RetryAfter: delay}
	}
	status := 0
	if response != nil && response.Response != nil {
		status = response.StatusCode
	}
	switch status {
	case http.StatusNotFound, http.StatusGone:
		return ErrNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrPermissionDenied
	case http.StatusUnprocessableEntity, http.StatusBadRequest:
		return ErrRejected
	}
	return ErrUnavailable
}
