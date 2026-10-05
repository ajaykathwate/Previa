package housekeeping

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/housekeeping/dbgen"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Verifier reads one tracked PR from GitHub and records what it found under
// the usual fences; *pullrequests.SyncService implements it. It never
// authorizes work.
type Verifier interface {
	VerifyUpstream(ctx context.Context, pullRequestID int64) (environments.UpstreamSnapshot, error)
}

// PullRequests confirms live PRs with GitHub, so a close, a new commit or a
// mergeability change missed while webhooks were down still takes effect: a
// recorded close starts the teardown, a recorded new commit supersedes the old
// request. Only open PRs with a preview or an unfinished build request are
// read, at most PullRequestBatch per pass and each at most once per
// PullRequestInterval, with installation tokens. Discovery stays open-only:
// a PR missing from a listing is never taken as closed.
type PullRequests struct {
	pool     *pgxpool.Pool
	verifier Verifier
	logger   *slog.Logger
	batch    int32
	interval time.Duration
	now      func() time.Time
}

func NewPullRequests(pool *pgxpool.Pool, verifier Verifier, logger *slog.Logger, settings Settings) (*PullRequests, error) {
	if pool == nil || verifier == nil || logger == nil || settings.PullRequestBatch <= 0 {
		return nil, errors.New("PR reconciliation requires a database, a verifier, a logger and a batch size")
	}
	return &PullRequests{pool: pool, verifier: verifier, logger: logger, batch: settings.PullRequestBatch, interval: PullRequestInterval, now: time.Now}, nil
}

// ConfirmPass is one pass's outcome.
type ConfirmPass struct {
	Outcomes map[string]int
	// RetryAfter is GitHub's rate-limit delay when it stopped the pass.
	RetryAfter time.Duration
}

func (p *PullRequests) RunOnce(ctx context.Context) (ConfirmPass, error) {
	pass := ConfirmPass{Outcomes: map[string]int{}}
	q := dbgen.New(p.pool)
	ids, err := q.ListPullRequestsToConfirm(ctx, dbgen.ListPullRequestsToConfirmParams{
		StaleBefore: pgtype.Timestamptz{Time: p.now().Add(-p.interval), Valid: true}, BatchSize: p.batch,
	})
	if err != nil {
		return pass, fmt.Errorf("list PRs to confirm: %w", err)
	}
	for _, id := range ids {
		_, err := p.verifier.VerifyUpstream(ctx, id)
		outcome, delay := outcomeOf(err)
		if ctx.Err() != nil {
			return pass, ctx.Err()
		}
		if outcome == "rate_limited" {
			// Stop the pass; the PR stays first in line for the next one.
			pass.RetryAfter = delay
			pass.Outcomes[outcome]++
			p.logger.WarnContext(ctx, "GitHub rate limit stopped PR reconciliation", "retry_after", delay)
			break
		}
		pass.Outcomes[outcome]++
		if err := q.RecordReconciliation(ctx, dbgen.RecordReconciliationParams{
			PullRequestID: id, ReconciledAt: pgtype.Timestamptz{Time: p.now(), Valid: true}, Outcome: outcome,
		}); err != nil {
			return pass, fmt.Errorf("record PR %d reconciliation: %w", id, err)
		}
		if outcome != "current" {
			p.logger.InfoContext(ctx, "PR reconciled", "pr_id", id, "outcome", outcome)
		}
	}
	return pass, nil
}

// outcomeOf turns a verification result into a stable category; raw errors
// never reach logs or the database.
func outcomeOf(err error) (string, time.Duration) {
	var retry *environments.UpstreamRetryAfter
	switch {
	case err == nil:
		return "current", 0
	case errors.As(err, &retry):
		return "rate_limited", retry.Delay
	case errors.Is(err, environments.ErrUpstreamClosed):
		return "closed", 0
	case errors.Is(err, environments.ErrUpstreamChanged):
		return "changed", 0
	case errors.Is(err, environments.ErrUpstreamInaccessible):
		return "inaccessible", 0
	case errors.Is(err, environments.ErrUpstreamUnavailable):
		return "unavailable", 0
	case errors.Is(err, environments.ErrPRChecking), errors.Is(err, environments.ErrPRConflicts), errors.Is(err, environments.ErrPRRefresh):
		return "blocked", 0
	}
	return "error", 0
}
