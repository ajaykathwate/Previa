package accounts

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

type sweepArgs struct{}

func (sweepArgs) Kind() string { return "account_lifecycle" }

type sweepWorker struct {
	river.WorkerDefaults[sweepArgs]
	service *Service
}

func (w *sweepWorker) Work(ctx context.Context, _ *river.Job[sweepArgs]) error {
	return w.service.Sweep(ctx)
}
func (*sweepWorker) Timeout(*river.Job[sweepArgs]) time.Duration { return 90 * time.Second }

func (s *Service) Register(c *river.Config) {
	if c.Queues == nil {
		c.Queues = map[string]river.QueueConfig{}
	}
	if c.Workers == nil {
		c.Workers = river.NewWorkers()
	}
	c.Queues["accounts"] = river.QueueConfig{MaxWorkers: 1}
	river.AddWorker(c.Workers, &sweepWorker{service: s})
	c.PeriodicJobs = append(c.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(30*time.Second), func() (river.JobArgs, *river.InsertOpts) {
		return sweepArgs{}, &river.InsertOpts{Queue: "accounts", MaxAttempts: 3}
	}, &river.PeriodicJobOpts{RunOnStart: true}))
}

func (s *Service) Sweep(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM lifecycle_operations WHERE stage<>'complete' AND available_at<=clock_timestamp() ORDER BY created_at,id LIMIT 50`)
	if err != nil {
		return fmt.Errorf("list pending account operations: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read pending account operations: %w", err)
	}
	var failures []error
	for _, id := range ids {
		if err := s.Process(ctx, id); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// Process holds a session advisory lock during external checks, never a row
// transaction. A crash releases it; every durable step is safe to replay.
func (s *Service) Process(ctx context.Context, id string) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire account operation lock: %w", err)
	}
	defer conn.Release()
	var locked bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended('previa.accounts.'||$1,0))`, id).Scan(&locked); err != nil {
		return fmt.Errorf("lock account operation: %w", err)
	}
	if !locked {
		return nil
	}
	defer func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, e := conn.Exec(c, `SELECT pg_advisory_unlock(hashtextextended('previa.accounts.'||$1,0))`, id); e != nil {
			_ = conn.Conn().Close(c)
		}
	}()
	var o Operation
	err = conn.QueryRow(ctx, `SELECT id::text,kind,target_id,target_public_id::text,coalesce(workspace_id,0),actor_id,stage,journal_ack_at FROM lifecycle_operations WHERE id=$1`, id).
		Scan(&o.ID, &o.Kind, &o.Target, &o.TargetPublicID, &o.Workspace, &o.Actor, &o.Stage, &o.JournalAcknowledged)
	if err != nil {
		return fmt.Errorf("read pending account operation: %w", err)
	}
	if o.Stage == "complete" {
		return nil
	}
	drained, err := s.drained(ctx, o)
	if err != nil {
		return errors.Join(err, s.wait(ctx, id, "cleanup_retry"))
	}
	if !drained {
		return s.wait(ctx, id, "cleanup_pending")
	}
	if o.JournalAcknowledged == nil {
		return s.wait(ctx, id, "journal_pending")
	}
	err = s.transaction(ctx, func(tx pgx.Tx) error {
		var stage string
		if err := tx.QueryRow(ctx, `SELECT stage FROM lifecycle_operations WHERE id=$1 FOR UPDATE`, id).Scan(&stage); err != nil {
			return fmt.Errorf("lock account erasure: %w", err)
		}
		if stage == "complete" {
			return nil
		}
		if err := s.erase(ctx, tx, o); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE lifecycle_operations SET stage='complete',last_error='',completed_at=clock_timestamp() WHERE id=$1 AND journal_ack_at IS NOT NULL`, id); err != nil {
			return fmt.Errorf("finish account operation: %w", err)
		}
		return nil
	})
	if err != nil {
		return errors.Join(err, s.wait(ctx, id, "erase_retry"))
	}
	return nil
}

func (s *Service) wait(ctx context.Context, id, reason string) error {
	_, err := s.pool.Exec(ctx, `UPDATE lifecycle_operations SET last_error=$2,stage=CASE WHEN journal_ack_at IS NULL THEN 'journal' ELSE 'cleanup' END,available_at=clock_timestamp()+interval '30 seconds' WHERE id=$1 AND stage<>'complete'`, id, reason)
	if err != nil {
		return fmt.Errorf("record account operation progress: %w", err)
	}
	return nil
}

func (s *Service) drained(ctx context.Context, o Operation) (bool, error) {
	if o.Kind != "repository" {
		var pending bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM lifecycle_operations WHERE parent_id=$1 AND stage<>'complete')`, o.ID).Scan(&pending); err != nil {
			return false, fmt.Errorf("check child cleanup: %w", err)
		}
		return !pending, nil
	}
	if s.comments != nil {
		if done, err := s.comments.Disconnected(ctx, o.Target); err != nil || !done {
			return false, err
		}
	}
	if s.previews == nil {
		return false, nil
	}
	if done, err := s.previews.RepositoryDrained(ctx, o.Target); err != nil || !done {
		return false, err
	}
	for _, c := range s.cleaners {
		if done, err := c.RepositoryDrained(ctx, o.Target); err != nil || !done {
			return false, err
		}
	}
	return true, nil
}

func (s *Service) erase(ctx context.Context, tx pgx.Tx, o Operation) error {
	switch o.Kind {
	case "repository":
		if len(s.repositories) == 0 {
			return errors.New("repository erasure is not configured")
		}
		for _, e := range s.repositories {
			if err := e.EraseRepositoryTx(ctx, tx, o.Target); err != nil {
				return err
			}
		}
	case "installation":
		if s.installations == nil {
			return errors.New("installation erasure is not configured")
		}
		return s.installations.EraseInstallationTx(ctx, tx, o.Target)
	case "workspace":
		if len(s.workspaces) == 0 {
			return errors.New("workspace erasure is not configured")
		}
		for _, e := range s.workspaces {
			if err := e.EraseWorkspaceTx(ctx, tx, o.Target); err != nil {
				return err
			}
		}
	case "account":
		if len(s.people) == 0 {
			return errors.New("personal erasure is not configured")
		}
		for _, e := range s.people {
			if err := e.EraseAccountTx(ctx, tx, o.Target); err != nil {
				return err
			}
		}
	default:
		return errors.New("unsupported lifecycle operation")
	}
	return nil
}
