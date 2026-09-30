package queue

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

const PingQueue = "ping"

// PingArgs carries a synthetic PR key for testing; it does not create a PR record.
// Later PR jobs must use the same JSON key/tag and ActivePRUniqueOpts policy.
type PingArgs struct {
	PRKey string `json:"pr_key" river:"unique"`
}

func (PingArgs) Kind() string { return "ping" }

func (PingArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: PingQueue, MaxAttempts: 5, UniqueOpts: ActivePRUniqueOpts()}
}

// ActivePRUniqueOpts prevents active jobs sharing a PR key, even across kinds/queues.
// Terminal jobs release the key so new work can be enqueued for that PR.
func ActivePRUniqueOpts() river.UniqueOpts {
	return river.UniqueOpts{
		ByArgs: true, ExcludeKind: true,
		ByState: []rivertype.JobState{
			rivertype.JobStateAvailable, rivertype.JobStatePending,
			rivertype.JobStateRunning, rivertype.JobStateRetryable,
			rivertype.JobStateScheduled,
		},
	}
}

var validPRKey = regexp.MustCompile(`^[A-Za-z0-9_./:#-]{1,128}$`)

func ValidatePRKey(key string) error {
	if !validPRKey.MatchString(key) {
		return errors.New("PR key must be 1–128 letters, digits, or _ . / : # -")
	}
	return nil
}

// PingWorker is idempotent: execution only records a diagnostic log message.
type PingWorker struct {
	river.WorkerDefaults[PingArgs]
	Logger *slog.Logger
}

func (w *PingWorker) Timeout(*river.Job[PingArgs]) time.Duration { return PingTimeout }

func (w *PingWorker) Work(ctx context.Context, job *river.Job[PingArgs]) error {
	if err := ValidatePRKey(job.Args.PRKey); err != nil {
		return river.JobCancel(err) // Invalid input is permanent; do not retry it.
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	w.Logger.InfoContext(ctx, "ping job processed", "job_id", job.ID, "pr_key", job.Args.PRKey, "attempt", job.Attempt)
	return nil // River persists completed/finalized_at after Work succeeds.
}
