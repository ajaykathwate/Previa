package webhooks

import (
	"context"
	"fmt"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/installations"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/pullrequests"
	"github.com/ajaykathwatee/previa/internal/webhooks/dbgen"
	"github.com/google/go-github/v91/github"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type metadataScheduler interface {
	installations.InitialDiscoverer
	pullrequests.MergeabilityRefresher
}

// revoker requests teardown of revoked repositories' pull requests inside the
// lifecycle transaction; environments.Broker implements it.
type revoker interface {
	RevokeTx(ctx context.Context, tx pgx.Tx, githubInstallationID int64, allRepositories bool, githubRepoIDs []int64) (int, error)
}

// lifecycleApplier records an installation lifecycle change;
// *installations.Repository implements it.
type lifecycleApplier interface {
	ApplyEventTx(ctx context.Context, tx pgx.Tx, change installations.LifecycleChange) error
}

// mergeFollower follows a pull request merged into a repository's branch,
// inside the delivery transaction: a test-data baseline refresh
// (docs/32-persistent-test-data.md). *sampledata.Store implements it.
type mergeFollower interface {
	MergedTx(ctx context.Context, tx pgx.Tx, installationID, githubRepoID int64, base, defaultBranch string) error
}

type DeliveryService struct {
	pool          *pgxpool.Pool
	observer      pullrequests.Observer
	revoker       revoker
	installations lifecycleApplier
	invalidate    func(int64)
	refresher     pullrequests.MergeabilityRefresher
	merges        mergeFollower
}

func NewDeliveryService(pool *pgxpool.Pool, observer pullrequests.Observer, invalidate func(int64), syncs ...metadataScheduler) *DeliveryService {
	s := &DeliveryService{pool: pool, observer: observer, installations: installations.NewRepository(pool), invalidate: invalidate}
	// The coordinator that observes PR events also records revocation teardowns.
	if r, ok := observer.(revoker); ok {
		s.revoker = r
	}
	if len(syncs) > 0 {
		s.refresher = syncs[0]
		s.installations = installations.NewRepository(pool, syncs[0])
	}
	return s
}

// WithMerges follows merged pull requests in the same transaction as their
// state change.
func (s *DeliveryService) WithMerges(m mergeFollower) *DeliveryService {
	s.merges = m
	return s
}

// Process records a verified delivery once and applies its event in the same
// transaction. A redelivery is a no-op; a reused ID with another event is
// ErrDeliveryConflict. Installation caches are invalidated after commit.
func (s *DeliveryService) Process(ctx context.Context, id, kind, hash string, event any) error {
	var installationID int64
	err := database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		if fresh, err := recordDelivery(ctx, dbgen.New(tx), id, kind, hash); err != nil || !fresh {
			return err
		}
		switch e := event.(type) {
		case *github.PullRequestEvent:
			return s.pullRequest(ctx, tx, e)
		case *github.InstallationEvent:
			if e.GetAction() == "" {
				return ErrInvalidEvent
			}
			switch e.GetAction() {
			case "created", "deleted", "suspend", "unsuspend", "new_permissions_accepted":
			default:
				return nil
			}
			installationID = e.GetInstallation().GetID()
			return s.lifecycle(ctx, tx, e, e.GetInstallation().GetUpdatedAt().Time)
		case *github.InstallationRepositoriesEvent:
			if e.GetAction() == "" {
				return ErrInvalidEvent
			}
			if e.GetAction() != "added" && e.GetAction() != "removed" {
				return nil
			}
			installationID = e.GetInstallation().GetID()
			return s.lifecycle(ctx, tx, e, e.GetInstallation().GetUpdatedAt().Time)
		default:
			return ErrInvalidEvent
		}
	})
	if err == nil && installationID > 0 && s.invalidate != nil {
		s.invalidate(installationID)
	}
	return err
}

// recordDelivery records the delivery ID and reports whether it is new.
func recordDelivery(ctx context.Context, q *dbgen.Queries, id, kind, hash string) (bool, error) {
	recorded, err := q.RecordDelivery(ctx, dbgen.RecordDeliveryParams{DeliveryID: id, EventType: kind, PayloadHash: hash})
	if err != nil {
		return false, fmt.Errorf("record delivery: %w", err)
	}
	if recorded > 0 {
		return true, nil
	}
	match, err := q.DeliveryMatches(ctx, dbgen.DeliveryMatchesParams{DeliveryID: id, EventType: kind, PayloadHash: hash})
	if err != nil {
		return false, err
	}
	if !match {
		return false, ErrDeliveryConflict
	}
	return false, nil
}

// pullRequest applies an opened, reopened, synchronized, edited or closed PR.
func (s *DeliveryService) pullRequest(ctx context.Context, tx pgx.Tx, e *github.PullRequestEvent) error {
	if e.GetAction() == "" {
		return ErrInvalidEvent
	}
	switch e.GetAction() {
	case "opened", "reopened", "synchronize", "closed", "edited":
	default:
		return nil
	}
	if e.GetInstallation().GetID() <= 0 || e.GetRepo().GetID() <= 0 {
		return ErrInvalidEvent
	}
	o, err := pullrequests.FromGitHub(e.GetPullRequest(), e.GetRepo().GetID())
	if err != nil {
		return fmt.Errorf("%w: incomplete PR", ErrInvalidEvent)
	}
	if e.GetAction() == "edited" {
		return pullrequests.ProcessMetadataWebhookTx(ctx, tx, e.GetRepo().GetID(), e.GetInstallation().GetID(), o)
	}
	closing := e.GetAction() == "closed"
	if closing == (o.State == environments.StateOpen) {
		return ErrInvalidEvent // a close must report a closed PR, every other action an open one
	}
	if err := pullrequests.ProcessWebhookTx(ctx, tx, e.GetRepo().GetID(), e.GetInstallation().GetID(), o, s.observer, s.refresher); err != nil {
		return err
	}
	if closing && e.GetPullRequest().GetMerged() && s.merges != nil {
		return s.merges.MergedTx(ctx, tx, e.GetInstallation().GetID(), e.GetRepo().GetID(), e.GetPullRequest().GetBase().GetRef(), e.GetRepo().GetDefaultBranch())
	}
	return nil
}

// lifecycle applies an installation or repository-selection change unless
// it is older than what is recorded, revokes what it removes, and records the
// installation's source time and pending cleanup.
func (s *DeliveryService) lifecycle(ctx context.Context, tx pgx.Tx, event any, stamp time.Time) error {
	change, err := installations.ParseLifecycleEvent(event)
	if err != nil || stamp.IsZero() {
		return ErrInvalidEvent
	}
	q := dbgen.New(tx)
	if err := q.EnsureInstallation(ctx, change.ID); err != nil {
		return err
	}
	source, err := q.LockInstallationSourceTime(ctx, change.ID)
	if err != nil {
		return err
	}
	revoke := change.Cleanup || len(change.Removed) > 0
	if _, repositoryEvent := event.(*github.InstallationRepositoriesEvent); repositoryEvent {
		if err := acceptRepositoryChanges(ctx, q, &change, stamp); err != nil {
			return err
		}
		revoke = len(change.Removed) > 0
	} else if change.Status != "inactive" && source.Valid && (stamp.Before(source.Time) || stamp.Equal(source.Time) && !revoke) {
		return nil
	}
	if err := s.installations.ApplyEventTx(ctx, tx, change); err != nil {
		return err
	}
	if revoke && s.revoker != nil {
		// Suspension and deletion revoke every repository; removal, the listed ones.
		removed := make([]int64, 0, len(change.Removed))
		for _, repo := range change.Removed {
			removed = append(removed, repo.ID)
		}
		if _, err := s.revoker.RevokeTx(ctx, tx, change.ID, change.Cleanup, removed); err != nil {
			return err
		}
	}
	return q.FinishInstallationEvent(ctx, dbgen.FinishInstallationEventParams{GithubInstallationID: change.ID, Stamp: timestamp(stamp), Revoke: revoke})
}

// acceptRepositoryChanges keeps only the added and removed repositories whose
// own event is newer than the last one recorded for them. The installation
// timestamp is not a selection sequence: independent additions work, while a
// removal wins an ambiguous same-time re-add.
func acceptRepositoryChanges(ctx context.Context, q *dbgen.Queries, change *installations.LifecycleChange, stamp time.Time) error {
	accept := func(repos []installations.RepositoryInfo, removed bool) ([]installations.RepositoryInfo, error) {
		accepted := make([]installations.RepositoryInfo, 0, len(repos))
		for _, repo := range repos {
			recorded, err := q.RecordRepositoryEvent(ctx, dbgen.RecordRepositoryEventParams{
				InstallationID: change.ID, GithubRepoID: repo.ID, SourceUpdatedAt: timestamp(stamp), Removed: removed,
			})
			if err != nil {
				return nil, err
			}
			if recorded > 0 {
				accepted = append(accepted, repo)
			}
		}
		return accepted, nil
	}
	var err error
	if change.Added, err = accept(change.Added, false); err != nil {
		return err
	}
	change.Removed, err = accept(change.Removed, true)
	return err
}

func timestamp(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
