package installations

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/metrics"
	"github.com/google/go-github/v91/github"
)

var ErrReconciliationCircuit = errors.New("installation reconciliation circuit is open; operator review required")
var ErrAppIdentity = errors.New("installation reconciliation App identity could not be verified")

type ReconciliationApp interface {
	App(context.Context) (*github.App, error)
	Installation(context.Context, int64) (*github.Installation, error)
	InstallationClient(int64) (*github.Client, error)
}
type ReconciliationApplier interface {
	Reconcile(context.Context, []ReconciliationChange) ([]ReconciliationChange, error)
}
type Reconciler struct {
	repo       *Repository
	app        ReconciliationApp
	apply      ReconciliationApplier
	log        *slog.Logger
	appID      int64
	maxChanges int
}

func NewReconciler(repo *Repository, app ReconciliationApp, apply ReconciliationApplier, log *slog.Logger, appID int64, maxChanges int) *Reconciler {
	if maxChanges == 0 {
		maxChanges = 5
	}
	return &Reconciler{repo: repo, app: app, apply: apply, log: log, appID: appID, maxChanges: maxChanges}
}

// Run observes the entire bounded batch before any mutation. A failed or partial
// observation cannot leave half a destructive pass committed.
func (s *Reconciler) Run(ctx context.Context, target int64) error { return s.run(ctx, target, false) }
func (s *Reconciler) run(ctx context.Context, target int64, explicitRelink bool) error {
	if s.appID <= 0 {
		return s.failure(ctx, ErrAppIdentity)
	}
	snapshots, total, err := s.repo.ReconciliationSnapshot(ctx, target)
	if err != nil {
		return s.failure(ctx, err)
	}
	app, err := s.app.App(ctx)
	if err != nil || app.GetID() != s.appID {
		return s.failure(ctx, ErrAppIdentity)
	}
	changes := make([]ReconciliationChange, 0, len(snapshots))
	whole, units := 0, 0
	for _, snapshot := range snapshots {
		change, e := s.observe(ctx, snapshot)
		if e != nil {
			return s.failure(ctx, e)
		}
		if change.Status != "" {
			whole++
			units++
		} else if len(change.Removed) > 0 && len(change.Removed) == len(snapshot.Repositories) {
			whole++
		}
		units += len(change.Removed)
		changes = append(changes, change)
	}
	if units > s.maxChanges || (!explicitRelink && whole > 0 && whole >= total) {
		metrics.InstallationReconciliation("circuit_open", 1)
		s.log.ErrorContext(ctx, "installation reconciliation circuit opened; no access revoked", "changes", units, "installations", whole, "known_installations", total)
		return ErrReconciliationCircuit
	}
	applied, err := s.apply.Reconcile(ctx, changes)
	if err != nil {
		return s.failure(ctx, err)
	}
	for _, change := range applied {
		outcome := "unchanged"
		count := 1
		switch {
		case change.Status == "inactive":
			outcome = "deleted"
		case change.Status == "suspended":
			outcome = "suspended"
		case len(change.Removed) > 0:
			outcome = "repository_removed"
			count = len(change.Removed)
		}
		metrics.InstallationReconciliation(outcome, count)
		if outcome != "unchanged" {
			s.log.WarnContext(ctx, "missed GitHub lifecycle delivery recovered", "outcome", outcome, "installation_id", change.ID, "count", count)
		}
	}
	return nil
}
func (s *Reconciler) failure(ctx context.Context, err error) error {
	metrics.InstallationReconciliation("error", 1)
	s.log.ErrorContext(ctx, "installation reconciliation failed; no unverified access revoked", "identity_failure", errors.Is(err, ErrAppIdentity))
	return err
}
func (s *Reconciler) observe(ctx context.Context, snapshot InstallationSnapshot) (ReconciliationChange, error) {
	c := ReconciliationChange{InstallationSnapshot: snapshot}
	i, err := s.app.Installation(ctx, snapshot.ID)
	if err != nil {
		var response *github.ErrorResponse
		if errors.As(err, &response) && response.Response != nil && response.Response.StatusCode == http.StatusNotFound {
			c.Status = "inactive"
			return c, nil
		}
		return c, fmt.Errorf("verify installation snapshot: %w", err)
	}
	if i.GetID() != snapshot.ID || i.GetAppID() != s.appID {
		return c, ErrAppIdentity
	}
	if i.SuspendedAt != nil {
		if snapshot.Status != "suspended" {
			c.Status = "suspended"
		}
		return c, nil
	}
	// A recovered suspension is restored only by a signed delivery or relinking.
	if snapshot.Status != "active" {
		return c, nil
	}
	repositories, err := s.repositories(ctx, snapshot.ID)
	if err != nil {
		return c, err
	}
	for _, repo := range snapshot.Repositories {
		if !repositories[repo.ID] {
			c.Removed = append(c.Removed, repo)
		}
	}
	return c, nil
}
func (s *Reconciler) repositories(ctx context.Context, id int64) (map[int64]bool, error) {
	client, err := s.app.InstallationClient(id)
	if err != nil {
		return nil, fmt.Errorf("create reconciliation installation client: %w", err)
	}
	seen := make(map[int64]bool)
	total := -1
	for page := 1; page <= 100; page++ {
		list, response, e := client.Apps.ListRepos(ctx, &github.ListOptions{Page: page, PerPage: 100})
		if e != nil {
			return nil, fmt.Errorf("read complete installation repository list: %w", e)
		}
		if list == nil || response == nil || list.TotalCount == nil || list.Repositories == nil || list.GetTotalCount() < 0 {
			return nil, errors.New("incomplete installation repository response")
		}
		if total < 0 {
			total = list.GetTotalCount()
		}
		if total != list.GetTotalCount() || total > 10000 {
			return nil, errors.New("installation repository listing changed or exceeded bound")
		}
		for _, repo := range list.Repositories {
			if repo.GetID() <= 0 || seen[repo.GetID()] {
				return nil, errors.New("invalid or repeated installation repository identity")
			}
			seen[repo.GetID()] = true
		}
		if response.NextPage == 0 {
			if len(seen) != total {
				return nil, errors.New("partial installation repository listing")
			}
			return seen, nil
		}
		if response.NextPage != page+1 {
			return nil, errors.New("invalid installation repository pagination")
		}
	}
	return nil, errors.New("installation repository pagination exceeded bound")
}

// ResolveLinkConflicts is an explicit, externally verified owner/admin action,
// not automatic reconciliation. Only a confirmed deletion may unblock relinking.
func (s *Reconciler) ResolveLinkConflicts(ctx context.Context, user, id int64, repos []RepositoryInfo) error {
	conflicts, err := s.repo.LinkConflicts(ctx, user, id, repos)
	if err != nil {
		return err
	}
	for _, old := range conflicts {
		if err = s.runDeletedLink(ctx, old); err != nil {
			return err
		}
	}
	return nil
}
func (s *Reconciler) runDeletedLink(ctx context.Context, id int64) error {
	app, err := s.app.App(ctx)
	if err != nil || app.GetID() != s.appID {
		return s.failure(ctx, ErrAppIdentity)
	}
	_, err = s.app.Installation(ctx, id)
	var response *github.ErrorResponse
	if !errors.As(err, &response) || response.Response == nil || response.Response.StatusCode != 404 {
		return ErrForbidden
	}
	return s.run(ctx, id, true)
}

const ReconciliationInterval = 5 * time.Minute
