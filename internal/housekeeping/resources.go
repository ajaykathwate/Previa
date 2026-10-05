package housekeeping

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/housekeeping/dbgen"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Leftovers lists and removes this instance's Docker objects by their labels;
// *runtime.Runtime implements it. Objects of other Previa instances on the
// same Docker host are never listed.
type Leftovers interface {
	environments.ResourceInventory
	// PullRequests returns the PRs this instance's objects belong to, with the
	// newest creation time among each PR's objects.
	PullRequests(ctx context.Context) (map[int64]time.Time, error)
	RemoveResource(ctx context.Context, resource environments.Resource) error
}

// Resources finds orphaned Docker objects. For PRs the database knows, the
// environments reconciler compares each object with the active, pending,
// route-protected and operation-held artifacts and schedules durable cleanup
// for what nothing references. Objects of PRs the database no longer has (for
// example after restoring an older backup) are removed directly, after
// verifying their labels. Nothing younger than the grace period is touched,
// and a dry run only logs.
type Resources struct {
	reconciler *environments.Reconciler
	leftovers  Leftovers
	pool       *pgxpool.Pool
	logger     *slog.Logger
	grace      time.Duration
	dryRun     bool
	now        func() time.Time
}

func NewResources(repo *environments.Repository, leftovers Leftovers, pool *pgxpool.Pool, logger *slog.Logger, settings Settings) (*Resources, error) {
	if repo == nil || leftovers == nil || pool == nil || logger == nil {
		return nil, errors.New("resource reconciliation requires a repository, leftovers, a database and a logger")
	}
	if err := settings.validate(); err != nil {
		return nil, err
	}
	r := &Resources{leftovers: leftovers, pool: pool, logger: logger, grace: settings.Grace, dryRun: settings.DryRun, now: time.Now}
	reconciler, err := environments.NewReconciler(repo, leftovers, logger, settings.BatchSize)
	if err != nil {
		return nil, err
	}
	r.reconciler = reconciler.WithGrace(settings.Grace, func() time.Time { return r.now() })
	if settings.DryRun {
		r.reconciler.WithDryRun()
	}
	return r, nil
}

// ResourcesPass is one pass's outcome.
type ResourcesPass struct {
	environments.InventoryPass
	UnknownPRs int                     // PRs with objects but no database row
	Removed    []environments.Resource // unknown PRs' objects removed (or that would be)
}

func (r *Resources) RunOnce(ctx context.Context) (ResourcesPass, error) {
	var pass ResourcesPass
	inventory, err := r.reconciler.Inventory(ctx)
	pass.InventoryPass = inventory
	if err != nil {
		return pass, err
	}
	found, err := r.leftovers.PullRequests(ctx)
	if err != nil {
		return pass, fmt.Errorf("list this instance's objects: %w", err)
	}
	ids := make([]int64, 0, len(found))
	for id := range found {
		ids = append(ids, id)
	}
	known, err := dbgen.New(r.pool).KnownPullRequests(ctx, ids)
	if err != nil {
		return pass, fmt.Errorf("match objects with PRs: %w", err)
	}
	isKnown := make(map[int64]bool, len(known))
	for _, id := range known {
		isKnown[id] = true
	}
	var errs []error
	for _, id := range ids {
		if isKnown[id] {
			continue
		}
		pass.UnknownPRs++
		if r.now().Sub(found[id]) < r.grace {
			continue // Possibly created in the moment before its PR row committed.
		}
		removed, err := r.removeUnknown(ctx, id)
		pass.Removed = append(pass.Removed, removed...)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return pass, errors.Join(errs...)
}

// removeUnknown removes an unknown PR's objects: containers before the
// images they run and the networks they join.
func (r *Resources) removeUnknown(ctx context.Context, pullRequestID int64) ([]environments.Resource, error) {
	resources, err := r.leftovers.Resources(ctx, pullRequestID)
	if err != nil {
		return nil, fmt.Errorf("list objects of unknown PR %d: %w", pullRequestID, err)
	}
	rank := func(res environments.Resource) int {
		switch {
		case res.Artifact.ContainerID != "":
			return 0
		case res.Artifact.ImageTag != "":
			return 1
		}
		return 2
	}
	sort.SliceStable(resources, func(i, j int) bool { return rank(resources[i]) < rank(resources[j]) })
	var removed []environments.Resource
	var errs []error
	for _, res := range resources {
		if r.grace > 0 && (res.CreatedAt.IsZero() || r.now().Sub(res.CreatedAt) < r.grace) {
			continue
		}
		r.logger.InfoContext(ctx, "object of unknown PR", "dry_run", r.dryRun, "pr_id", pullRequestID,
			"container_id", res.Artifact.ContainerID, "image", res.Artifact.ImageTag, "network", res.Network)
		removed = append(removed, res)
		if r.dryRun {
			continue
		}
		if err := r.leftovers.RemoveResource(ctx, res); err != nil {
			errs = append(errs, fmt.Errorf("remove object of unknown PR %d: %w", pullRequestID, err))
		}
	}
	return removed, errors.Join(errs...)
}
