package environments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments/dbgen"
)

type ResourceInventory interface {
	Resources(context.Context, int64) ([]Resource, error)
}

// RouteSyncer rewrites the router's configuration from the accepted route
// table when it is missing or behind, for example after a crash between an
// acceptance and its write.
type RouteSyncer interface {
	Sync(context.Context) error
}

// Reconciler repairs lost operation wake-ups and schedules cleanup for labeled
// runtime resources that are not referenced by an active or pending environment.
// Inventory calls run outside database transactions. With WithRoutes it also
// rewrites the router's configuration from the accepted route table.
type Reconciler struct {
	repo      *Repository
	inventory ResourceInventory
	routes    RouteSyncer
	logger    *slog.Logger
	batchSize int32
	afterID   int64
	grace     time.Duration
	dryRun    bool
	now       func() time.Time
}

// WithRoutes also rewrites the router's configuration on each pass.
func (r *Reconciler) WithRoutes(routes RouteSyncer) *Reconciler {
	r.routes = routes
	return r
}

// WithGrace leaves resources younger than grace alone, so an object a build
// or start is still creating is never taken for an orphan. Resources whose
// creation time is unknown are left alone too.
func (r *Reconciler) WithGrace(grace time.Duration, now func() time.Time) *Reconciler {
	r.grace = max(grace, 0)
	if now != nil {
		r.now = now
	}
	return r
}

// WithDryRun logs what each pass would clean up and schedules nothing.
func (r *Reconciler) WithDryRun() *Reconciler {
	r.dryRun = true
	return r
}

// InventoryPass is one bounded inventory pass's outcome.
type InventoryPass struct {
	Inspected int        // PRs whose resources were listed
	Orphans   []Resource // scheduled for cleanup, or that would be in a dry run
	Young     int        // left alone: younger than the grace period
	Missing   []int64    // PRs whose active preview container no longer exists
}

func NewReconciler(repo *Repository, inventory ResourceInventory, logger *slog.Logger, batchSize int32) (*Reconciler, error) {
	if repo == nil || inventory == nil || logger == nil {
		return nil, errors.New("environment reconciler requires repository, inventory, and logger")
	}
	if batchSize <= 0 {
		return nil, errors.New("environment reconciler batch size must be positive")
	}
	return &Reconciler{repo: repo, inventory: inventory, logger: logger, batchSize: batchSize, now: time.Now}, nil
}

// RunOnce is bounded: it recovers a batch of outbox operations and inventories a
// batch of PRs. Call it periodically; the in-memory cursor only avoids repeatedly
// scanning the first page during one process lifetime, while durable operation
// rows make restarts safe.
func (r *Reconciler) RunOnce(ctx context.Context) error {
	if _, err := r.repo.RecoverOperations(ctx, r.batchSize); err != nil {
		return err
	}
	if r.routes != nil {
		if err := r.routes.Sync(ctx); err != nil {
			return fmt.Errorf("sync router configuration: %w", err)
		}
	}
	_, err := r.Inventory(ctx)
	return err
}

// Inventory lists a bounded batch of PRs' Docker resources and schedules
// cleanup for those nothing references any more, past the grace period. In a
// dry run it only logs. Each decision is logged.
func (r *Reconciler) Inventory(ctx context.Context) (InventoryPass, error) {
	var pass InventoryPass
	rows, err := dbgen.New(r.repo.pool).ListPullRequestsForReconciliation(ctx, dbgen.ListPullRequestsForReconciliationParams{
		AfterID: r.afterID, BatchSize: r.batchSize,
	})
	if err != nil {
		return pass, fmt.Errorf("list PRs for resource reconciliation: %w", err)
	}
	if len(rows) == 0 {
		r.afterID = 0
		return pass, nil
	}
	for _, row := range rows {
		request := NewRequest(row.ID, row.Generation, row.HeadSha, row.State)
		// Read the environment before listing Docker: an active container
		// absent from a later listing really is gone.
		before, err := r.repo.Snapshot(ctx, row.ID)
		if err != nil {
			return pass, fmt.Errorf("read environment of PR %d: %w", row.ID, err)
		}
		resources, err := r.inventory.Resources(ctx, row.ID)
		if err != nil {
			return pass, fmt.Errorf("inspect resources for PR %d: %w", row.ID, err)
		}
		if active := before.Artifact.ContainerID; before.Status == "running" && active != "" && !listsContainer(resources, active) {
			r.logger.WarnContext(ctx, "active preview container is missing", "dry_run", r.dryRun, "pr_id", row.ID, "container_id", active)
			pass.Missing = append(pass.Missing, row.ID)
			if !r.dryRun {
				if _, err := r.repo.MarkActiveMissing(ctx, row.ID, active); err != nil {
					return pass, err
				}
			}
		}
		old := resources[:0:0]
		for _, resource := range resources {
			if r.grace > 0 && (resource.CreatedAt.IsZero() || r.now().Sub(resource.CreatedAt) < r.grace) {
				pass.Young++
				continue
			}
			old = append(old, resource)
		}
		var orphans []Resource
		if r.dryRun {
			orphans, err = r.repo.PlanResourceCleanup(ctx, request, old)
		} else {
			orphans, err = r.repo.scheduleResourceCleanup(ctx, request, old, false)
		}
		if err != nil {
			return pass, fmt.Errorf("schedule orphan cleanup for PR %d: %w", row.ID, err)
		}
		for _, o := range orphans {
			r.logger.InfoContext(ctx, "orphaned resource", "dry_run", r.dryRun, "pr_id", o.PullRequestID, "generation", o.Generation,
				"revision", o.Revision, "container_id", o.Artifact.ContainerID, "image", o.Artifact.ImageTag, "network", o.Network)
		}
		pass.Orphans = append(pass.Orphans, orphans...)
		pass.Inspected++
		r.afterID = row.ID
	}
	r.logger.InfoContext(ctx, "environment reconciliation pass completed", "last_pr_id", r.afterID, "prs_inspected", pass.Inspected,
		"orphans", len(pass.Orphans), "within_grace", pass.Young, "dry_run", r.dryRun)
	return pass, nil
}

func listsContainer(resources []Resource, id string) bool {
	for _, r := range resources {
		if r.Artifact.ContainerID == id {
			return true
		}
	}
	return false
}
