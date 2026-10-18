package usage

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestExecutionCountsOnceExcludesWaitingAndSeparatesRetries(t *testing.T) {
	f := newFixture(t)
	pr := f.pr(t, 1)
	key := f.reserve(t, pr)
	f.now = f.now.Add(20 * time.Second) // an admission delay is not execution
	f.start(t, key)
	f.now = f.now.Add(12500 * time.Millisecond)
	f.finish(t, key, "failed")
	f.finish(t, key, "failed")
	if got := f.consumed(t); got != 12500000 {
		t.Fatal("execution should count once, without admission wait", got)
	}
	// Replaying the physical key cannot create another segment or execute it.
	if reason, _ := f.admit(t, pr, key); reason != "" {
		t.Fatal(reason)
	}
	if _, err := f.store.start(t.Context(), key); err == nil {
		t.Fatal("replayed physical attempt executed")
	}
	second := f.reserve(t, pr)
	f.start(t, second)
	f.now = f.now.Add(7 * time.Second)
	f.finish(t, second, "succeeded")
	if got := f.consumed(t); got != 19500000 {
		t.Fatal("a physical retry must count its own execution", got)
	}
	f.assertReconciles(t)
}

func TestReservationsPreventConcurrentOverspend(t *testing.T) {
	f := newFixture(t)
	f.exec(t, `UPDATE workspace_entitlements SET included_seconds=60,concurrent_builds=10,preview_slots=10 WHERE workspace_id=$1`, f.workspace)
	prs := []int64{f.pr(t, 1), f.pr(t, 2), f.pr(t, 3), f.pr(t, 4)}
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for _, pr := range prs {
		wg.Go(func() {
			err := f.store.transaction(t.Context(), func(tx pgx.Tx) error {
				reason, _, err := f.store.AdmitPRTx(t.Context(), tx, pr, uuid.NewString(), f.now)
				if err == nil && reason == "" {
					admitted.Add(1)
				}
				return err
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if admitted.Load() != 1 {
		t.Fatal("reserved allowance overspent", admitted.Load())
	}
	if reason, until := f.admit(t, prs[0], uuid.NewString()); reason != WaitMinutes || until.After(f.now.Add(time.Minute)) {
		t.Fatal("reserved time must be checked before the monthly reset", reason, until)
	}
	var seconds int64
	if err := f.pool.QueryRow(t.Context(), `SELECT sum(extract(epoch FROM reserved_until-starts_at))::bigint FROM workspace_usage_segments WHERE ended_at IS NULL`).Scan(&seconds); err != nil || seconds != 60 {
		t.Fatal(seconds, err)
	}
}

func TestConcurrentBuildsAndPreviewSlotsAreSharedWithBaselines(t *testing.T) {
	f := newFixture(t)
	first, second, third := f.pr(t, 1), f.pr(t, 2), f.pr(t, 3)
	one := f.reserve(t, first)
	two := f.reserve(t, second)
	if reason, _ := f.admit(t, third, uuid.NewString()); reason != WaitBuilds {
		t.Fatal(reason)
	}
	refresh := f.id(t, `INSERT INTO baseline_refreshes(repo_id,sequence,trigger) VALUES($1,1,'button') RETURNING id`, f.repo)
	baselineKey := uuid.NewString()
	var reason string
	err := f.store.transaction(t.Context(), func(tx pgx.Tx) error {
		var err error
		reason, _, err = f.store.AdmitBaselineTx(t.Context(), tx, f.workspace, refresh, baselineKey, f.now)
		return err
	})
	if err != nil || reason != WaitBuilds {
		t.Fatal(reason, err)
	}
	f.finish(t, two, "failed")
	err = f.store.transaction(t.Context(), func(tx pgx.Tx) error {
		var err error
		reason, _, err = f.store.AdmitBaselineTx(t.Context(), tx, f.workspace, refresh, baselineKey, f.now)
		return err
	})
	if err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	f.start(t, baselineKey)
	f.now = f.now.Add(3 * time.Second)
	f.finish(t, baselineKey, "succeeded")
	if f.consumed(t) != 3000000 {
		t.Fatal("baseline execution was not counted")
	}
	f.finish(t, one, "failed")
	f.exec(t, `UPDATE workspace_entitlements SET preview_slots=1 WHERE workspace_id=$1`, f.workspace)
	replacement := f.reserve(t, first)
	if reason, _ := f.admit(t, second, uuid.NewString()); reason != WaitPreviews {
		t.Fatal("new PR should need a slot", reason)
	}
	// A new physical attempt for the same PR reuses the reserved identity.
	f.reserve(t, first)
	f.finish(t, replacement, "failed")
	f.assertReconciles(t)
}

func TestCrashRecoveryIsBoundedIdempotentAndProvisional(t *testing.T) {
	f := newFixture(t)
	key := f.reserve(t, f.pr(t, 1))
	f.start(t, key)
	f.now = f.now.Add(20 * time.Second)
	if _, err := f.store.renew(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(2 * time.Hour)
	if err := f.store.transaction(t.Context(), func(tx pgx.Tx) error {
		if _, err := lock(t.Context(), tx, f.workspace); err != nil {
			return err
		}
		return f.store.reconcileTx(t.Context(), tx, f.workspace, f.now)
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.consumed(t); got != 80000000 {
		t.Fatal("crash must stop at last deadline, not recovery time", got)
	}
	f.finish(t, key, "failed")
	if got := f.consumed(t); got != 80000000 {
		t.Fatal("late finish double counted crash", got)
	}
	var provisional bool
	if err := f.pool.QueryRow(t.Context(), `SELECT provisional FROM workspace_usage_attempts WHERE attempt_key=$1`, key).Scan(&provisional); err != nil || !provisional {
		t.Fatal(provisional, err)
	}
	// An admitted attempt that never executed costs nothing on recovery.
	queued := f.reserve(t, f.pr(t, 2))
	f.now = f.now.Add(time.Minute)
	if err := f.store.transaction(t.Context(), func(tx pgx.Tx) error {
		if _, err := lock(t.Context(), tx, f.workspace); err != nil {
			return err
		}
		return f.store.reconcileTx(t.Context(), tx, f.workspace, f.now)
	}); err != nil {
		t.Fatal(err)
	}
	f.finish(t, queued, "failed")
	if f.consumed(t) != 80000000 {
		t.Fatal("unstarted work consumed usage")
	}
	f.assertReconciles(t)
}

func TestSegmentsSplitAtUTCMonthAndDayBoundaries(t *testing.T) {
	f := newFixture(t)
	f.now = time.Date(2026, 10, 31, 23, 59, 50, 0, time.UTC)
	key := f.reserve(t, f.pr(t, 1))
	f.start(t, key)
	f.now = f.now.Add(25 * time.Second)
	f.finish(t, key, "succeeded")
	rows, err := f.pool.Query(t.Context(), `SELECT period_start,day,sum(consumed_us)::bigint FROM workspace_usage_segments WHERE consumed_us>0 GROUP BY period_start,day ORDER BY day`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var parts []int64
	for rows.Next() {
		var start, day time.Time
		var consumed int64
		if err = rows.Scan(&start, &day, &consumed); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, consumed)
		if start.Month() != day.Month() {
			t.Fatal("wrong allowance period", start, day)
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || parts[0] != 10000000 || parts[1] != 15000000 {
		t.Fatal("execution crossed boundary without splitting", parts)
	}
	f.assertReconciles(t)
}

func TestAllowanceExhaustionDailySafetyAndReset(t *testing.T) {
	f := newFixture(t)
	pr := f.pr(t, 1)
	f.exec(t, `UPDATE workspace_entitlements SET included_seconds=10,period_kind='day' WHERE workspace_id=$1`, f.workspace)
	key := f.reserve(t, pr)
	f.start(t, key)
	f.now = f.now.Add(10 * time.Second)
	f.finish(t, key, "failed")
	if reason, until := f.admit(t, pr, uuid.NewString()); reason != WaitMinutes || until.Hour() != 0 || until.Day() != 19 {
		t.Fatal(reason, until)
	}
	f.now = time.Date(2026, 10, 19, 0, 0, 1, 0, time.UTC)
	key = f.reserve(t, pr)
	f.finish(t, key, "failed")
	f.exec(t, `UPDATE workspace_entitlements SET included_seconds=600 WHERE workspace_id=$1`, f.workspace)
	f.store.dailySeconds = 60
	key = f.reserve(t, pr)
	f.start(t, key)
	f.now = f.now.Add(time.Minute)
	f.finish(t, key, "succeeded")
	if reason, until := f.admit(t, pr, uuid.NewString()); reason != WaitDaily || until.Day() != 20 {
		t.Fatal(reason, until)
	}
	f.assertReconciles(t)
}

func TestPublicationRechecksLoweredEntitlementAndPreservesExistingPreview(t *testing.T) {
	f := newFixture(t)
	pr := f.pr(t, 1)
	key := f.reserve(t, pr)
	f.start(t, key)
	f.now = f.now.Add(20 * time.Second)
	f.finish(t, key, "succeeded")
	f.exec(t, `UPDATE workspace_entitlements SET included_seconds=10 WHERE workspace_id=$1`, f.workspace)
	var reason string
	err := f.store.transaction(t.Context(), func(tx pgx.Tx) error {
		var err error
		reason, _, err = f.store.PublicationTx(t.Context(), tx, pr, f.now)
		return err
	})
	if err != nil || reason != WaitMinutes {
		t.Fatal(reason, err)
	}
	f.exec(t, `UPDATE workspace_entitlements SET compute_restricted=true WHERE workspace_id=$1`, f.workspace)
	err = f.store.transaction(t.Context(), func(tx pgx.Tx) error {
		var err error
		reason, _, err = f.store.PublicationTx(t.Context(), tx, pr, f.now)
		return err
	})
	if err != nil || reason != WaitRestricted {
		t.Fatal(reason, err)
	}
}

type canceledRuntime struct{}

func (canceledRuntime) Error() string       { return "runtime canceled" }
func (canceledRuntime) FailureCode() string { return "cancelled" }

func TestRunCancelsAtReservedDeadline(t *testing.T) {
	f := newFixture(t)
	f.store.now = time.Now
	f.now = time.Now().UTC()
	pr := f.pr(t, 1)
	f.exec(t, `UPDATE workspace_entitlements SET included_seconds=1 WHERE workspace_id=$1`, f.workspace)
	key := f.reserve(t, pr)
	started := time.Now()
	err := f.store.Run(t.Context(), key, func(ctx context.Context) error { <-ctx.Done(); return canceledRuntime{} })
	var limit *Blocked
	if !errors.As(err, &limit) || time.Since(started) > 2*time.Second {
		t.Fatal("execution did not stop at hard allowance", time.Since(started), err)
	}
	var classified interface{ FailureCode() string }
	if !errors.As(err, &classified) || classified.FailureCode() == "cancelled" {
		t.Fatal("quota wait hidden by runtime cancellation", err)
	}
	got := f.consumed(t)
	if got < 900000 || got > 1000000 {
		t.Fatal("bounded execution accounting", got)
	}
	f.assertReconciles(t)
}

func TestParentCancellationCannotBecomeSuccessfulExecution(t *testing.T) {
	f := newFixture(t)
	f.store.now = time.Now
	f.now = time.Now().UTC()
	key := f.reserve(t, f.pr(t, 1))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := f.store.Run(ctx, key, func(ctx context.Context) error {
		timer := time.AfterFunc(10*time.Millisecond, cancel)
		defer timer.Stop()
		<-ctx.Done()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("parent cancellation lost", err)
	}
	var outcome string
	if err = f.pool.QueryRow(t.Context(), `SELECT outcome FROM workspace_usage_attempts WHERE attempt_key=$1`, key).Scan(&outcome); err != nil || outcome != "interrupted" {
		t.Fatal("canceled execution was not recorded as interrupted", outcome, err)
	}
	if f.consumed(t) <= 0 {
		t.Fatal("canceled execution was not counted")
	}
	f.assertReconciles(t)
}

func TestConcurrentReplayCannotFinishTheRunningAttempt(t *testing.T) {
	f := newFixture(t)
	f.store.now = time.Now
	f.now = time.Now().UTC()
	key := f.reserve(t, f.pr(t, 1))
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- f.store.Run(t.Context(), key, func(context.Context) error {
			close(started)
			<-release
			return nil
		})
	}()
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
		f.assertReconciles(t)
	}()
	<-started
	err := f.store.Run(t.Context(), key, func(context.Context) error {
		t.Error("replayed physical attempt executed twice")
		return nil
	})
	if err == nil {
		t.Fatal("concurrent replay succeeded")
	}
	var state string
	if err = f.pool.QueryRow(t.Context(), `SELECT state FROM workspace_usage_attempts WHERE attempt_key=$1`, key).Scan(&state); err != nil || state != "running" {
		t.Fatal("replay settled the original execution", state, err)
	}
	var open int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM workspace_usage_segments WHERE attempt_key=$1 AND ended_at IS NULL`, key).Scan(&open); err != nil || open == 0 {
		t.Fatal("replay released the original reservation", open, err)
	}
}

func TestDatabaseStallCannotExtendExecutionReservation(t *testing.T) {
	f := newFixture(t)
	f.store.now = time.Now
	f.now = time.Now().UTC()
	f.exec(t, `UPDATE workspace_entitlements SET included_seconds=1 WHERE workspace_id=$1`, f.workspace)
	key := f.reserve(t, f.pr(t, 1))
	started := make(chan struct{})
	stopped := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- f.store.Run(t.Context(), key, func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			close(stopped)
			return ctx.Err()
		})
	}()
	<-started
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err = lock(t.Context(), tx, f.workspace); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("database lock extended the execution reservation")
	}
	if err = tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-result; err == nil {
		t.Fatal("stalled accounting silently succeeded")
	}
	f.assertReconciles(t)
}

func TestExecutionRechecksEntitlementsAfterAdmissionDelay(t *testing.T) {
	f := newFixture(t)
	key := f.reserve(t, f.pr(t, 1))
	f.exec(t, `UPDATE workspace_entitlements SET compute_restricted=true WHERE workspace_id=$1`, f.workspace)
	if _, err := f.store.start(t.Context(), key); err == nil {
		t.Fatal("restricted workspace executed an old reservation")
	}
	f.finish(t, key, "limit")
	if f.consumed(t) != 0 {
		t.Fatal("rejected execution consumed time")
	}
}

func TestPhysicalKeyCannotBeReusedForDifferentWork(t *testing.T) {
	f := newFixture(t)
	key := f.reserve(t, f.pr(t, 1))
	other := f.pr(t, 2)
	err := f.store.transaction(t.Context(), func(tx pgx.Tx) error { _, _, err := f.store.AdmitPRTx(t.Context(), tx, other, key, f.now); return err })
	if err == nil {
		t.Fatal("physical key was accepted for another PR")
	}
}

func TestPreviewSlotSurvivesSleepAndIsReleasedAfterTeardown(t *testing.T) {
	f := newFixture(t)
	first, second := f.pr(t, 1), f.pr(t, 2)
	f.exec(t, `UPDATE workspace_entitlements SET preview_slots=1 WHERE workspace_id=$1`, f.workspace)
	f.exec(t, `INSERT INTO environments(pull_request_id,subdomain,status,container_id,image_tag,expires_at) VALUES($1,'usage-retained','running','serving','image',now()+interval '1 day')`, first)
	if reason, _ := f.admit(t, second, uuid.NewString()); reason != WaitPreviews {
		t.Fatal("retained preview did not keep its slot", reason)
	}
	// A same-PR replacement may execute while the serving preview remains.
	key := f.reserve(t, first)
	f.finish(t, key, "failed")
	f.exec(t, `UPDATE environments SET container_id=NULL,image_tag=NULL,status='torn_down' WHERE pull_request_id=$1`, first)
	f.reserve(t, second)
}
