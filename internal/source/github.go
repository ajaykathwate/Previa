package source

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/go-github/v91/github"
)

// InstallationClients is satisfied by platform/githubapp.Client.
type InstallationClients interface {
	InstallationClient(int64) (*github.Client, error)
}

// GitHubArchives downloads the tarball for one exact commit of the base
// repository. The installation token goes only to the GitHub API request that
// returns the archive location; the archive is then fetched from an allowlisted
// HTTPS host by a separate client that carries no credentials, so a redirect can
// never forward them. Fork commits are fetched the same way: GitHub serves PR head
// commits through the base repository, so no access to the fork is needed.
type GitHubArchives struct {
	clients      InstallationClients
	download     *http.Client
	allowedHosts map[string]bool
	maxRedirects int
}

// ArchiveOption customizes GitHubArchives, mainly for tests and GitHub Enterprise.
type ArchiveOption func(*GitHubArchives)

// WithDownloadClient replaces the credential-free download client. Its redirect
// policy is always replaced by the allowlist check.
func WithDownloadClient(client *http.Client) ArchiveOption {
	return func(a *GitHubArchives) { a.download = client }
}

// WithArchiveHosts replaces the allowlist of archive hosts (host[:port]).
func WithArchiveHosts(hosts ...string) ArchiveOption {
	return func(a *GitHubArchives) {
		a.allowedHosts = map[string]bool{}
		for _, host := range hosts {
			a.allowedHosts[strings.ToLower(host)] = true
		}
	}
}

// WithMaxRedirects bounds archive-download redirects.
func WithMaxRedirects(n int) ArchiveOption {
	return func(a *GitHubArchives) { a.maxRedirects = n }
}

var errRedirectRejected = errors.New("archive redirect target is not allowed")

func NewGitHubArchives(clients InstallationClients, options ...ArchiveOption) (*GitHubArchives, error) {
	if clients == nil {
		return nil, errors.New("source archives require installation authentication")
	}
	a := &GitHubArchives{
		clients:      clients,
		allowedHosts: map[string]bool{"codeload.github.com": true},
		maxRedirects: DefaultLimits().MaxRedirects,
		download: &http.Client{Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true, MaxIdleConns: 4,
			TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second, IdleConnTimeout: 30 * time.Second,
		}},
	}
	for _, option := range options {
		option(a)
	}
	if a.download == nil {
		return nil, errors.New("source archives require a download client")
	}
	client := *a.download // Never mutate a caller's client.
	client.CheckRedirect = a.checkRedirect
	client.Jar = nil
	a.download = &client
	return a, nil
}

func (a *GitHubArchives) allowed(u *url.URL) bool {
	return u != nil && u.Scheme == "https" && u.User == nil && a.allowedHosts[strings.ToLower(u.Host)]
}

func (a *GitHubArchives) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > a.maxRedirects || !a.allowed(req.URL) {
		return errRedirectRejected
	}
	req.Header.Del("Authorization")
	req.Header.Del("Cookie")
	return nil
}

// Open returns the gzip tarball stream for request.CommitSHA and its declared
// length (-1 when unknown). The caller must close the stream.
func (a *GitHubArchives) Open(ctx context.Context, request Request) (io.ReadCloser, int64, error) {
	client, err := a.clients.InstallationClient(request.InstallationID)
	if err != nil || client == nil {
		return nil, 0, failure(CodeAuthUnavailable, "Installation authentication is unavailable; check the GitHub App configuration.", nil)
	}
	repo, response, err := client.Repositories.GetByID(ctx, request.BaseRepoID)
	if err != nil {
		return nil, 0, apiFailure(ctx, response, err)
	}
	if repo.GetID() != request.BaseRepoID || repo.GetOwner().GetLogin() == "" || repo.GetName() == "" {
		return nil, 0, failure(CodeUnavailable, "GitHub returned a different repository than requested.", nil)
	}
	link, response, err := client.Repositories.GetArchiveLink(ctx, repo.GetOwner().GetLogin(), repo.GetName(), github.Tarball, &github.RepositoryContentGetOptions{Ref: request.CommitSHA}, 0)
	if err != nil {
		return nil, 0, apiFailure(ctx, response, err)
	}
	if !a.allowed(link) {
		return nil, 0, failure(CodeRedirectRejected, "GitHub returned an archive location that is not on an allowed host.", nil)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link.String(), nil)
	if err != nil {
		return nil, 0, failure(CodeAPIError, "GitHub returned an unusable archive location.", nil)
	}
	req.Header.Set("User-Agent", "previa-source")
	resp, err := a.download.Do(req)
	if err != nil {
		return nil, 0, downloadFailure(ctx, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, 0, statusFailure(resp.StatusCode)
	}
	return resp.Body, resp.ContentLength, nil
}

func apiFailure(ctx context.Context, response *github.Response, err error) error {
	if ctx.Err() != nil {
		return &Error{Code: CodeCancelled, Message: "Source preparation was cancelled or timed out.", Retryable: errors.Is(ctx.Err(), context.DeadlineExceeded), cause: ctx.Err()}
	}
	var rate *github.RateLimitError
	if errors.As(err, &rate) {
		return &RetryAfter{Delay: max(time.Second, time.Until(rate.Rate.Reset.Time)), Err: retryable(CodeRateLimited, "GitHub rate limit reached; retry after it resets.", nil)}
	}
	var abuse *github.AbuseRateLimitError
	if errors.As(err, &abuse) {
		delay := time.Minute
		if abuse.RetryAfter != nil {
			delay = *abuse.RetryAfter
		}
		return &RetryAfter{Delay: delay, Err: retryable(CodeRateLimited, "GitHub secondary rate limit reached; retry later.", nil)}
	}
	if response == nil {
		return retryable(CodeAPIUnavailable, "GitHub could not be reached; retry source preparation.", nil)
	}
	return statusFailure(response.StatusCode)
}

func statusFailure(status int) error {
	switch {
	case status == http.StatusNotFound || status == http.StatusGone || status == http.StatusUnprocessableEntity:
		return failure(CodeUnavailable, "The repository or commit is unavailable, or this installation cannot read it.", nil)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return failure(CodeAccessDenied, "GitHub denied access to the repository archive; check repository access and App permissions.", nil)
	case status == http.StatusTooManyRequests:
		return retryable(CodeRateLimited, "GitHub rate limit reached; retry after it resets.", nil)
	case status >= 500 || status == http.StatusRequestTimeout:
		return retryable(CodeAPIError, "GitHub could not provide the archive right now; retry.", nil)
	default:
		return failure(CodeAPIError, "GitHub could not provide the repository archive.", nil)
	}
}

// downloadFailure never keeps the transport error: url.Error embeds the archive
// URL, whose query string carries a short-lived access token.
func downloadFailure(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, errRedirectRejected):
		return failure(CodeRedirectRejected, "The archive download redirected to a host that is not allowed.", nil)
	case ctx.Err() != nil:
		return &Error{Code: CodeCancelled, Message: "Source preparation was cancelled or timed out.", Retryable: errors.Is(ctx.Err(), context.DeadlineExceeded), cause: ctx.Err()}
	default:
		return retryable(CodeAPIUnavailable, "The archive download failed; retry source preparation.", nil)
	}
}
