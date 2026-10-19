package housekeeping

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Lifecycle removes repository-attributable source and build caches after
// execution leases and preview resources have drained. It deliberately has no
// age grace period: explicit erasure is different from routine disk pruning.
type Lifecycle struct {
	pool       *pgxpool.Pool
	caches     Caches
	workspaces Workspaces
}

func NewLifecycle(pool *pgxpool.Pool, caches Caches, workspaces Workspaces) *Lifecycle {
	return &Lifecycle{pool, caches, workspaces}
}
func (l *Lifecycle) RepositoryDrained(ctx context.Context, repo int64) (bool, error) {
	rows, err := l.pool.Query(ctx, `SELECT id FROM pull_requests WHERE repo_id=$1`, repo)
	if err != nil {
		return false, fmt.Errorf("list erased source PRs: %w", err)
	}
	prs, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return false, fmt.Errorf("read erased source PRs: %w", err)
	}
	rows, err = l.pool.Query(ctx, `SELECT id FROM baseline_refreshes WHERE repo_id=$1`, repo)
	if err != nil {
		return false, fmt.Errorf("list erased baseline sources: %w", err)
	}
	baselines, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return false, fmt.Errorf("read erased baseline sources: %w", err)
	}
	ownedPR, ownedBaseline := map[int64]bool{}, map[int64]bool{}
	for _, id := range prs {
		ownedPR[id] = true
	}
	for _, id := range baselines {
		ownedBaseline[id] = true
	}
	if l.workspaces == nil || l.caches == nil {
		return false, nil
	}
	sources, err := l.workspaces.List()
	if err != nil {
		return false, fmt.Errorf("inspect erased source workspaces: %w", err)
	}
	for _, w := range sources {
		if w.Marked && (ownedPR[w.PullRequestID] || ownedBaseline[w.BaselineRefreshID]) {
			if err = l.workspaces.Remove(w); err != nil {
				return false, fmt.Errorf("remove erased source workspace: %w", err)
			}
		}
	}
	var githubRepo *int64
	if err = l.pool.QueryRow(ctx, `SELECT github_repo_id FROM repos WHERE id=$1`, repo).Scan(&githubRepo); err != nil {
		return false, fmt.Errorf("read erased cache identity: %w", err)
	}
	if githubRepo == nil {
		return true, nil
	}
	caches, err := l.caches.Caches(ctx)
	if err != nil {
		return false, fmt.Errorf("inspect erased repository caches: %w", err)
	}
	drained := true
	for _, c := range caches {
		if c.BaseRepoID != *githubRepo {
			continue
		}
		if c.InUse {
			drained = false
			continue
		}
		if err = l.caches.RemoveCache(ctx, c.Name); err != nil {
			return false, fmt.Errorf("remove erased repository cache: %w", err)
		}
	}
	return drained, nil
}
