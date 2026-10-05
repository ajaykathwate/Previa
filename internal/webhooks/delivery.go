package webhooks

import (
	"context"
	"fmt"
	"time"

	"github.com/ajaykathwatee/previa/internal/installations"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/pullrequests"
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

type DeliveryService struct {
	pool          *pgxpool.Pool
	observer      pullrequests.Observer
	revoker       revoker
	installations *installations.Repository
	invalidate    func(int64)
	refresher     pullrequests.MergeabilityRefresher
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
func (s *DeliveryService) Process(ctx context.Context, id, kind, hash string, event any) error {
	var installationID int64
	err := database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO webhook_deliveries(delivery_id,event_type,payload_hash) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, id, kind, hash)
		if err != nil {
			return fmt.Errorf("record delivery: %w", err)
		}
		if tag.RowsAffected() == 0 {
			var match bool
			if err := tx.QueryRow(ctx, `SELECT event_type=$2 AND payload_hash=$3 FROM webhook_deliveries WHERE delivery_id=$1`, id, kind, hash).Scan(&match); err != nil {
				return err
			}
			if !match {
				return ErrDeliveryConflict
			}
			return nil
		}
		switch e := event.(type) {
		case *github.PullRequestEvent:
			if e.GetAction() == "" {
				return ErrInvalidEvent
			}
			switch e.GetAction() {
			case "opened", "reopened", "synchronize", "closed":
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
			if e.GetAction() == "closed" && o.State == "open" || e.GetAction() != "closed" && o.State != "open" {
				return ErrInvalidEvent
			}
			return pullrequests.ProcessWebhookTx(ctx, tx, e.GetRepo().GetID(), e.GetInstallation().GetID(), o, s.observer, s.refresher)
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
func (s *DeliveryService) lifecycle(ctx context.Context, tx pgx.Tx, event any, stamp time.Time) error {
	change, err := installations.ParseLifecycleEvent(event)
	if err != nil {
		return ErrInvalidEvent
	}
	if stamp.IsZero() {
		return ErrInvalidEvent
	}
	if _, err := tx.Exec(ctx, `INSERT INTO installations(github_installation_id,status) VALUES($1,'active') ON CONFLICT DO NOTHING`, change.ID); err != nil {
		return err
	}
	var source pgtype.Timestamptz
	if err := tx.QueryRow(ctx, `SELECT source_updated_at FROM installations WHERE github_installation_id=$1 FOR UPDATE`, change.ID).Scan(&source); err != nil {
		return err
	}
	revoke := change.Cleanup || len(change.Removed) > 0
	if _, repositoryEvent := event.(*github.InstallationRepositoriesEvent); repositoryEvent {
		// The installation timestamp is not a selection sequence. Track each repo:
		// independent additions work, while removal wins ambiguous same-time re-adds.
		filter := func(repos []installations.RepositoryInfo, removed bool) ([]installations.RepositoryInfo, error) {
			accepted := make([]installations.RepositoryInfo, 0, len(repos))
			for _, repo := range repos {
				tag, err := tx.Exec(ctx, `INSERT INTO installation_repository_events(installation_id,github_repo_id,source_updated_at,removed)
    VALUES($1,$2,$3,$4) ON CONFLICT(installation_id,github_repo_id) DO UPDATE
    SET source_updated_at=EXCLUDED.source_updated_at,removed=EXCLUDED.removed
    WHERE installation_repository_events.source_updated_at<EXCLUDED.source_updated_at
    OR (installation_repository_events.source_updated_at=EXCLUDED.source_updated_at AND EXCLUDED.removed)`, change.ID, repo.ID, stamp, removed)
				if err != nil {
					return nil, err
				}
				if tag.RowsAffected() > 0 {
					accepted = append(accepted, repo)
				}
			}
			return accepted, nil
		}
		change.Added, err = filter(change.Added, false)
		if err != nil {
			return err
		}
		change.Removed, err = filter(change.Removed, true)
		if err != nil {
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
	_, err = tx.Exec(ctx, `UPDATE installations SET source_updated_at=GREATEST(source_updated_at,$2),cleanup_pending=cleanup_pending OR $3 WHERE github_installation_id=$1`, change.ID, stamp, revoke)
	return err
}
