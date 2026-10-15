// Package idle lets previews nobody visits sleep and wakes them on the next
// visit. A sleeping preview keeps its address, route record and managed
// services (so their data survives); only its app and processes stop, which
// frees the memory and CPU they held. The router sends its visitors to a
// wake page on the dashboard, which starts it again and sends them back.
package idle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ajaykathwatee/previa/internal/idle/dbgen"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

const (
	// Queue runs sleeps and wakes; a lock per preview serializes them.
	Queue      = "idle"
	MaxWorkers = 4
	// SleepInterval is how often idle previews are looked for.
	SleepInterval = 5 * time.Minute
	sleepBatch    = 20
	// visitsWithin: previews sleep only while the router's request counts
	// were read this recently, so an unread visit never puts one to sleep.
	visitsWithin = 3 * time.Minute
)

// Containers stops and starts a published candidate's app and processes;
// *runtime.Runtime implements it.
type Containers interface {
	SleepPreview(ctx context.Context, pullRequestID, generation, revision int64) error
	WakePreview(ctx context.Context, pullRequestID, generation, revision int64) error
}

// Routes rewrites the router's file from the accepted route table;
// *routing.Controller implements it.
type Routes interface {
	Sync(ctx context.Context) error
}

// Visits reports whether the router's request counts were read recently;
// *routing.VisitHealth implements it.
type Visits interface {
	Fresh(within time.Duration) bool
}

// Idle sleeps and wakes previews.
type Idle struct {
	pool       *pgxpool.Pool
	containers Containers
	routes     Routes
	visits     Visits
	after      time.Duration // PREVIEW_IDLE_SLEEP; zero: previews never sleep
	logger     *slog.Logger
	now        func() time.Time
}

func New(pool *pgxpool.Pool, containers Containers, routes Routes, visits Visits, after time.Duration, logger *slog.Logger) (*Idle, error) {
	if pool == nil || containers == nil || routes == nil || visits == nil || logger == nil || after < 0 {
		return nil, errors.New("idle previews need a database, containers, routes, visit counts and a logger")
	}
	return &Idle{pool: pool, containers: containers, routes: routes, visits: visits, after: after, logger: logger, now: time.Now}, nil
}

// SleepIdle puts previews to sleep that nobody visited for the idle time.
// It returns how many it put to sleep.
func (i *Idle) SleepIdle(ctx context.Context) (int, error) {
	if i.after <= 0 {
		return 0, nil
	}
	if !i.visits.Fresh(visitsWithin) {
		i.logger.WarnContext(ctx, "previews are not put to sleep: the router's request counts were not read recently")
		return 0, nil
	}
	idleBefore := pgtype.Timestamptz{Time: i.now().Add(-i.after), Valid: true}
	ids, err := dbgen.New(i.pool).ListIdlePreviews(ctx, dbgen.ListIdlePreviewsParams{IdleBefore: idleBefore, BatchSize: sleepBatch})
	if err != nil {
		return 0, fmt.Errorf("list idle previews: %w", err)
	}
	slept := 0
	var errs []error
	for _, id := range ids {
		ok, err := i.sleep(ctx, id, idleBefore)
		if err != nil {
			errs = append(errs, err)
		}
		if ok {
			slept++
		}
	}
	return slept, errors.Join(errs...)
}

// sleep marks one preview asleep, so the router sends visitors to the wake
// page, then stops its app and processes, unless it was woken meanwhile.
func (i *Idle) sleep(ctx context.Context, environmentID int64, idleBefore pgtype.Timestamptz) (bool, error) {
	var target dbgen.LockIdlePreviewRow
	err := database.InTransaction(ctx, i.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if err := q.LockPreview(ctx, environmentID); err != nil {
			return err
		}
		var err error
		if target, err = q.LockIdlePreview(ctx, dbgen.LockIdlePreviewParams{EnvironmentID: environmentID, IdleBefore: idleBefore}); err != nil {
			return err
		}
		if err := q.MarkAsleep(ctx, environmentID); err != nil {
			return err
		}
		_, err = q.AdvanceRouteTable(ctx)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // Visited, rebuilt or removed meanwhile.
	}
	if err != nil {
		return false, fmt.Errorf("put preview %d to sleep: %w", environmentID, err)
	}
	if err := i.routes.Sync(ctx); err != nil {
		return true, fmt.Errorf("route sleeping preview %d to the wake page: %w", environmentID, err)
	}
	err = database.InTransaction(ctx, i.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if err := q.LockPreview(ctx, environmentID); err != nil {
			return err
		}
		if _, err := q.LockSleeping(ctx, environmentID); err != nil {
			return err // Woken (or rebuilt) meanwhile: keep it running.
		}
		return i.containers.SleepPreview(ctx, target.PullRequestID, target.AppliedGeneration, target.AppliedRevision)
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return true, fmt.Errorf("stop sleeping preview %d: %w", environmentID, err)
	}
	i.logger.InfoContext(ctx, "preview asleep", "environment_id", environmentID, "pr_id", target.PullRequestID)
	return true, nil
}

// Wake starts a sleeping preview and routes it again. A preview that cannot
// wake stays asleep with a note for its wake page.
func (i *Idle) Wake(ctx context.Context, environmentID int64) error {
	var woke bool
	err := database.InTransaction(ctx, i.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if err := q.LockPreview(ctx, environmentID); err != nil {
			return err
		}
		target, err := q.LockSleeping(ctx, environmentID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // Already awake, rebuilt or removed.
		}
		if err != nil {
			return err
		}
		if err := i.containers.WakePreview(ctx, target.PullRequestID, target.AppliedGeneration, target.AppliedRevision); err != nil {
			i.logger.WarnContext(ctx, "preview did not wake", "environment_id", environmentID, "error", err)
			// Containers that started go back to sleep with it.
			_ = i.containers.SleepPreview(context.WithoutCancel(ctx), target.PullRequestID, target.AppliedGeneration, target.AppliedRevision)
			return q.FailWake(ctx, dbgen.FailWakeParams{EnvironmentID: environmentID, Note: wakeNote(err)})
		}
		if _, err := q.MarkAwake(ctx, environmentID); err != nil {
			return err
		}
		if _, err := q.AdvanceRouteTable(ctx); err != nil {
			return err
		}
		woke = true
		return nil
	})
	if err != nil {
		return fmt.Errorf("wake preview %d: %w", environmentID, err)
	}
	if !woke {
		return nil
	}
	if err := i.routes.Sync(ctx); err != nil {
		return fmt.Errorf("route woken preview %d: %w", environmentID, err)
	}
	i.logger.InfoContext(ctx, "preview woke", "environment_id", environmentID)
	return nil
}

// safeMessage is a classified failure with words safe for visitors.
type safeMessage interface{ SafeMessage() string }

func wakeNote(err error) string {
	var m safeMessage
	if errors.As(err, &m) && m.SafeMessage() != "" {
		return m.SafeMessage()
	}
	return "The preview could not start again."
}

// SleepArgs is the periodic look for idle previews.
type SleepArgs struct{}

func (SleepArgs) Kind() string { return "preview_sleep" }

// WakeArgs wakes one preview for a waiting visitor.
type WakeArgs struct {
	EnvironmentID int64 `json:"environment_id"`
}

func (WakeArgs) Kind() string { return "preview_wake" }

// InsertOpts keeps one waiting wake per preview.
func (WakeArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: Queue, MaxAttempts: 1, UniqueOpts: river.UniqueOpts{
		ByArgs: true,
		ByState: []rivertype.JobState{
			rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning,
			rivertype.JobStateRetryable, rivertype.JobStateScheduled,
		},
	}}
}

type sleepWorker struct {
	river.WorkerDefaults[SleepArgs]
	idle *Idle
}

func (*sleepWorker) Timeout(*river.Job[SleepArgs]) time.Duration { return 4 * time.Minute }
func (w *sleepWorker) Work(ctx context.Context, _ *river.Job[SleepArgs]) error {
	_, err := w.idle.SleepIdle(ctx)
	return err
}

type wakeWorker struct {
	river.WorkerDefaults[WakeArgs]
	idle *Idle
}

func (*wakeWorker) Timeout(*river.Job[WakeArgs]) time.Duration { return 5 * time.Minute }
func (w *wakeWorker) Work(ctx context.Context, job *river.Job[WakeArgs]) error {
	return w.idle.Wake(ctx, job.Args.EnvironmentID)
}

// Register adds the idle queue, its workers and, when previews may sleep,
// the periodic look for idle ones (on River's leader only).
func (i *Idle) Register(cfg *river.Config) {
	cfg.Queues[Queue] = river.QueueConfig{MaxWorkers: MaxWorkers}
	river.AddWorker(cfg.Workers, &wakeWorker{idle: i})
	river.AddWorker(cfg.Workers, &sleepWorker{idle: i})
	if i.after > 0 {
		cfg.PeriodicJobs = append(cfg.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(SleepInterval), func() (river.JobArgs, *river.InsertOpts) {
			return SleepArgs{}, &river.InsertOpts{Queue: Queue, MaxAttempts: 1}
		}, nil))
	}
}
