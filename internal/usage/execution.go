package usage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type execution struct {
	key       string
	workspace int64
	state     string
	deadline  time.Time
}

func (s *Store) execution(ctx context.Context, tx pgx.Tx, key string) (execution, entitlement, error) {
	a := execution{key: key}
	if err := tx.QueryRow(ctx, `SELECT workspace_id FROM workspace_usage_attempts WHERE attempt_key=$1`, key).Scan(&a.workspace); err != nil {
		return a, entitlement{}, fmt.Errorf("read usage attempt: %w", err)
	}
	e, err := lock(ctx, tx, a.workspace)
	if err != nil {
		return a, e, err
	}
	err = tx.QueryRow(ctx, `SELECT a.state,coalesce(max(s.reserved_until),a.admitted_at) FROM workspace_usage_attempts a
        LEFT JOIN workspace_usage_segments s ON s.attempt_key=a.attempt_key AND s.ended_at IS NULL
        WHERE a.attempt_key=$1 GROUP BY a.attempt_key`, key).Scan(&a.state, &a.deadline)
	// Aggregate queries cannot lock rows. The entitlement lock already
	// serializes every mutation of these attempt/segment rows.
	if err != nil {
		return a, e, fmt.Errorf("read execution reservation: %w", err)
	}
	return a, e, nil
}

// reserve allocates at most 60 seconds in total. If a UTC boundary falls
// inside it, two segments reserve the balances on their respective sides.
func (s *Store) reserve(ctx context.Context, tx pgx.Tx, e entitlement, key string, now time.Time) (time.Time, error) {
	var sequence int
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(sequence),0) FROM workspace_usage_segments WHERE attempt_key=$1`, key).Scan(&sequence); err != nil {
		return time.Time{}, fmt.Errorf("read segment sequence: %w", err)
	}
	horizon := now.Add(slice)
	from := now
	for range 2 {
		reason, until, to, err := s.allowance(ctx, tx, e, from)
		if err != nil {
			return time.Time{}, err
		}
		if reason != "" {
			if from.After(now) {
				return from, nil
			}
			return time.Time{}, &Blocked{Reason: reason, Until: until}
		}
		if horizon.Before(to) {
			to = horizon
		}
		sequence++
		if err = s.segment(ctx, tx, e, key, sequence, from, to); err != nil {
			return time.Time{}, err
		}
		_, midnight := period(from, "day")
		if !to.Equal(midnight) || !to.Before(horizon) {
			return to, nil
		}
		from = to
	}
	return from, nil
}

// settle closes only open segments. The guarded UPDATE and entitlement lock
// make retries safe even if the caller lost the commit acknowledgement.
func settle(ctx context.Context, tx pgx.Tx, key string, at time.Time, executed bool) error {
	rows, err := tx.Query(ctx, `SELECT sequence,workspace_id,period_start,starts_at,reserved_until FROM workspace_usage_segments
        WHERE attempt_key=$1 AND ended_at IS NULL ORDER BY sequence`, key)
	if err != nil {
		return fmt.Errorf("read unsettled execution segments: %w", err)
	}
	type segment struct {
		seq              int
		workspace        int64
		period, from, to time.Time
	}
	var segments []segment
	for rows.Next() {
		var v segment
		if err = rows.Scan(&v.seq, &v.workspace, &v.period, &v.from, &v.to); err != nil {
			rows.Close()
			return fmt.Errorf("scan execution segment: %w", err)
		}
		segments = append(segments, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("read execution segments: %w", err)
	}
	for _, v := range segments {
		end := at
		if end.After(v.to) {
			end = v.to
		}
		if end.Before(v.from) || !executed {
			end = v.from
		}
		consumed := end.Sub(v.from).Microseconds()
		tag, err := tx.Exec(ctx, `UPDATE workspace_usage_segments SET ended_at=$3,consumed_us=$4 WHERE attempt_key=$1 AND sequence=$2 AND ended_at IS NULL`, key, v.seq, end, consumed)
		if err != nil {
			return fmt.Errorf("settle execution segment: %w", err)
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		if _, err = tx.Exec(ctx, `UPDATE workspace_usage_periods SET consumed_us=consumed_us+$3,measured_at=clock_timestamp() WHERE workspace_id=$1 AND starts_at=$2`, v.workspace, v.period, consumed); err != nil {
			return fmt.Errorf("settle allowance balance: %w", err)
		}
	}
	return nil
}

func (s *Store) start(ctx context.Context, key string) (time.Time, error) {
	var deadline time.Time
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		a, e, err := s.execution(ctx, tx, key)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		if a.state != "reserved" {
			return errors.New("usage attempt was already executed; a physical retry needs a new key")
		}
		if !now.Before(a.deadline) {
			return &Blocked{Reason: WaitMinutes}
		}
		var running int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM workspace_usage_attempts WHERE workspace_id=$1 AND state IN ('reserved','running')`, e.workspace).Scan(&running); err != nil {
			return fmt.Errorf("recheck running build limit: %w", err)
		}
		ceiling := e.builds
		if s.buildCeiling > 0 {
			ceiling = min(ceiling, s.buildCeiling)
		}
		if running > ceiling {
			return &Blocked{Reason: WaitBuilds}
		}
		var pr int64
		if err = tx.QueryRow(ctx, `SELECT coalesce(pull_request_id,0) FROM workspace_usage_attempts WHERE attempt_key=$1`, key).Scan(&pr); err != nil {
			return fmt.Errorf("read reserved preview identity: %w", err)
		}
		if pr > 0 {
			count, _, err := slots(ctx, tx, e.workspace, pr)
			if err != nil {
				return err
			}
			if count > e.previews {
				return &Blocked{Reason: WaitPreviews}
			}
		}
		// Admission-to-execution delay is not execution. Release the admission
		// reservation and recheck the actual balances immediately before work.
		if err = settle(ctx, tx, key, now, false); err != nil {
			return err
		}
		if e.restricted {
			return &Blocked{Reason: WaitRestricted}
		}
		deadline, err = s.reserve(ctx, tx, e, key, now)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE workspace_usage_attempts SET state='running',started_at=$2 WHERE attempt_key=$1`, key, now)
		if err != nil {
			return fmt.Errorf("start metered execution: %w", err)
		}
		return nil
	})
	return deadline, err
}

func (s *Store) renew(ctx context.Context, key string) (time.Time, error) {
	var deadline time.Time
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		a, e, err := s.execution(ctx, tx, key)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		if a.state != "running" || !now.Before(a.deadline) {
			return &Blocked{Reason: WaitMinutes}
		}
		if err = settle(ctx, tx, key, now, true); err != nil {
			return err
		}
		if e.restricted {
			return &Blocked{Reason: WaitRestricted}
		}
		deadline, err = s.reserve(ctx, tx, e, key, now)
		return err
	})
	return deadline, err
}

func (s *Store) finish(ctx context.Context, key, outcome string) error {
	return s.finishState(ctx, key, outcome, false)
}

func (s *Store) finishState(ctx context.Context, key, outcome string, onlyReserved bool) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		a, _, err := s.execution(ctx, tx, key)
		if err != nil {
			return err
		}
		if a.state == "finished" || a.state == "reconciled" || (onlyReserved && a.state != "reserved") {
			return nil
		}
		now := s.now().UTC()
		if err = settle(ctx, tx, key, now, a.state == "running"); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE workspace_usage_attempts SET state='finished',outcome=$2,finished_at=$3 WHERE attempt_key=$1`, key, outcome, now)
		if err != nil {
			return fmt.Errorf("finish metered execution: %w", err)
		}
		return nil
	})
}

// Run provides an execution context with a separately armed reservation timer.
// A stuck/failed renewal cannot keep work alive past its last committed lease.
// The runtime must honor cancellation (as its existing contract requires).
func (s *Store) Run(ctx context.Context, key string, work func(context.Context) error) error {
	deadline, err := s.start(ctx, key)
	if err != nil {
		finishCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer stop()
		// A replay may refer to another invocation's running attempt. A failed
		// start may release an unstarted hold, never settle that execution.
		return errors.Join(err, s.finishState(finishCtx, key, "limit", true))
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	timer := time.AfterFunc(max(time.Until(deadline), 0), func() { cancel(&Blocked{Reason: WaitMinutes}) })
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			delay := max(time.Until(deadline)/3, time.Millisecond)
			tick := time.NewTimer(delay)
			select {
			case <-done:
				tick.Stop()
				return
			case <-runCtx.Done():
				tick.Stop()
				return
			case <-tick.C:
			}
			renewCtx, stop := context.WithDeadline(runCtx, deadline)
			next, renewErr := s.renew(renewCtx, key)
			stop()
			if renewErr != nil {
				var denied *Blocked
				if !errors.As(renewErr, &denied) {
					renewErr = &Blocked{Reason: WaitAccounting, Until: s.now().Add(30 * time.Second), Err: renewErr}
				}
				cancel(renewErr)
				return
			}
			// If the old timer already fired, cancellation is irreversible.
			if runCtx.Err() != nil {
				return
			}
			deadline = next
			timer.Reset(max(time.Until(deadline), 0))
		}
	}()
	workErr := work(runCtx)
	close(done)
	<-stopped
	timer.Stop()
	cause := context.Cause(runCtx)
	cancel(context.Canceled)
	outcome := "succeeded"
	if workErr != nil {
		outcome = "failed"
	}
	if ctx.Err() != nil {
		outcome = "failed"
		if errors.Is(ctx.Err(), context.Canceled) {
			outcome = "interrupted"
		}
		workErr = errors.Join(ctx.Err(), workErr)
	} else if cause != nil {
		outcome = "limit"
		// Runtime errors can classify cancellation as a generic build failure.
		// Keep the reservation reason first so every consumer sees the limit.
		workErr = errors.Join(cause, workErr)
	}
	finishCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer stop()
	return errors.Join(workErr, s.finish(finishCtx, key, outcome))
}
