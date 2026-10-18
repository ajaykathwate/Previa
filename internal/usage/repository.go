package usage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const slice = 60 * time.Second

const (
	WaitBuilds     = "workspace_builds"
	WaitMinutes    = "workspace_minutes"
	WaitDaily      = "workspace_daily"
	WaitPreviews   = "workspace_previews"
	WaitRestricted = "workspace_restricted"
	WaitAccounting = "workspace_accounting"
)

// Blocked is retryable compute work. Reads and cleanup never call this gate.
type Blocked struct {
	Reason string
	Until  time.Time
	Err    error
}

func (b *Blocked) Error() string       { return "workspace compute waits: " + b.Reason }
func (b *Blocked) FailureCode() string { return b.Reason }
func (b *Blocked) RetryAt() time.Time  { return b.Until }
func (b *Blocked) Unwrap() error       { return b.Err }

type entitlement struct {
	workspace        int64
	kind             string
	seconds          int64
	builds, previews int
	restricted       bool
}

// Store uses the workspace entitlement as the lock for all its reservations.
// Callers admitting work hold their feature's scope locks before this lock.
// Renewals and recovery acquire only usage locks, never feature scope locks.
type Store struct {
	pool                                       *pgxpool.Pool
	dailySeconds                               int64
	buildCeiling                               int
	previewLifetime, previewIdle, logRetention time.Duration
	now                                        func() time.Time
}

func (s *Store) WithPreviewPolicy(lifetime, idle, logs time.Duration) *Store {
	s.previewLifetime, s.previewIdle, s.logRetention = lifetime, idle, logs
	return s
}

func NewStore(pool *pgxpool.Pool, dailyMinutes, buildCeiling int) *Store {
	return &Store{pool: pool, dailySeconds: int64(dailyMinutes) * 60, buildCeiling: buildCeiling, now: time.Now}
}

func period(t time.Time, kind string) (time.Time, time.Time) {
	t = t.UTC()
	if kind == "month" {
		start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 1, 0)
	}
	start := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 0, 1)
}

func lock(ctx context.Context, tx pgx.Tx, workspace int64) (entitlement, error) {
	e := entitlement{workspace: workspace}
	err := tx.QueryRow(ctx, `SELECT period_kind,included_seconds,concurrent_builds,preview_slots,compute_restricted
        FROM workspace_entitlements WHERE workspace_id=$1 FOR UPDATE`, workspace).Scan(&e.kind, &e.seconds, &e.builds, &e.previews, &e.restricted)
	if err != nil {
		return e, fmt.Errorf("lock workspace entitlement: %w", err)
	}
	return e, nil
}

// AdmitPRTx reserves a physical attempt in the caller's admission transaction.
// Repeating the same key returns its existing reservation, without charging.
func (s *Store) AdmitPRTx(ctx context.Context, tx pgx.Tx, pr int64, key string, now time.Time) (string, time.Time, error) {
	var workspace, repo, generation, revision int64
	err := tx.QueryRow(ctx, `SELECT i.workspace_id,r.id,b.generation,b.revision FROM pull_requests p
        JOIN repos r ON r.id=p.repo_id JOIN installations i ON i.id=r.installation_id
        JOIN build_intents b ON b.pull_request_id=p.id WHERE p.id=$1`, pr).Scan(&workspace, &repo, &generation, &revision)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read PR usage scope: %w", err)
	}
	return s.admit(ctx, tx, workspace, repo, pr, 0, generation, revision, key, now)
}

func (s *Store) AdmitBaselineTx(ctx context.Context, tx pgx.Tx, workspace, refresh int64, key string, now time.Time) (string, time.Time, error) {
	var repo int64
	if err := tx.QueryRow(ctx, `SELECT r.id FROM baseline_refreshes b JOIN repos r ON r.id=b.repo_id
        JOIN installations i ON i.id=r.installation_id WHERE b.id=$1 AND i.workspace_id=$2`, refresh, workspace).Scan(&repo); err != nil {
		return "", time.Time{}, fmt.Errorf("read baseline usage scope: %w", err)
	}
	return s.admit(ctx, tx, workspace, repo, 0, refresh, 0, 0, key, now)
}

func (s *Store) admit(ctx context.Context, tx pgx.Tx, workspace, repo, pr, baseline, generation, revision int64, key string, now time.Time) (string, time.Time, error) {
	e, err := lock(ctx, tx, workspace)
	if err != nil {
		return "", time.Time{}, err
	}
	if err = s.reconcileTx(ctx, tx, workspace, now); err != nil {
		return "", time.Time{}, err
	}
	var exists, matches bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_usage_attempts WHERE attempt_key=$1),
        EXISTS(SELECT 1 FROM workspace_usage_attempts WHERE attempt_key=$1 AND workspace_id=$2 AND repo_id=$3
            AND coalesce(pull_request_id,0)=$4 AND coalesce(baseline_refresh_id,0)=$5 AND generation=$6 AND revision=$7)`, key, workspace, repo, pr, baseline, generation, revision).Scan(&exists, &matches); err != nil {
		return "", time.Time{}, fmt.Errorf("read attempt identity: %w", err)
	}
	if exists {
		if !matches {
			return "", time.Time{}, errors.New("physical usage attempt key conflicts with different work")
		}
		return "", time.Time{}, nil
	}
	if e.restricted {
		return WaitRestricted, time.Time{}, nil
	}
	var running int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM workspace_usage_attempts WHERE workspace_id=$1 AND state IN ('reserved','running')`, workspace).Scan(&running); err != nil {
		return "", time.Time{}, fmt.Errorf("count workspace executions: %w", err)
	}
	ceiling := e.builds
	if s.buildCeiling > 0 {
		ceiling = min(ceiling, s.buildCeiling)
	}
	if running >= ceiling {
		return WaitBuilds, time.Time{}, nil
	}
	if pr > 0 {
		used, ours, err := slots(ctx, tx, workspace, pr)
		if err != nil {
			return "", time.Time{}, err
		}
		if !ours && used >= e.previews {
			return WaitPreviews, time.Time{}, nil
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO workspace_usage_attempts(attempt_key,workspace_id,repo_id,pull_request_id,baseline_refresh_id,generation,revision,admitted_at)
        VALUES($1,$2,$3,nullif($4,0),nullif($5,0),$6,$7,$8)`, key, workspace, repo, pr, baseline, generation, revision, now)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("reserve workspace execution: %w", err)
	}
	_, err = s.reserve(ctx, tx, e, key, now)
	var b *Blocked
	if errors.As(err, &b) {
		if _, delErr := tx.Exec(ctx, `DELETE FROM workspace_usage_attempts WHERE attempt_key=$1`, key); delErr != nil {
			return "", time.Time{}, fmt.Errorf("discard blocked reservation: %w", delErr)
		}
		return b.Reason, b.Until, nil
	}
	return "", time.Time{}, err
}

// slots counts PR identities, not deployments. A replacement reuses its slot.
func slots(ctx context.Context, tx pgx.Tx, workspace, pr int64) (int, bool, error) {
	var count int
	var ours bool
	err := tx.QueryRow(ctx, `WITH held AS (
        SELECT p.id FROM pull_requests p JOIN repos r ON r.id=p.repo_id JOIN installations i ON i.id=r.installation_id
        JOIN environments e ON e.pull_request_id=p.id LEFT JOIN environment_routes er ON er.environment_id=e.id
        WHERE i.workspace_id=$1 AND (e.container_id IS NOT NULL OR e.pending_container_id IS NOT NULL OR er.state='active')
        UNION SELECT a.pull_request_id FROM workspace_usage_attempts a
        LEFT JOIN build_intents b ON b.pull_request_id=a.pull_request_id
        WHERE a.workspace_id=$1 AND a.pull_request_id IS NOT NULL AND
        (a.state IN ('reserved','running') OR (a.outcome='succeeded' AND b.revision=a.revision AND b.generation=a.generation AND b.state='running'))
    ) SELECT count(*),coalesce(bool_or(id=$2),false) FROM held`, workspace, pr).Scan(&count, &ours)
	if err != nil {
		return 0, false, fmt.Errorf("count preview slots: %w", err)
	}
	return count, ours, nil
}

func (s *Store) allowance(ctx context.Context, tx pgx.Tx, e entitlement, now time.Time) (reason string, until, deadline time.Time, err error) {
	start, end := period(now, e.kind)
	_, err = tx.Exec(ctx, `INSERT INTO workspace_usage_periods(workspace_id,starts_at,ends_at) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, e.workspace, start, end)
	if err != nil {
		return "", until, deadline, fmt.Errorf("open allowance period: %w", err)
	}
	var used, reserved int64
	var heldUntil *time.Time
	err = tx.QueryRow(ctx, `SELECT p.consumed_us,coalesce((SELECT sum(extract(epoch FROM reserved_until-starts_at)*1000000)::bigint
        FROM workspace_usage_segments WHERE workspace_id=$1 AND period_start=$2 AND ended_at IS NULL),0),
        (SELECT min(reserved_until) FROM workspace_usage_segments WHERE workspace_id=$1 AND period_start=$2 AND ended_at IS NULL)
        FROM workspace_usage_periods p WHERE workspace_id=$1 AND starts_at=$2`, e.workspace, start).Scan(&used, &reserved, &heldUntil)
	if err != nil {
		return "", until, deadline, fmt.Errorf("read allowance balance: %w", err)
	}
	available := e.seconds*1000000 - used - reserved
	if available <= 0 {
		return WaitMinutes, nextCheck(end, used, e.seconds*1000000, heldUntil), deadline, nil
	}
	day, midnight := period(now, "day")
	if s.dailySeconds > 0 {
		var daily, held int64
		var heldUntil *time.Time
		err = tx.QueryRow(ctx, `SELECT coalesce(sum(consumed_us),0)::bigint,
            coalesce(sum(CASE WHEN ended_at IS NULL THEN extract(epoch FROM reserved_until-starts_at)*1000000 ELSE 0 END),0)::bigint,
            min(reserved_until) FILTER(WHERE ended_at IS NULL)
            FROM workspace_usage_segments WHERE workspace_id=$1 AND day=$2`, e.workspace, day).Scan(&daily, &held, &heldUntil)
		if err != nil {
			return "", until, deadline, fmt.Errorf("read operator daily balance: %w", err)
		}
		remaining := s.dailySeconds*1000000 - daily - held
		if remaining <= 0 {
			return WaitDaily, nextCheck(midnight, daily, s.dailySeconds*1000000, heldUntil), deadline, nil
		}
		available = min(available, remaining)
	}
	deadline = now.Add(min(slice, time.Duration(available)*time.Microsecond))
	if end.Before(deadline) {
		deadline = end
	}
	if midnight.Before(deadline) {
		deadline = midnight
	}
	return "", until, deadline, nil
}

// A balance held by another reservation can free before the allowance resets.
// Never snooze that work for an entire month just because a slice is reserved.
func nextCheck(reset time.Time, used, limit int64, heldUntil *time.Time) time.Time {
	if used < limit && heldUntil != nil && heldUntil.Before(reset) {
		return *heldUntil
	}
	return reset
}

func (s *Store) segment(ctx context.Context, tx pgx.Tx, e entitlement, key string, sequence int, from, to time.Time) error {
	start, _ := period(from, e.kind)
	day, _ := period(from, "day")
	_, err := tx.Exec(ctx, `INSERT INTO workspace_usage_segments(attempt_key,sequence,workspace_id,period_start,day,starts_at,reserved_until)
        VALUES($1,$2,$3,$4,$5,$6,$7)`, key, sequence, e.workspace, start, day, from, to)
	if err != nil {
		return fmt.Errorf("reserve execution segment: %w", err)
	}
	return nil
}

// PublicationTx rechecks current entitlement and slots under the same lock.
// Completed, accounted execution may publish at an exact exhausted balance.
func (s *Store) PublicationTx(ctx context.Context, tx pgx.Tx, pr int64, now time.Time) (string, time.Time, error) {
	var workspace int64
	if err := tx.QueryRow(ctx, `SELECT i.workspace_id FROM pull_requests p JOIN repos r ON r.id=p.repo_id
        JOIN installations i ON i.id=r.installation_id WHERE p.id=$1`, pr).Scan(&workspace); err != nil {
		return "", time.Time{}, fmt.Errorf("read publication workspace: %w", err)
	}
	e, err := lock(ctx, tx, workspace)
	if err != nil {
		return "", time.Time{}, err
	}
	if e.restricted {
		return WaitRestricted, time.Time{}, nil
	}
	count, _, err := slots(ctx, tx, workspace, pr)
	if err != nil {
		return "", time.Time{}, err
	}
	if count > e.previews {
		return WaitPreviews, time.Time{}, nil
	}
	return s.publicationAllowance(ctx, tx, e, now)
}

func (s *Store) PublicationBaselineTx(ctx context.Context, tx pgx.Tx, workspace int64, now time.Time) (string, time.Time, error) {
	e, err := lock(ctx, tx, workspace)
	if err != nil {
		return "", time.Time{}, err
	}
	if e.restricted {
		return WaitRestricted, time.Time{}, nil
	}
	return s.publicationAllowance(ctx, tx, e, now)
}

func (s *Store) publicationAllowance(ctx context.Context, tx pgx.Tx, e entitlement, now time.Time) (string, time.Time, error) {
	start, end := period(now, e.kind)
	var used int64
	if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT consumed_us FROM workspace_usage_periods WHERE workspace_id=$1 AND starts_at=$2),0)+
        coalesce((SELECT sum(extract(epoch FROM reserved_until-starts_at)*1000000)::bigint FROM workspace_usage_segments WHERE workspace_id=$1 AND period_start=$2 AND ended_at IS NULL),0)`, e.workspace, start).Scan(&used); err != nil {
		return "", time.Time{}, fmt.Errorf("recheck publication allowance: %w", err)
	}
	if used > e.seconds*1000000 {
		return WaitMinutes, end, nil
	}
	if s.dailySeconds > 0 {
		day, midnight := period(now, "day")
		var daily int64
		if err := tx.QueryRow(ctx, `SELECT coalesce(sum(CASE WHEN ended_at IS NULL THEN extract(epoch FROM reserved_until-starts_at)*1000000 ELSE consumed_us END),0)::bigint FROM workspace_usage_segments WHERE workspace_id=$1 AND day=$2`, e.workspace, day).Scan(&daily); err != nil {
			return "", time.Time{}, fmt.Errorf("recheck publication daily ceiling: %w", err)
		}
		if daily > s.dailySeconds*1000000 {
			return WaitDaily, midnight, nil
		}
	}
	return "", time.Time{}, nil
}

func (s *Store) transaction(ctx context.Context, fn func(pgx.Tx) error) error {
	return database.InTransaction(ctx, s.pool, fn)
}
