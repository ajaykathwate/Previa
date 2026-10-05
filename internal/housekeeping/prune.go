package housekeeping

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ajaykathwatee/previa/internal/builds"
	"github.com/ajaykathwatee/previa/internal/housekeeping/dbgen"
	"github.com/ajaykathwatee/previa/internal/platform/metrics"
	"github.com/ajaykathwatee/previa/internal/source"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Caches lists and removes build caches and untagged images;
// *builds.Builder implements it.
type Caches interface {
	Caches(ctx context.Context) ([]builds.Cache, error)
	RemoveCache(ctx context.Context, name string) error
	DanglingImages(ctx context.Context) ([]string, error)
	RemoveDanglingImage(ctx context.Context, id string) error
}

// Workspaces lists and removes source workspaces on this worker's disk;
// *source.Workspaces implements it. Without one, workspaces are not pruned.
type Workspaces interface {
	List() ([]source.WorkspaceInfo, error)
	Remove(info source.WorkspaceInfo) error
}

// Prune frees disk within budgets: images that lost their tag, source
// workspaces older than any build may run, the build caches of repositories
// this instance can no longer build, and then the oldest unused caches until
// all caches fit BUILD_CACHE_BUDGET (half of it in an emergency, when a build
// waits for disk space). Nothing in use is removed: Docker refuses to remove
// an image a container runs or a volume a builder holds, and active previews'
// images are tagged. Expired build logs are pruned by the build-log retention
// job. A dry run only logs.
type Prune struct {
	caches     Caches
	workspaces Workspaces
	pool       *pgxpool.Pool
	logger     *slog.Logger
	settings   Settings
	now        func() time.Time
}

func NewPrune(caches Caches, workspaces Workspaces, pool *pgxpool.Pool, logger *slog.Logger, settings Settings) (*Prune, error) {
	if caches == nil || pool == nil || logger == nil {
		return nil, errors.New("pruning requires caches, a database and a logger")
	}
	if err := settings.validate(); err != nil {
		return nil, err
	}
	return &Prune{caches: caches, workspaces: workspaces, pool: pool, logger: logger, settings: settings, now: time.Now}, nil
}

// PrunePass is one pass's outcome.
type PrunePass struct {
	Emergency  bool
	Images     int   // untagged images removed (or that would be)
	Workspaces int   // leftover workspaces removed (or that would be)
	Caches     int   // caches removed (or that would be)
	CacheBytes int64 // their size
}

// count records what a pass deleted; a dry run deletes nothing.
func (p *Prune) count(pass PrunePass) {
	if p.settings.DryRun {
		return
	}
	metrics.Housekeeping("prune", "images", pass.Images)
	metrics.Housekeeping("prune", "workspaces", pass.Workspaces)
	metrics.Housekeeping("prune", "caches", pass.Caches)
}

func (p *Prune) RunOnce(ctx context.Context, emergency bool) (PrunePass, error) {
	pass := PrunePass{Emergency: emergency}
	var errs []error
	images, err := p.caches.DanglingImages(ctx)
	if err != nil {
		errs = append(errs, err)
	}
	for _, id := range images {
		p.logger.InfoContext(ctx, "untagged image pruned", "dry_run", p.settings.DryRun, "image_id", id)
		pass.Images++
		if !p.settings.DryRun {
			if err := p.caches.RemoveDanglingImage(ctx, id); err != nil {
				errs = append(errs, err)
			}
		}
	}
	var workspaces []source.WorkspaceInfo
	if p.workspaces != nil {
		if workspaces, err = p.workspaces.List(); err != nil {
			errs = append(errs, err)
		}
	}
	for _, w := range workspaces {
		if p.now().Sub(w.CreatedAt) < p.settings.WorkspaceMaxAge {
			continue // newest last: possibly a running build's
		}
		p.logger.InfoContext(ctx, "source workspace pruned", "dry_run", p.settings.DryRun, "pr_id", w.PullRequestID, "created_at", w.CreatedAt)
		pass.Workspaces++
		if !p.settings.DryRun {
			if err := p.workspaces.Remove(w); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if err := p.pruneCaches(ctx, emergency, &pass); err != nil {
		errs = append(errs, err)
	}
	if pass.Images > 0 || pass.Workspaces > 0 || pass.Caches > 0 {
		p.logger.InfoContext(ctx, "housekeeping prune", "dry_run", p.settings.DryRun, "emergency", emergency, "images", pass.Images,
			"workspaces", pass.Workspaces, "caches", pass.Caches, "cache_bytes", pass.CacheBytes)
	}
	return pass, errors.Join(errs...)
}

func (p *Prune) pruneCaches(ctx context.Context, emergency bool, pass *PrunePass) error {
	caches, err := p.caches.Caches(ctx)
	if err != nil {
		return err
	}
	active, err := dbgen.New(p.pool).ActiveRepositoryIDs(ctx)
	if err != nil {
		return fmt.Errorf("list buildable repositories: %w", err)
	}
	buildable := make(map[int64]bool, len(active))
	for _, id := range active {
		buildable[id] = true
	}
	budget := p.settings.CacheBudget
	if emergency {
		budget /= 2
	}
	var total int64
	for _, c := range caches {
		total += max(c.Size, 0)
	}
	var errs []error
	remove := func(c builds.Cache, reason string) {
		p.logger.InfoContext(ctx, "build cache pruned", "dry_run", p.settings.DryRun, "volume", c.Name, "base_repository_id", c.BaseRepoID,
			"bytes", c.Size, "reason", reason)
		pass.Caches++
		pass.CacheBytes += max(c.Size, 0)
		total -= max(c.Size, 0)
		if !p.settings.DryRun {
			if err := p.caches.RemoveCache(ctx, c.Name); err != nil {
				errs = append(errs, err)
			}
		}
	}
	kept := caches[:0:0]
	for _, c := range caches {
		if !c.InUse && !buildable[c.BaseRepoID] {
			remove(c, "repository_unavailable")
			continue
		}
		kept = append(kept, c)
	}
	for _, c := range kept { // oldest first
		if total <= budget {
			break
		}
		if !c.InUse {
			remove(c, "over_budget")
		}
	}
	return errors.Join(errs...)
}
