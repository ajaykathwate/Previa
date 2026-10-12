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
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river/rivertype"
)

// Reasons a build request waits.
const (
	WaitCapacity = "capacity" // every build slot, or the preview budget, is in use; or another account's turn
	WaitDisk     = "disk"     // the build host is short of disk space
	WaitPaused   = "paused"   // the operator paused new builds (BUILDS_PAUSED)
	WaitAccount  = "account"  // the account already runs BUILDS_PER_ACCOUNT builds
	WaitQuota    = "quota"    // the account used its daily build minutes
)

// liveWait is how recently a waiting request must have looked again to hold
// its turn; waiting jobs look at least every five minutes (waitDelay).
const liveWait = 10 * time.Minute

// WaitingError is a build request that cannot proceed for lack of capacity.
// It is not a failure: the request keeps its place (queued, or running when it
// was already admitted), the PR's report says it is waiting, and the job is
// retried with backoff without spending an attempt.
type WaitingError struct {
	Reason string
	Err    error
	Until  time.Time // when to look again, if not the usual backoff (a daily budget's reset)
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

// AccountLimits share one server fairly between accounts (installation
// owners). Zero turns a limit off.
type AccountLimits struct {
	BuildsPerAccount int // running builds at once (BUILDS_PER_ACCOUNT)
	MinutesPerDay    int // build minutes per UTC day (BUILD_MINUTES_PER_ACCOUNT_DAY)
}

// WithAccountLimits bounds each account's builds; see AccountLimits.
func (r *Repository) WithAccountLimits(limits AccountLimits) *Repository {
	r.accounts = AccountLimits{BuildsPerAccount: max(limits.BuildsPerAccount, 0), MinutesPerDay: max(limits.MinutesPerDay, 0)}
	return r
}

// admissionWaitTx decides whether the request must wait before it may take a
// build slot, and why. The caller holds the PR's scope locks; the advisory
// lock serializes admissions until the caller's transaction ends, so the
// counts are exact. In order: every slot taken; the account's own builds;
// its daily minutes; and, for the last free slot, another account's turn
// (one with nothing building that has waited longer). A request that already
// runs keeps its slot and never waits here.
func (r *Repository) admissionWaitTx(ctx context.Context, q *dbgen.Queries, pullRequestID int64, now time.Time) (*WaitingError, error) {
	if r.buildSlots <= 0 && r.accounts == (AccountLimits{}) {
		return nil, nil
	}
	if err := q.LockBuildSlots(ctx); err != nil {
		return nil, fmt.Errorf("lock build slots: %w", err)
	}
	current, err := q.ReadIntentWait(ctx, pullRequestID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // nothing to admit; the claim finds no row
	}
	if err != nil {
		return nil, fmt.Errorf("read build request wait: %w", err)
	}
	if current.State == string(IntentRunning) {
		return nil, nil
	}
	running, err := q.CountRunningBuilds(ctx, pullRequestID)
	if err != nil {
		return nil, fmt.Errorf("count running builds: %w", err)
	}
	if r.buildSlots > 0 && running >= int64(r.buildSlots) {
		return &WaitingError{Reason: WaitCapacity}, nil
	}
	owner, err := q.FindPROwner(ctx, pullRequestID)
	if err != nil {
		return nil, fmt.Errorf("find the build's account: %w", err)
	}
	if r.accounts.BuildsPerAccount > 0 {
		mine, err := q.CountAccountRunningBuilds(ctx, dbgen.CountAccountRunningBuildsParams{PrID: pullRequestID, OwnerUserID: owner.Int64})
		if err != nil {
			return nil, fmt.Errorf("count the account's running builds: %w", err)
		}
		if mine >= int64(r.accounts.BuildsPerAccount) {
			return &WaitingError{Reason: WaitAccount}, nil
		}
	}
	if r.accounts.MinutesPerDay > 0 {
		day := now.UTC()
		used, err := q.AccountBuildSeconds(ctx, dbgen.AccountBuildSecondsParams{UserID: owner.Int64, Day: pgtype.Date{Time: day, Valid: true}})
		if err != nil {
			return nil, fmt.Errorf("read the account's build minutes: %w", err)
		}
		if used >= int64(r.accounts.MinutesPerDay)*60 {
			midnight := time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, time.UTC)
			return &WaitingError{Reason: WaitQuota, Until: midnight}, nil
		}
	}
	if r.buildSlots > 0 && running == int64(r.buildSlots)-1 {
		since := now
		if current.WaitingSince.Valid {
			since = current.WaitingSince.Time
		}
		turn, err := q.OlderWaiterFromIdleAccount(ctx, dbgen.OlderWaiterFromIdleAccountParams{
			Since: pgtype.Timestamptz{Time: since, Valid: true}, LiveAfter: pgtype.Timestamptz{Time: now.Add(-liveWait), Valid: true}, OwnerUserID: owner.Int64,
		})
		if err != nil {
			return nil, fmt.Errorf("check other accounts' turns: %w", err)
		}
		if turn {
			return &WaitingError{Reason: WaitCapacity}, nil
		}
	}
	return nil, nil
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
	if err := dbgen.New(tx).TouchIntentWaiting(ctx, dbgen.TouchIntentWaitingParams{PrID: request.PullRequestID, Revision: request.Revision}); err != nil {
		return fmt.Errorf("note a waiting build request looked again: %w", err)
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
