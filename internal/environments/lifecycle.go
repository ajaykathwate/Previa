package environments

import (
	"context"
	"errors"
	"fmt"

	"github.com/ajaykathwatee/previa/internal/environments/dbgen"

	"github.com/jackc/pgx/v5"
)

// Lifecycle preserves the feature's teardown fences and verifies Docker before
// a repository's product records can be erased. It does not require GitHub.
type Lifecycle struct {
	repo      *Repository
	broker    *Broker
	inventory ResourceInventory
}

func NewLifecycle(repo *Repository, broker *Broker, inventory ResourceInventory) *Lifecycle {
	return &Lifecycle{repo: repo, broker: broker, inventory: inventory}
}

func (l *Lifecycle) FenceRepositoryTx(ctx context.Context, tx pgx.Tx, repo int64) error {
	var installation, githubRepo int64
	if err := tx.QueryRow(ctx, `SELECT i.github_installation_id,coalesce(r.github_repo_id,0) FROM repos r JOIN installations i ON i.id=r.installation_id WHERE r.id=$1`, repo).Scan(&installation, &githubRepo); err != nil {
		return fmt.Errorf("read repository teardown identity: %w", err)
	}
	if _, err := l.broker.RevokeTx(ctx, tx, installation, false, []int64{githubRepo}); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT id,generation FROM pull_requests WHERE repo_id=$1 AND teardown_requested_generation=generation AND teardown_completed_generation<generation ORDER BY id FOR UPDATE`, repo)
	if err != nil {
		return fmt.Errorf("list disconnected preview fences: %w", err)
	}
	type target struct{ ID, Generation int64 }
	targets, err := pgx.CollectRows(rows, pgx.RowToStructByPos[target])
	if err != nil {
		return fmt.Errorf("read disconnected preview fences: %w", err)
	}
	for _, p := range targets {
		if _, err = l.repo.startTeardownTx(ctx, tx, p.ID); err != nil {
			return err
		}
		// Accept route tombstones in the disconnect transaction too. A restore's
		// initial route sync can then never republish this removed preview.
		q := dbgen.New(tx)
		env, err := q.LockRouteEnvironment(ctx, p.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("lock disconnected route environment: %w", err)
		}
		route, err := q.LockRoute(ctx, env.ID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("lock disconnected route: %w", err)
		}
		if err == nil && compareVersion(route.Generation, route.Revision, p.Generation, 0) >= 0 {
			continue
		}
		version, err := q.NextRouteTableVersion(ctx)
		if err != nil {
			return fmt.Errorf("advance disconnected route table: %w", err)
		}
		if _, err = q.SaveRemovedRoute(ctx, dbgen.SaveRemovedRouteParams{EnvironmentID: env.ID, PullRequestID: p.ID, Generation: p.Generation, TableVersion: version}); err != nil {
			return fmt.Errorf("fence disconnected route: %w", err)
		}
	}
	return nil
}

func (l *Lifecycle) RepositoryDrained(ctx context.Context, repo int64) (bool, error) {
	if l.inventory == nil {
		return false, nil
	}
	var outstanding bool
	err := l.repo.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pull_requests p WHERE p.repo_id=$1 AND
		(p.teardown_requested_generation>p.teardown_completed_generation OR EXISTS(SELECT 1 FROM environments e WHERE e.pull_request_id=p.id AND e.status NOT IN ('torn_down'))
		OR EXISTS(SELECT 1 FROM environment_operations o WHERE o.pull_request_id=p.id AND o.kind IN ('remove_route','cleanup_artifact') AND o.state NOT IN ('succeeded','superseded'))))
		OR EXISTS(SELECT 1 FROM workspace_usage_attempts WHERE repo_id=$1 AND state IN ('reserved','running'))`, repo).Scan(&outstanding)
	if err != nil {
		return false, fmt.Errorf("check repository teardown: %w", err)
	}
	if outstanding {
		return false, nil
	}
	rows, err := l.repo.pool.Query(ctx, `SELECT id FROM pull_requests WHERE repo_id=$1 ORDER BY id`, repo)
	if err != nil {
		return false, fmt.Errorf("list repository previews: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return false, fmt.Errorf("read repository previews: %w", err)
	}
	drained := true
	for _, id := range ids {
		resources, err := l.inventory.Resources(ctx, id)
		if err != nil {
			return false, fmt.Errorf("inspect repository preview resources: %w", err)
		}
		if len(resources) > 0 {
			drained = false
			if err := l.repo.ScheduleResourceCleanup(ctx, Request{PullRequestID: id}, resources); err != nil {
				return false, err
			}
		}
	}
	return drained, nil
}
