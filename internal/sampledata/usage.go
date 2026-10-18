package sampledata

import (
	"context"
	"fmt"
	"time"

	"github.com/ajaykathwatee/previa/internal/sampledata/dbgen"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (r *Runner) reserveUsageTx(ctx context.Context, tx pgx.Tx, q *dbgen.Queries, scope dbgen.WorkerScopeRow, refresh dbgen.BaselineRefresh, now time.Time) (string, time.Duration, error) {
	if r.parts.Usage == nil {
		return "", 0, nil
	}
	key := uuid.NewString()
	reason, until, err := r.parts.Usage.AdmitBaselineTx(ctx, tx, scope.WorkspaceID.Int64, refresh.ID, key, now)
	if err != nil {
		return "", 0, err
	}
	if reason == "" {
		return key, 0, nil
	}
	wait, err := r.waitUsageTx(ctx, q, refresh.ID, reason, until, now)
	return "", wait, err
}

func (r *Runner) publicationUsageTx(ctx context.Context, tx pgx.Tx, q *dbgen.Queries, refresh dbgen.BaselineRefresh) (bool, error) {
	if r.parts.Usage == nil {
		return true, nil
	}
	scope, err := q.WorkerScope(ctx, refresh.RepoID)
	if err != nil {
		return false, fmt.Errorf("read baseline publication scope: %w", err)
	}
	reason, until, err := r.parts.Usage.PublicationBaselineTx(ctx, tx, scope.WorkspaceID.Int64, r.now())
	if err != nil {
		return false, err
	}
	if reason == "" {
		return true, nil
	}
	_, err = r.waitUsageTx(ctx, q, refresh.ID, reason, until, r.now())
	return false, err
}

func (r *Runner) waitUsageTx(ctx context.Context, q *dbgen.Queries, refresh int64, reason string, until, now time.Time) (time.Duration, error) {
	if until.IsZero() {
		until = now.Add(r.retry)
	}
	if err := q.WaitRefresh(ctx, dbgen.WaitRefreshParams{ID: refresh, WaitingReason: text(reason), AvailableAt: timestamp(until)}); err != nil {
		return 0, fmt.Errorf("wait for baseline usage allowance: %w", err)
	}
	return max(until.Sub(now), time.Second), nil
}
