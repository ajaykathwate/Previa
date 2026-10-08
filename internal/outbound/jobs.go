package outbound

import (
	"context"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// Queue runs the refresh, one at a time.
const Queue = "outbound"

// RefreshInterval: every preview network's approvals on the egress proxy are
// brought up to date at least this often (and after every change).
const RefreshInterval = time.Minute

// RefreshArgs runs one refresh. The schedule inserts it periodically and an
// owner's change inserts one more, so a withdrawn approval stops working
// within seconds.
type RefreshArgs struct{}

func (RefreshArgs) Kind() string { return "outbound_refresh" }

// Refresher writes the current approvals to the egress proxy; the preview
// runtime implements it.
type Refresher interface {
	RefreshOutbound(ctx context.Context) error
}

// RefreshWorker runs refreshes.
type RefreshWorker struct {
	river.WorkerDefaults[RefreshArgs]
	Refresher Refresher
}

func (*RefreshWorker) Timeout(*river.Job[RefreshArgs]) time.Duration { return time.Minute }

func (w *RefreshWorker) Work(ctx context.Context, _ *river.Job[RefreshArgs]) error {
	return w.Refresher.RefreshOutbound(ctx)
}

// Register adds the refresh worker, its queue and its schedule to a River
// worker configuration.
func Register(cfg *river.Config, refresher Refresher) {
	if cfg.Queues == nil {
		cfg.Queues = map[string]river.QueueConfig{}
	}
	cfg.Queues[Queue] = river.QueueConfig{MaxWorkers: 1}
	if cfg.Workers == nil {
		cfg.Workers = river.NewWorkers()
	}
	river.AddWorker(cfg.Workers, &RefreshWorker{Refresher: refresher})
	cfg.PeriodicJobs = append(cfg.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(RefreshInterval), func() (river.JobArgs, *river.InsertOpts) {
		return RefreshArgs{}, &river.InsertOpts{Queue: Queue, MaxAttempts: 1, UniqueOpts: river.UniqueOpts{
			ByArgs: true,
			ByState: []rivertype.JobState{
				rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning,
				rivertype.JobStateRetryable, rivertype.JobStateScheduled,
			},
		}}
	}, &river.PeriodicJobOpts{RunOnStart: true}))
}
