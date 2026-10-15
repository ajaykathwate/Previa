package housekeeping

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/ajaykathwatee/previa/internal/housekeeping/dbgen"
	"github.com/ajaykathwatee/previa/internal/platform/metrics"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// Housekeeper runs the housekeeping jobs. A nil part is not scheduled, so a
// worker without GitHub access, for example, simply skips PR reconciliation.
type Housekeeper struct {
	Recovery     *Recovery
	Resources    *Resources
	PullRequests *PullRequests
	Expiry       *Expiry
	Prune        *Prune
	Records      *Records

	pool   *pgxpool.Pool
	logger *slog.Logger
	now    func() time.Time

	mu            sync.Mutex
	lastEmergency time.Time
}

func NewHousekeeper(pool *pgxpool.Pool, logger *slog.Logger) (*Housekeeper, error) {
	if pool == nil || logger == nil {
		return nil, errors.New("housekeeping requires a database and a logger")
	}
	return &Housekeeper{pool: pool, logger: logger, now: time.Now}, nil
}

// Job arguments carry no data: each job reads what is due from PostgreSQL.
type (
	RecoverArgs      struct{}
	ResourcesArgs    struct{}
	PullRequestsArgs struct{}
	ExpiryArgs       struct{}
	RecordsArgs      struct{}
	PruneArgs        struct {
		Emergency bool `json:"emergency"`
	}
)

func (RecoverArgs) Kind() string      { return "housekeeping_recover" }
func (ResourcesArgs) Kind() string    { return "housekeeping_resources" }
func (PullRequestsArgs) Kind() string { return "housekeeping_pull_requests" }
func (ExpiryArgs) Kind() string       { return "housekeeping_expiry" }
func (PruneArgs) Kind() string        { return "housekeeping_prune" }
func (RecordsArgs) Kind() string      { return "housekeeping_records" }

// insertOpts keeps at most one unfinished job of each kind: a slow pass is
// never joined by a pile of later ones. A failed pass is not retried; the
// next scheduled one runs anyway.
func insertOpts() *river.InsertOpts {
	return &river.InsertOpts{Queue: Queue, MaxAttempts: 1, UniqueOpts: river.UniqueOpts{
		ByArgs: true,
		ByState: []rivertype.JobState{
			rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning,
			rivertype.JobStateRetryable, rivertype.JobStateScheduled,
		},
	}}
}

func periodic(interval time.Duration, args river.JobArgs) *river.PeriodicJob {
	return river.NewPeriodicJob(river.PeriodicInterval(interval), func() (river.JobArgs, *river.InsertOpts) {
		return args, insertOpts()
	}, &river.PeriodicJobOpts{RunOnStart: true})
}

// Register adds the housekeeping queue (one job at a time), the workers and
// the periodic schedules to a River worker configuration. River runs periodic
// jobs only on its elected leader.
func (h *Housekeeper) Register(cfg *river.Config) {
	if cfg.Queues == nil {
		cfg.Queues = map[string]river.QueueConfig{}
	}
	cfg.Queues[Queue] = river.QueueConfig{MaxWorkers: 1}
	if cfg.Workers == nil {
		cfg.Workers = river.NewWorkers()
	}
	add := func(interval time.Duration, args river.JobArgs) {
		cfg.PeriodicJobs = append(cfg.PeriodicJobs, periodic(interval, args))
	}
	if h.Recovery != nil {
		river.AddWorker(cfg.Workers, &recoverWorker{h: h})
		add(RecoverInterval, RecoverArgs{})
	}
	if h.Resources != nil {
		river.AddWorker(cfg.Workers, &resourcesWorker{h: h})
		add(ResourcesInterval, ResourcesArgs{})
	}
	if h.PullRequests != nil {
		river.AddWorker(cfg.Workers, &pullRequestsWorker{h: h})
		add(PullRequestInterval, PullRequestsArgs{})
	}
	if h.Expiry != nil {
		river.AddWorker(cfg.Workers, &expiryWorker{h: h})
		add(ExpiryInterval, ExpiryArgs{})
	}
	if h.Prune != nil {
		river.AddWorker(cfg.Workers, &pruneWorker{h: h})
		add(PruneInterval, PruneArgs{})
	}
	if h.Records != nil {
		river.AddWorker(cfg.Workers, &recordsWorker{h: h})
		add(RecordsInterval, RecordsArgs{})
	}
}

// Recover runs one recovery pass and, when a build waits for disk space, an
// emergency prune (at most every EmergencyPruneEvery).
func (h *Housekeeper) Recover(ctx context.Context) error {
	pass, err := h.Recovery.RunOnce(ctx)
	metrics.Housekeeping("recovery", "wakeups", pass.Wakeups)
	metrics.Housekeeping("recovery", "operations", pass.Operations)
	metrics.Housekeeping("recovery", "teardowns", pass.Teardowns)
	if h.Prune == nil {
		return err
	}
	waiting, countErr := dbgen.New(h.pool).CountWaitingForDisk(ctx)
	if countErr != nil || waiting == 0 {
		return errors.Join(err, countErr)
	}
	h.mu.Lock()
	due := h.now().Sub(h.lastEmergency) >= EmergencyPruneEvery
	if due {
		h.lastEmergency = h.now()
	}
	h.mu.Unlock()
	if !due {
		return err
	}
	h.logger.WarnContext(ctx, "builds wait for disk space; pruning now", "waiting", waiting)
	prune, pruneErr := h.Prune.RunOnce(ctx, true)
	h.Prune.count(prune)
	return errors.Join(err, pruneErr)
}

type recoverWorker struct {
	river.WorkerDefaults[RecoverArgs]
	h *Housekeeper
}

func (*recoverWorker) Timeout(*river.Job[RecoverArgs]) time.Duration { return 2 * time.Minute }
func (w *recoverWorker) Work(ctx context.Context, _ *river.Job[RecoverArgs]) error {
	return w.h.Recover(ctx)
}

type resourcesWorker struct {
	river.WorkerDefaults[ResourcesArgs]
	h *Housekeeper
}

func (*resourcesWorker) Timeout(*river.Job[ResourcesArgs]) time.Duration { return 5 * time.Minute }
func (w *resourcesWorker) Work(ctx context.Context, _ *river.Job[ResourcesArgs]) error {
	pass, err := w.h.Resources.RunOnce(ctx)
	metrics.Housekeeping("resources", "missing", len(pass.Missing))
	if !w.h.Resources.dryRun { // a dry run only reports what it would delete
		metrics.Housekeeping("resources", "orphans", len(pass.Orphans))
		metrics.Housekeeping("resources", "unknown_removed", len(pass.Removed))
	}
	return err
}

type pullRequestsWorker struct {
	river.WorkerDefaults[PullRequestsArgs]
	h *Housekeeper
}

func (*pullRequestsWorker) Timeout(*river.Job[PullRequestsArgs]) time.Duration {
	return 5 * time.Minute
}
func (w *pullRequestsWorker) Work(ctx context.Context, _ *river.Job[PullRequestsArgs]) error {
	pass, err := w.h.PullRequests.RunOnce(ctx)
	confirmed := 0
	for _, n := range pass.Outcomes {
		confirmed += n
	}
	metrics.Housekeeping("pull_requests", "confirmed", confirmed)
	return err
}

type expiryWorker struct {
	river.WorkerDefaults[ExpiryArgs]
	h *Housekeeper
}

func (*expiryWorker) Timeout(*river.Job[ExpiryArgs]) time.Duration { return 2 * time.Minute }
func (w *expiryWorker) Work(ctx context.Context, _ *river.Job[ExpiryArgs]) error {
	pass, err := w.h.Expiry.RunOnce(ctx)
	metrics.Housekeeping("expiry", "expired", pass.Expired) // zero in a dry run
	return err
}

type pruneWorker struct {
	river.WorkerDefaults[PruneArgs]
	h *Housekeeper
}

func (*pruneWorker) Timeout(*river.Job[PruneArgs]) time.Duration { return 15 * time.Minute }
func (w *pruneWorker) Work(ctx context.Context, job *river.Job[PruneArgs]) error {
	pass, err := w.h.Prune.RunOnce(ctx, job.Args.Emergency)
	w.h.Prune.count(pass)
	return err
}

type recordsWorker struct {
	river.WorkerDefaults[RecordsArgs]
	h *Housekeeper
}

func (*recordsWorker) Timeout(*river.Job[RecordsArgs]) time.Duration { return 10 * time.Minute }
func (w *recordsWorker) Work(ctx context.Context, _ *river.Job[RecordsArgs]) error {
	pass, err := w.h.Records.RunOnce(ctx)
	if !w.h.Records.dryRun {
		metrics.Housekeeping("records", "webhook_deliveries", int(pass.Deliveries))
		metrics.Housekeeping("records", "operations", int(pass.Operations))
		metrics.Housekeeping("records", "pull_requests", pass.PullRequests)
	}
	return err
}
