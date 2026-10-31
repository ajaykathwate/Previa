package onboarding

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/ajaykathwatee/previa/internal/repoconfig"
	"github.com/google/go-github/v91/github"
)

// maxResponseBytes bounds every GitHub answer a check reads, error bodies
// included. A file's JSON carries its content, so a Dockerfile far above this
// is reported as EntryLarge rather than read.
const maxResponseBytes = 256 << 10

var errResponseTooLarge = errors.New("GitHub response exceeds the setup check limit")

// InstallationClients is the GitHub App access checks need;
// *githubapp.Client implements it.
type InstallationClients interface {
	InstallationClient(int64) (*github.Client, error)
}

// GitHubFiles reads single files at an exact commit through the GitHub API.
// It never downloads a repository archive.
type GitHubFiles struct{ clients InstallationClients }

// NewGitHubFiles needs the App's installation clients.
func NewGitHubFiles(clients InstallationClients) (*GitHubFiles, error) {
	if clients == nil {
		return nil, errors.New("setup checks require the GitHub App")
	}
	return &GitHubFiles{clients: clients}, nil
}

// Commit opens one repository, resolved by its immutable GitHub ID so a rename
// cannot point a check elsewhere, at one full commit SHA.
func (g *GitHubFiles) Commit(ctx context.Context, installationID, repoID int64, sha string) (Tree, error) {
	client, err := g.clients.InstallationClient(installationID)
	if err != nil || client == nil {
		return nil, &AttemptError{Code: FailApp}
	}
	httpClient := client.Client() // a copy; the shared client stays untouched
	transport := httpClient.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	httpClient.Transport = boundedTransport{next: transport}
	baseURL, uploadURL := client.BaseURL(), client.UploadURL()
	bounded, err := github.NewClient(github.WithHTTPClient(httpClient), github.WithURLs(&baseURL, &uploadURL))
	if err != nil {
		return nil, &AttemptError{Code: FailApp}
	}
	repo, response, err := bounded.Repositories.GetByID(ctx, repoID)
	if err != nil {
		return nil, classify(ctx, response, err)
	}
	if repo.GetID() != repoID || repo.GetOwner().GetLogin() == "" || repo.GetName() == "" {
		return nil, &AttemptError{Code: FailGitHubError}
	}
	return &gitHubTree{client: bounded, owner: repo.GetOwner().GetLogin(), name: repo.GetName(), sha: sha}, nil
}

type gitHubTree struct {
	client           *github.Client
	owner, name, sha string
}

func (t *gitHubTree) contents(ctx context.Context, name string) (*github.RepositoryContent, []*github.RepositoryContent, *github.Response, error) {
	return t.client.Repositories.GetContents(ctx, t.owner, t.name, name, &github.RepositoryContentGetOptions{Ref: t.sha})
}

func (t *gitHubTree) Config(ctx context.Context) ([]byte, error) {
	data, err := t.Read(ctx, repoconfig.Filename, repoconfig.MaxConfigBytes)
	if !errors.Is(err, ErrMissing) {
		return data, err
	}
	// GitHub answers 404 for an unreadable commit too: only call the file
	// missing once the commit itself is readable.
	sha, response, err := t.client.Repositories.GetCommitSHA1(ctx, t.owner, t.name, t.sha, "")
	switch {
	case err != nil:
		return nil, classify(ctx, response, err)
	case !strings.EqualFold(strings.TrimSpace(sha), t.sha):
		return nil, &AttemptError{Code: FailGitHubError}
	}
	return nil, ErrMissing
}

func (t *gitHubTree) Stat(ctx context.Context, name string) (string, error) {
	file, _, response, err := t.contents(ctx, name)
	switch {
	case err == nil && file == nil:
		return EntryDir, nil
	case err == nil:
		return file.GetType(), nil
	case response != nil && response.StatusCode == http.StatusNotFound:
		return EntryMissing, nil
	case response != nil && response.StatusCode == http.StatusOK && errors.Is(err, errResponseTooLarge):
		return EntryLarge, nil
	}
	return "", classify(ctx, response, err)
}

func (t *gitHubTree) Read(ctx context.Context, name string, limit int64) ([]byte, error) {
	file, _, response, err := t.contents(ctx, name)
	switch {
	case err == nil && (file == nil || file.GetType() != EntryFile):
		return nil, ErrNotRegular
	case err == nil && int64(file.GetSize()) > limit:
		return nil, ErrTooLarge
	case err == nil:
		content, err := file.GetContent()
		if err != nil {
			return nil, &AttemptError{Code: FailGitHubError}
		}
		if int64(len(content)) > limit {
			return nil, ErrTooLarge
		}
		return []byte(content), nil
	case response != nil && response.StatusCode == http.StatusNotFound:
		return nil, ErrMissing
	case response != nil && response.StatusCode == http.StatusOK && errors.Is(err, errResponseTooLarge):
		return nil, ErrTooLarge
	}
	return nil, classify(ctx, response, err)
}

// classify turns a GitHub failure into a stored code, keeping nothing of the
// response itself.
func classify(ctx context.Context, response *github.Response, err error) error {
	var rate *github.RateLimitError
	var abuse *github.AbuseRateLimitError
	switch {
	case ctx.Err() != nil:
		return &AttemptError{Code: FailGitHubDown}
	case errors.As(err, &rate), errors.As(err, &abuse), response != nil && response.StatusCode == http.StatusTooManyRequests:
		return &AttemptError{Code: FailRateLimited}
	case response == nil:
		return &AttemptError{Code: FailGitHubDown}
	case response.StatusCode == http.StatusUnauthorized, response.StatusCode == http.StatusForbidden, response.StatusCode == http.StatusNotFound:
		return &AttemptError{Code: FailAccess}
	case response.StatusCode >= 500, response.StatusCode == http.StatusRequestTimeout:
		return &AttemptError{Code: FailGitHubDown}
	default:
		return &AttemptError{Code: FailGitHubError}
	}
}

// boundedTransport caps response bodies, error bodies included.
type boundedTransport struct{ next http.RoundTripper }

func (t boundedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.next.RoundTrip(request)
	if err != nil {
		return response, err
	}
	response.Body = &boundedBody{reader: &io.LimitedReader{R: response.Body, N: maxResponseBytes + 1}, closer: response.Body}
	return response, nil
}

type boundedBody struct {
	reader *io.LimitedReader
	closer io.Closer
}

func (b *boundedBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if b.reader.N == 0 {
		return n, errResponseTooLarge
	}
	return n, err
}

func (b *boundedBody) Close() error { return b.closer.Close() }
