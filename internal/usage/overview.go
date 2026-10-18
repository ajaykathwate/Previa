package usage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrUnavailable = errors.New("usage is unavailable to this workspace member")

type Overview struct {
	PreviewLifetime, PreviewIdle, LogRetention                 *int64
	WorkspaceID                                                int64
	Name, Kind                                                 string
	Manager                                                    bool
	Start, End, Measured, Cutover, OperatorChecked             time.Time
	Included, Consumed, Reserved, Daily                        int64 // microseconds for consumed/reserved; seconds for limits
	Builds, BuildLimit, Slots, SlotLimit, Provisional, Pending int
	Repositories                                               []RepositoryUsage
	Blocked                                                    []BlockedWork
}
type RepositoryUsage struct {
	ID                    int64
	Name                  string
	Consumed, Reserved    int64
	Attempts, Provisional int
}
type BlockedWork struct {
	RepoID                   int64
	Repository, Reason, Kind string
	Number                   int
	Until                    time.Time
}

// Overview verifies membership on every request. Developers receive only
// their granted repositories' technical usage, without workspace totals.
func (s *Store) Overview(ctx context.Context, user, workspace int64) (Overview, error) {
	var o Overview
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return o, fmt.Errorf("begin usage snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var role string
	err = tx.QueryRow(ctx, `SELECT w.name,m.role,clock_timestamp() FROM workspaces w JOIN workspace_memberships m ON m.workspace_id=w.id
        WHERE w.id=$1 AND m.user_id=$2 AND m.state='active' AND w.state='active' AND m.role IN ('owner','admin','developer')`, workspace, user).Scan(&o.Name, &role, &o.Measured)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, ErrUnavailable
	}
	if err != nil {
		return o, fmt.Errorf("authorize workspace usage: %w", err)
	}
	o.WorkspaceID, o.Manager = workspace, role == "owner" || role == "admin"
	err = tx.QueryRow(ctx, `SELECT e.period_kind,e.included_seconds,e.concurrent_builds,e.preview_slots,p.cutover_at,p.operator_daily_seconds
        FROM workspace_entitlements e CROSS JOIN free_plan_policy p WHERE e.workspace_id=$1`, workspace).
		Scan(&o.Kind, &o.Included, &o.BuildLimit, &o.SlotLimit, &o.Cutover, &o.Daily)
	if err != nil {
		return o, fmt.Errorf("read workspace plan: %w", err)
	}
	// A separate nullable scan keeps the displayed effective operator cap
	// identical to the worker's configured cap.
	var operatorBuilds int
	var checked *time.Time
	if err = tx.QueryRow(ctx, `SELECT operator_concurrent_builds,operator_checked_at,preview_lifetime_seconds,preview_idle_seconds,log_retention_seconds FROM free_plan_policy`).Scan(&operatorBuilds, &checked, &o.PreviewLifetime, &o.PreviewIdle, &o.LogRetention); err != nil {
		return o, fmt.Errorf("read execution safety policy: %w", err)
	}
	if checked != nil {
		o.OperatorChecked = *checked
	}
	if operatorBuilds > 0 {
		o.BuildLimit = min(o.BuildLimit, operatorBuilds)
	}
	o.Start, o.End = period(o.Measured, o.Kind)
	if o.Manager {
		err = tx.QueryRow(ctx, `SELECT coalesce((SELECT consumed_us FROM workspace_usage_periods WHERE workspace_id=$1 AND starts_at=$2),0),
            coalesce((SELECT sum(extract(epoch FROM reserved_until-starts_at)*1000000)::bigint FROM workspace_usage_segments WHERE workspace_id=$1 AND period_start=$2 AND ended_at IS NULL),0),
            (SELECT count(*) FROM workspace_usage_attempts WHERE workspace_id=$1 AND state IN ('reserved','running')),
            (SELECT count(DISTINCT a.attempt_key) FROM workspace_usage_attempts a JOIN workspace_usage_segments s ON s.attempt_key=a.attempt_key WHERE a.workspace_id=$1 AND a.provisional AND s.period_start=$2),
            (SELECT count(*) FROM workspace_usage_attempts a WHERE workspace_id=$1 AND state IN ('reserved','running') AND NOT EXISTS(
                SELECT 1 FROM workspace_usage_segments s WHERE s.attempt_key=a.attempt_key AND s.ended_at IS NULL AND s.reserved_until>$3))`, workspace, o.Start, o.Measured).
			Scan(&o.Consumed, &o.Reserved, &o.Builds, &o.Provisional, &o.Pending)
		if err != nil {
			return o, fmt.Errorf("read workspace usage totals: %w", err)
		}
		o.Slots, _, err = slots(ctx, tx, workspace, 0)
		if err != nil {
			return o, err
		}
	}
	if o.Repositories, err = repositoryUsage(ctx, tx, user, workspace, o.Start); err != nil {
		return o, err
	}
	if o.Blocked, err = blockedWork(ctx, tx, user, workspace); err != nil {
		return o, err
	}
	if err = tx.Commit(ctx); err != nil {
		return o, fmt.Errorf("finish usage snapshot: %w", err)
	}
	return o, nil
}

func repositoryUsage(ctx context.Context, tx pgx.Tx, user, workspace int64, start time.Time) ([]RepositoryUsage, error) {
	rows, err := tx.Query(ctx, `SELECT r.id,r.full_name,
        coalesce(sum(s.consumed_us),0)::bigint,
        coalesce(sum(CASE WHEN s.ended_at IS NULL THEN extract(epoch FROM s.reserved_until-s.starts_at)*1000000 ELSE 0 END),0)::bigint,
        count(DISTINCT a.attempt_key) FILTER(WHERE s.sequence IS NOT NULL),count(DISTINCT a.attempt_key) FILTER(WHERE a.provisional AND s.sequence IS NOT NULL)
        FROM repos r JOIN installations i ON i.id=r.installation_id
        LEFT JOIN workspace_usage_attempts a ON a.repo_id=r.id AND a.workspace_id=$1
        LEFT JOIN workspace_usage_segments s ON s.attempt_key=a.attempt_key AND s.period_start=$3
        WHERE i.workspace_id=$1 AND workspace_repository_access(r.id,$2,'operate')
        GROUP BY r.id ORDER BY r.full_name`, workspace, user, start)
	if err != nil {
		return nil, fmt.Errorf("read repository usage: %w", err)
	}
	defer rows.Close()
	var out []RepositoryUsage
	for rows.Next() {
		var r RepositoryUsage
		if err = rows.Scan(&r.ID, &r.Name, &r.Consumed, &r.Reserved, &r.Attempts, &r.Provisional); err != nil {
			return nil, fmt.Errorf("scan repository usage: %w", err)
		}
		out = append(out, r)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("read repository usage rows: %w", err)
	}
	return out, nil
}

func blockedWork(ctx context.Context, tx pgx.Tx, user, workspace int64) ([]BlockedWork, error) {
	rows, err := tx.Query(ctx, `WITH waiting AS (
        SELECT r.id,r.full_name,b.waiting_reason AS reason,'Pull request' AS kind,p.number,coalesce(b.usage_wait_until,'epoch') AS until
        FROM build_intents b JOIN pull_requests p ON p.id=b.pull_request_id JOIN repos r ON r.id=p.repo_id
        JOIN installations i ON i.id=r.installation_id WHERE i.workspace_id=$1 AND b.waiting_revision=b.revision
        AND b.waiting_reason IS NOT NULL AND b.state IN ('pending','queued','running') AND workspace_repository_access(r.id,$2,'operate')
        UNION ALL SELECT r.id,r.full_name,b.waiting_reason,'Baseline',0,b.available_at FROM baseline_refreshes b
        JOIN repos r ON r.id=b.repo_id JOIN installations i ON i.id=r.installation_id WHERE i.workspace_id=$1 AND b.state='pending'
        AND b.waiting_reason IS NOT NULL AND workspace_repository_access(r.id,$2,'operate')
        UNION ALL SELECT r.id,r.full_name,op.last_error_code,'Publication',p.number,op.available_at FROM environment_operations op
        JOIN pull_requests p ON p.id=op.pull_request_id JOIN repos r ON r.id=p.repo_id JOIN installations i ON i.id=r.installation_id
        WHERE i.workspace_id=$1 AND op.kind='activate_route' AND op.state='retryable' AND op.last_error_code LIKE 'workspace_%'
        AND workspace_repository_access(r.id,$2,'operate')
    ) SELECT * FROM waiting ORDER BY full_name,kind,number LIMIT 50`, workspace, user)
	if err != nil {
		return nil, fmt.Errorf("read blocked compute: %w", err)
	}
	defer rows.Close()
	var out []BlockedWork
	for rows.Next() {
		var b BlockedWork
		if err = rows.Scan(&b.RepoID, &b.Repository, &b.Reason, &b.Kind, &b.Number, &b.Until); err != nil {
			return nil, fmt.Errorf("scan blocked work: %w", err)
		}
		out = append(out, b)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("read blocked work rows: %w", err)
	}
	return out, nil
}
