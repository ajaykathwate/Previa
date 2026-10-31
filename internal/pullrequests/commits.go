package pullrequests

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ajaykathwatee/previa/internal/pullrequests/dbgen"
	"github.com/jackc/pgx/v5/pgtype"
)

// SaveHeadCommit caches display details for a PR's still-current head commit.
func (r *Repository) SaveHeadCommit(ctx context.Context, id int64, sha string, c CommitDetails) error {
	err := dbgen.New(r.pool).SaveHeadCommit(ctx, dbgen.SaveHeadCommitParams{ID: id, Sha: sha, Message: c.Message, Author: c.Author,
		CommittedAt: pgtype.Timestamptz{Time: c.At, Valid: !c.At.IsZero()}})
	if err != nil {
		return fmt.Errorf("save head commit details: %w", err)
	}
	return nil
}

// CommitDetails is a commit's message and authorship, for display only.
type CommitDetails struct {
	Message, Author string
	At              time.Time
}

// CommitSource reads one commit of a tracked repository from GitHub.
type CommitSource interface {
	Commit(ctx context.Context, installationID, repoID int64, sha string) (CommitDetails, error)
}

// WithCommits shows the head commit's message on the PR page.
func (h *Handler) WithCommits(source CommitSource) *Handler { h.commits = source; return h }

const (
	commitFetchTimeout = 2500 * time.Millisecond
	commitRetryAfter   = 10 * time.Minute
	maxCommitMessage   = 4000
)

// headCommit returns the cached head commit, fetching it once per head SHA. A
// failed fetch is retried at most every commitRetryAfter, and never delays the
// page by more than commitFetchTimeout.
func (h *Handler) headCommit(ctx context.Context, row dbgen.OwnedPRRow) CommitDetails {
	cached := CommitDetails{Message: row.HeadCommitMessage, Author: row.HeadCommitAuthor, At: row.HeadCommitAt.Time}
	fresh := row.HeadCommitSha == row.HeadSha && (cached.Message != "" || time.Since(row.HeadCommitCheckedAt.Time) < commitRetryAfter)
	if fresh || h.commits == nil || row.GithubInstallationID <= 0 || row.GithubRepoNumber <= 0 {
		if row.HeadCommitSha != row.HeadSha {
			return CommitDetails{}
		}
		return cached
	}
	fetchCtx, cancel := context.WithTimeout(ctx, commitFetchTimeout)
	defer cancel()
	c, err := h.commits.Commit(fetchCtx, row.GithubInstallationID, row.GithubRepoNumber, row.HeadSha)
	if err != nil {
		var limited *RetryAfterError
		if !errors.As(err, &limited) {
			h.logger.InfoContext(ctx, "head commit details unavailable", "pull_request", row.ID)
		}
		c = CommitDetails{}
	}
	c.Message = strings.TrimSpace(c.Message)
	for utf8.RuneCountInString(c.Message) > maxCommitMessage {
		c.Message = string([]rune(c.Message)[:maxCommitMessage])
	}
	saveCtx, done := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer done()
	if err := h.repo.SaveHeadCommit(saveCtx, row.ID, row.HeadSha, c); err != nil {
		h.logger.WarnContext(ctx, "cache head commit details", "error", err)
	}
	return c
}
