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
	var before scope
	var captured dbgen.PullRequest
	err := database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		identity, err := q.FindPRIdentity(ctx, pullRequestID)
		if err != nil {
			return scopeError(err)
		}
		before, err = lockScope(ctx, tx, identity.RepoID, 0)
		if err != nil {
			return err
		}
		captured, err = q.LockPR(ctx, dbgen.LockPRParams(identity))
		if err != nil {
			return scopeError(err)
		}
		return nil
	})
	if err != nil {
		return environments.UpstreamSnapshot{}, verificationScopeError(err)
	}
	if captured.State != "open" {
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
		after, err := lockScope(ctx, tx, captured.RepoID, 0)
		if err != nil {
			return err
		}
		if before.install.Revision != after.install.Revision || before.repo.ObservationVersion != after.repo.ObservationVersion {
			return environments.ErrUpstreamChanged
		}
		q := dbgen.New(tx)
		pr, err := q.LockPR(ctx, dbgen.LockPRParams{RepoID: captured.RepoID, Number: captured.Number})
		if err != nil {
			return scopeError(err)
		}
		if pr.Generation != captured.Generation {
			return environments.ErrUpstreamChanged // A concurrent observation won; read again.
		}
		authorized, err := q.HasAutomaticIntent(ctx, dbgen.HasAutomaticIntentParams{PullRequestID: pr.ID, Generation: pr.Generation})
		if err != nil {
			return fmt.Errorf("check existing automatic authorization: %w", err)
		}
		if _, err := ObserveTx(ctx, tx, captured.RepoID, upstream.Observation, false, s.observer); err != nil {
			return err
		}
		saved, err := q.LockPR(ctx, dbgen.LockPRParams{RepoID: captured.RepoID, Number: captured.Number})
		if err != nil {
			return scopeError(err)
		}
		if saved.Generation != pr.Generation {
			// A recorded change fences any in-flight discovery page snapshot.
			if err := q.BumpObservation(ctx, captured.RepoID); err != nil {
				return fmt.Errorf("fence discovery after verification: %w", err)
			}
			if authorized && s.observer != nil {
				if _, err := s.observer.ObserveTx(ctx, tx, environments.NewRequest(saved.ID, saved.Generation, saved.HeadSha, saved.State)); err != nil {
					return err
				}
			}
		}
		switch {
		case saved.State != "open":
			outcome = environments.ErrUpstreamClosed
		case saved.HeadSha != upstream.SHA:
			outcome = environments.ErrUpstreamChanged // Older or ambiguous upstream view; never guess.
		default:
			if blocked := environments.BuildBlockReason(saved.BuildStatus, saved.NeedsRefresh); blocked != nil {
				outcome = blocked
				if saved.BuildStatus == "checking" && !saved.NeedsRefresh && s.jobs != nil {
					if err := s.RefreshMergeabilityTx(ctx, tx, captured.RepoID, saved.Number, authorized); err != nil {
						return err
					}
				}
			}
		}
		headRepoID := int64(0)
		if saved.HeadRepoID.Valid {
			headRepoID = saved.HeadRepoID.Int64
		}
		snapshot = environments.UpstreamSnapshot{
			PullRequestID: saved.ID, Number: saved.Number, Generation: saved.Generation, HeadSHA: saved.HeadSha,
			InstallationID: after.install.GithubInstallationID, BaseRepoID: after.repo.GithubRepoID.Int64,
			HeadRepoID: headRepoID, Fork: saved.IsFork || headRepoID != after.repo.GithubRepoID.Int64,
			Owner: upstream.Owner, Repository: upstream.Repository,
		}
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

func verificationScopeError(err error) error {
	if errors.Is(err, ErrUnavailable) {
		return environments.ErrUpstreamInaccessible
	}
	return err
}
