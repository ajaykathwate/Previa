package sampledata

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type Lifecycle struct {
	store     *Store
	storage   *Storage
	leftovers Leftovers
}

func NewLifecycle(store *Store, storage *Storage, leftovers Leftovers) *Lifecycle {
	return &Lifecycle{store: store, storage: storage, leftovers: leftovers}
}

func (l *Lifecycle) FenceRepositoryTx(ctx context.Context, tx pgx.Tx, repo int64) error {
	for _, q := range []string{
		`UPDATE repository_sampledata_settings SET enabled=false,keep_data=false,use_baseline=false,auto_refresh=false,revision=revision+1 WHERE repo_id=$1`,
		`UPDATE baseline_refreshes SET state='cancelled',finished_at=clock_timestamp(),log_open=false WHERE repo_id=$1 AND state IN ('pending','running')`,
	} {
		if _, err := tx.Exec(ctx, q, repo); err != nil {
			return fmt.Errorf("fence repository test data: %w", err)
		}
	}
	return nil
}

func (l *Lifecycle) RepositoryDrained(ctx context.Context, repo int64) (bool, error) {
	if l.leftovers == nil {
		return false, nil
	}
	rows, err := l.store.pool.Query(ctx, `SELECT id FROM baseline_refreshes WHERE repo_id=$1 ORDER BY id`, repo)
	if err != nil {
		return false, fmt.Errorf("list baseline cleanup targets: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return false, fmt.Errorf("read baseline cleanup targets: %w", err)
	}
	for _, id := range ids {
		if err = l.leftovers.RemoveBaseline(ctx, id); err != nil {
			return false, fmt.Errorf("remove disconnected baseline resources: %w", err)
		}
		if err = l.leftovers.RemoveBaselineImages(ctx, id); err != nil {
			return false, fmt.Errorf("remove disconnected baseline images: %w", err)
		}
	}
	if l.storage != nil {
		rows, err := l.store.pool.Query(ctx, `SELECT storage_key FROM repository_baselines WHERE repo_id=$1`, repo)
		if err != nil {
			return false, fmt.Errorf("list disconnected baseline files: %w", err)
		}
		keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return false, fmt.Errorf("read disconnected baseline files: %w", err)
		}
		for _, key := range keys {
			if err = l.storage.Remove(key); err != nil {
				return false, err
			}
		}
		if err = l.storage.RemoveRepository(repo); err != nil {
			return false, err
		}
	}
	return true, nil
}
