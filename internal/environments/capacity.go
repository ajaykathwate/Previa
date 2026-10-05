package environments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments/dbgen"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river/rivertype"
)

// Reasons a build request waits.
const (
	WaitCapacity = "capacity" // every build slot, or the preview budget, is in use
	WaitDisk     = "disk"     // the build host is short of disk space
)

// WaitingError is a build request that cannot proceed for lack of capacity.
// It is not a failure: the request keeps its place (queued, or running when it
// was already admitted), the PR's report says it is waiting, and the job is
// retried with backoff without spending an attempt.
type WaitingError struct {
	Reason string
	Err    error
}

func (e *WaitingError) Error() string {
	if e.Err != nil {
		return "waiting for " + e.Reason + ": " + e.Err.Error()
	}
	return "waiting for " + e.Reason
}

func (e *WaitingError) Unwrap() error { return e.Err }

// Headroom says, before a build is admitted, whether the host can take it;
// *runtime.Runtime implements it. It returns a *WaitingError when it cannot.
// The answer is advisory: the runtime checks again, atomically, before it
// starts the candidate.
type Headroom interface {
	Admit(ctx context.Context, request Request) error
}

// WithBuildSlots bounds how many builds run at once across every worker
// (MAX_CONCURRENT_BUILDS). Admission counts running requests under a global
// lock, so the bound holds across processes, retries and restarts. Zero, the
// default, leaves builds unbounded.
func (r *Repository) WithBuildSlots(slots int) *Repository {
	r.buildSlots = max(slots, 0)
	return r
}

// buildSlotsFullTx reports whether every build slot is taken by other PRs.
// The caller holds the PR's scope locks; the advisory lock serializes
// admissions until the caller's transaction ends.
func (r *Repository) buildSlotsFullTx(ctx context.Context, q *dbgen.Queries, pullRequestID int64) (bool, error) {
	if r.buildSlots <= 0 {
		return false, nil
	}
	if err := q.LockBuildSlots(ctx); err != nil {
		return false, fmt.Errorf("lock build slots: %w", err)
	}
	running, err := q.CountRunningBuilds(ctx, pullRequestID)
	if err != nil {
		return false, fmt.Errorf("count running builds: %w", err)
	}
	return running >= int64(r.buildSlots), nil
}

// markWaitingTx notes that the request waits, and reports the PR when the
// note changed.
func (r *Repository) markWaitingTx(ctx context.Context, tx pgx.Tx, request Request, reason string) error {
	changed, err := dbgen.New(tx).MarkIntentWaiting(ctx, dbgen.MarkIntentWaitingParams{
		PrID: request.PullRequestID, Generation: request.Generation, Revision: request.Revision, Reason: reason,
	})
	if err != nil {
		return fmt.Errorf("note waiting build request: %w", err)
	}
	if changed == 0 {
		return nil
	}
	return r.reportTx(ctx, tx, request.PullRequestID)
}

// markWaiting notes, under the PR's lock, that an admitted or queued request
// waits.
func (r *Repository) markWaiting(ctx context.Context, request Request, reason string) error {
	return database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := dbgen.New(tx).LockRequest(ctx, request.PullRequestID); err != nil {
			return fmt.Errorf("lock PR to note waiting: %w", err)
		}
		return r.markWaitingTx(ctx, tx, request, reason)
	})
}

// waitingReason classifies a runtime failure that means "not now" rather than
// "failed": the preview budget is full, or the build host is short of disk.
func waitingReason(err error) string {
	var waiting *WaitingError
	if errors.As(err, &waiting) {
		return waiting.Reason
	}
	var coded failureCoder
	if !errors.As(err, &coded) {
		return ""
	}
	switch coded.FailureCode() {
	case "capacity":
		return WaitCapacity
	case "disk_low":
		return WaitDisk
	}
	return ""
}

// waitDelay backs off a waiting job: 30 seconds, doubling per snooze, at most
// five minutes. River keeps the snooze count in the job's metadata.
func waitDelay(job *rivertype.JobRow) time.Duration {
	var metadata struct {
		Snoozes int64 `json:"snoozes"`
	}
	_ = json.Unmarshal(job.Metadata, &metadata) // no metadata: the first wait
	delay := 30 * time.Second
	for i := int64(0); i < metadata.Snoozes && delay < 5*time.Minute; i++ {
		delay *= 2
	}
	return min(delay, 5*time.Minute)
}
