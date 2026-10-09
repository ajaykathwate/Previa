package environments

import (
	"context"
	"errors"
	"time"
)

// UpstreamSnapshot is a tracked PR as GitHub reported it moments ago, already
// recorded under the existing installation → repository → PR fences. Owner and
// Repository are current names for API paths only; identity is numeric.
type UpstreamSnapshot struct {
	PullRequestID  int64
	Number         int32
	Generation     int64
	HeadSHA        string
	InstallationID int64 // GitHub installation ID
	BaseRepoID     int64 // GitHub repository ID of the PR's base
	HeadRepoID     int64 // GitHub repository ID of the head; 0 when unknown or deleted
	Fork           bool  // true unless the head is provably the base repository
	Owner          string
	Repository     string
}

// Matches reports whether the snapshot still describes the captured request.
// A mismatch means the request is superseded, never that it should be retried.
func (s UpstreamSnapshot) Matches(r Request) bool {
	return s.PullRequestID == r.PullRequestID && s.Generation == r.Generation && s.HeadSHA == r.HeadSHA && r.State == StateOpen
}

// UpstreamVerifier refreshes one tracked PR from GitHub immediately before manual
// authorization and before executing eligible work. Network IO happens outside
// transactions; the result is reconciled under the usual lock order. Verifying is
// observation only: it never authorizes work, and it preserves (but does not
// create) event-authorized Automatic intent. Implementations fail closed with the
// errors below or with ErrPRChecking, ErrPRConflicts or ErrPRRefresh.
type UpstreamVerifier interface {
	VerifyUpstream(ctx context.Context, pullRequestID int64) (UpstreamSnapshot, error)
}

var (
	// ErrUpstreamUnavailable: GitHub could not be reached or answered with a
	// server error. Retryable.
	ErrUpstreamUnavailable = errors.New("GitHub could not confirm the pull request right now; retry shortly")
	// ErrUpstreamChanged: installation access, repository observations or the PR
	// itself changed while verification was in flight. Retryable with a fresh read.
	ErrUpstreamChanged = errors.New("the pull request changed while it was being verified; retry with its latest state")
	// ErrUpstreamInaccessible: the installation can no longer read the repository
	// or PR, or local access was revoked. Terminal until access is restored.
	ErrUpstreamInaccessible = errors.New("the repository or pull request is no longer accessible to this installation")
	// ErrUpstreamClosed: GitHub reports the PR closed or merged. Terminal.
	ErrUpstreamClosed = errors.New("the pull request is closed or merged")
)

// UpstreamRetryAfter wraps a retryable verification failure with GitHub's
// rate-limit delay.
type UpstreamRetryAfter struct {
	Delay time.Duration
	Err   error
}

func (e *UpstreamRetryAfter) Error() string { return e.Err.Error() }
func (e *UpstreamRetryAfter) Unwrap() error { return e.Err }

// UpstreamRetryable reports whether a verification failure may succeed later
// without anyone changing the repository or the PR.
func UpstreamRetryable(err error) bool {
	return errors.Is(err, ErrUpstreamUnavailable) || errors.Is(err, ErrUpstreamChanged) || errors.Is(err, ErrPRChecking)
}
