package repoconfig

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-github/v91/github"
)

const FetchTimeout = 15 * time.Second
const maxResponseBytes = 128 * 1024 // Bounded JSON/base64 envelope, including errors.

var errResponseTooLarge = errors.New("GitHub configuration response exceeds limit")

// InstallationClients is satisfied by platform/githubapp.Client. Its transport
// continues to own token minting/caching; this feature never handles token values.
type InstallationClients interface {
	InstallationClient(int64) (*github.Client, error)
}

type GitHubFetcher struct {
	clients InstallationClients
}

func NewGitHubFetcher(clients InstallationClients) (*GitHubFetcher, error) {
	if clients == nil {
		return nil, errors.New("repository config requires installation authentication")
	}
	return &GitHubFetcher{clients: clients}, nil
}

func (f *GitHubFetcher) Fetch(ctx context.Context, request Request) ([]byte, error) {
	if err := request.validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	client, err := f.clients.InstallationClient(request.InstallationID)
	if err != nil || client == nil {
		return nil, &FetchError{Code: "authentication_unavailable", Message: "Installation authentication is unavailable; check the GitHub App configuration."}
	}
	// Client() returns a copy. Wrap its existing authenticated transport without
	// mutating the shared client, and bound successful AND error-body reads.
	httpClient := client.Client()
	transport := httpClient.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	httpClient.Transport = responseBound{transport: transport}
	baseURL, uploadURL := client.BaseURL(), client.UploadURL()
	bounded, err := github.NewClient(github.WithHTTPClient(httpClient), github.WithURLs(&baseURL, &uploadURL))
	if err != nil {
		return nil, &FetchError{Code: "authentication_unavailable", Message: "Installation API client configuration is invalid."}
	}
	file, _, response, err := bounded.Repositories.GetContents(ctx, request.Owner, request.Repository, Filename, &github.RepositoryContentGetOptions{Ref: request.CommitSHA})
	if err != nil {
		if response != nil && response.StatusCode == http.StatusNotFound {
			// GitHub masks unavailable repositories/commits as 404. Only call the
			// file missing after proving this installation can read this commit.
			sha, commitResponse, commitErr := bounded.Repositories.GetCommitSHA1(ctx, request.Owner, request.Repository, request.CommitSHA, "")
			if commitErr == nil && strings.EqualFold(strings.TrimSpace(sha), request.CommitSHA) {
				return nil, &FetchError{Code: "missing_file", Message: "Add .previewrc.yaml to the repository root at the requested commit."}
			}
			if commitErr == nil {
				return nil, &FetchError{Code: "invalid_response", Message: "GitHub could not verify access to the requested commit."}
			}
			return nil, classifyFetchError(ctx, commitResponse, commitErr)
		}
		return nil, classifyFetchError(ctx, response, err)
	}
	if file == nil || file.GetType() != "file" {
		return nil, &FetchError{Code: "invalid_file", Message: ".previewrc.yaml must be a regular repository file."}
	}
	if file.GetSize() < 0 || file.GetSize() > MaxConfigBytes {
		return nil, &FetchError{Code: "too_large", Message: "Configuration must not exceed 64 KiB."}
	}
	content, err := file.GetContent()
	if err != nil {
		return nil, &FetchError{Code: "invalid_response", Message: "GitHub returned an unsupported or malformed file response."}
	}
	if len(content) > MaxConfigBytes {
		return nil, &FetchError{Code: "too_large", Message: "Configuration must not exceed 64 KiB."}
	}
	return []byte(content), nil
}

func classifyFetchError(ctx context.Context, response *github.Response, err error) error {
	if ctx.Err() != nil {
		return &FetchError{Code: "request_cancelled", Message: "Configuration fetch was cancelled or timed out.", Retryable: errors.Is(ctx.Err(), context.DeadlineExceeded), cause: ctx.Err()}
	}
	if errors.Is(err, errResponseTooLarge) {
		return &FetchError{Code: "too_large", Message: "GitHub's configuration response exceeds the safe size limit."}
	}
	var rateLimit *github.RateLimitError
	var abuseLimit *github.AbuseRateLimitError
	if errors.As(err, &rateLimit) || errors.As(err, &abuseLimit) || (response != nil && response.StatusCode == http.StatusTooManyRequests) {
		return &FetchError{Code: "rate_limited", Message: "GitHub rate limit reached; retry after the limit resets.", Retryable: true}
	}
	if response == nil {
		return &FetchError{Code: "api_unavailable", Message: "GitHub could not be reached; retry the configuration fetch.", Retryable: true}
	}
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return &FetchError{Code: "access_denied", Message: "Installation access was denied; check repository access and App permissions."}
	case http.StatusNotFound:
		return &FetchError{Code: "resource_unavailable", Message: "The repository or commit is unavailable, or this installation cannot access it."}
	default:
		return &FetchError{Code: "api_error", Message: "GitHub could not provide the requested configuration.", Retryable: response.StatusCode >= 500 || response.StatusCode == http.StatusRequestTimeout}
	}
}

type responseBound struct {
	transport http.RoundTripper
}

func (t responseBound) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.transport.RoundTrip(request)
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
