package pullrequests

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/pullrequests/dbgen"
	"github.com/jackc/pgx/v5"
)

// VerifyTimeout bounds the GitHub reads of one verification.
const VerifyTimeout = 20 * time.Second

// UpstreamSource is the targeted GitHub read used for fresh verification.
type UpstreamSource interface {
	Inspect(ctx context.Context, installationID, repoID int64, number int32) (Upstream, error)
}

var _ environments.UpstreamVerifier = (*SyncService)(nil)

// VerifyUpstream refreshes a tracked PR from GitHub immediately before manual
// authorization or execution. It captures installation/repository revisions and
// the PR generation before any IO, reads GitHub with no transaction open, then
// records the result under installation → repository → PR locks. The observation
// is committed even when the outcome is a refusal, so a closure or conflict seen
// here is never lost. Verification never authorizes work; it only carries
// existing event-authorized Automatic intent across a recorded status change.
func (s *SyncService) VerifyUpstream(ctx context.Context, pullRequestID int64) (environments.UpstreamSnapshot, error) {
	if pullRequestID <= 0 {
		return environments.UpstreamSnapshot{}, environments.ErrUpstreamInaccessible
	}
	source, ok := s.source.(UpstreamSource)
	if !ok {
		return environments.UpstreamSnapshot{}, environments.ErrUpstreamUnavailable
	}
	before, captured, err := s.captureVerification(ctx, pullRequestID)
	if err != nil {
		return environments.UpstreamSnapshot{}, verificationScopeError(err)
	}
	if captured.State != environments.StateOpen {
		return environments.UpstreamSnapshot{}, environments.ErrUpstreamClosed
	}

	ioCtx, cancel := context.WithTimeout(ctx, VerifyTimeout)
	upstream, err := source.Inspect(ioCtx, before.install.GithubInstallationID, before.repo.GithubRepoID.Int64, captured.Number)
	cancel()
	if err != nil {
		return environments.UpstreamSnapshot{}, err
	}
	if upstream.Number != captured.Number {
		return environments.UpstreamSnapshot{}, environments.ErrUpstreamChanged
	}

	var snapshot environments.UpstreamSnapshot
	var outcome error
	err = database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		rec, err := s.recordUpstreamTx(ctx, tx, before, captured, upstream)
		if err != nil {
			return err
		}
		outcome = verificationOutcome(rec.saved, upstream)
		if errors.Is(outcome, environments.ErrPRChecking) && rec.saved.BuildStatus == "checking" && s.jobs != nil {
			// Still checking mergeability: make sure a check is coming.
			if err := s.RefreshMergeabilityTx(ctx, tx, captured.RepoID, rec.saved.Number, rec.authorized); err != nil {
				return err
			}
		}
		snapshot = upstreamSnapshot(rec, upstream)
		return nil
	})
	if err != nil {
		return environments.UpstreamSnapshot{}, verificationScopeError(err)
	}
	if outcome != nil {
		return environments.UpstreamSnapshot{}, outcome
	}
	return snapshot, nil
}

// captureVerification reads, under the usual locks, the PR and the
// installation and repository revisions that recording checks later.
func (s *SyncService) captureVerification(ctx context.Context, pullRequestID int64) (before scope, captured dbgen.PullRequest, err error) {
	err = database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		identity, err := q.FindPRIdentity(ctx, pullRequestID)
		if err != nil {
			return scopeError(err)
		}
		if before, err = lockScope(ctx, tx, identity.RepoID, 0); err != nil {
			return err
		}
		if captured, err = q.LockPR(ctx, dbgen.LockPRParams(identity)); err != nil {
			return scopeError(err)
		}
		return nil
	})
	return before, captured, err
}

// recorded is a PR as verification recorded it, with the scope it was
// recorded in and whether Automatic intent already authorized its build.
type recorded struct {
	after      scope
	saved      dbgen.PullRequest
	authorized bool
}

// recordUpstreamTx records GitHub's view of the PR unless the installation,
// repository or PR changed since it was captured. A recorded change fences
// in-flight discovery and carries existing Automatic authorization over.
func (s *SyncService) recordUpstreamTx(ctx context.Context, tx pgx.Tx, before scope, captured dbgen.PullRequest, upstream Upstream) (recorded, error) {
	after, err := lockScope(ctx, tx, captured.RepoID, 0)
	if err != nil {
		return recorded{}, err
	}
	if !after.unchanged(before) {
		return recorded{}, environments.ErrUpstreamChanged
	}
	q := dbgen.New(tx)
	key := dbgen.LockPRParams{RepoID: captured.RepoID, Number: captured.Number}
	pr, err := q.LockPR(ctx, key)
	if err != nil {
		return recorded{}, scopeError(err)
	}
	if pr.Generation != captured.Generation {
		return recorded{}, environments.ErrUpstreamChanged // A concurrent observation won; read again.
	}
	authorized, err := q.HasAutomaticIntent(ctx, dbgen.HasAutomaticIntentParams{PullRequestID: pr.ID, Generation: pr.Generation})
	if err != nil {
		return recorded{}, fmt.Errorf("check existing automatic authorization: %w", err)
	}
	if _, err := ObserveTx(ctx, tx, captured.RepoID, upstream.Observation, false, s.observer); err != nil {
		return recorded{}, err
	}
	saved, err := q.LockPR(ctx, key)
	if err != nil {
		return recorded{}, scopeError(err)
	}
	if saved.Generation != pr.Generation {
		// A recorded change fences any in-flight discovery page snapshot.
		if err := q.BumpObservation(ctx, captured.RepoID); err != nil {
			return recorded{}, fmt.Errorf("fence discovery after verification: %w", err)
		}
		if authorized && s.observer != nil {
			if _, err := s.observer.ObserveTx(ctx, tx, environments.NewRequest(saved.ID, saved.Generation, saved.HeadSha, saved.State)); err != nil {
				return recorded{}, err
			}
		}
	}
	return recorded{after: after, saved: saved, authorized: authorized}, nil
}

// verificationOutcome refuses a recorded PR that may not build now: closed,
// at another commit than GitHub showed (an older or ambiguous view; never
// guess), or blocked by its mergeability.
func verificationOutcome(saved dbgen.PullRequest, upstream Upstream) error {
	switch {
	case saved.State != environments.StateOpen:
		return environments.ErrUpstreamClosed
	case saved.HeadSha != upstream.SHA:
		return environments.ErrUpstreamChanged
	}
	return environments.BuildBlockReason(saved.BuildStatus, saved.NeedsRefresh)
}

func upstreamSnapshot(rec recorded, upstream Upstream) environments.UpstreamSnapshot {
	saved, base := rec.saved, rec.after.repo.GithubRepoID.Int64
	headRepoID := int64(0)
	if saved.HeadRepoID.Valid {
		headRepoID = saved.HeadRepoID.Int64
	}
	return environments.UpstreamSnapshot{
		PullRequestID: saved.ID, Number: saved.Number, Generation: saved.Generation, HeadSHA: saved.HeadSha,
		InstallationID: rec.after.install.GithubInstallationID, BaseRepoID: base,
		HeadRepoID: headRepoID, Fork: saved.IsFork || headRepoID != base,
		Owner: upstream.Owner, Repository: upstream.Repository,
	}
}

func verificationScopeError(err error) error {
	if errors.Is(err, ErrUnavailable) {
		return environments.ErrUpstreamInaccessible
	}
	return err
}
