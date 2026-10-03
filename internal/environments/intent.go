package environments

import (
	"errors"
	"regexp"
	"time"
)

type IntentState string

const (
	IntentPending    IntentState = "pending"
	IntentQueued     IntentState = "queued"
	IntentRunning    IntentState = "running"
	IntentSucceeded  IntentState = "succeeded"
	IntentFailed     IntentState = "failed"
	IntentCancelled  IntentState = "cancelled"
	IntentSuperseded IntentState = "superseded"
)

type IntentAction string

const (
	BuildLatest IntentAction = "build"
	RetryBuild  IntentAction = "retry"
	Rebuild     IntentAction = "rebuild"
)

var (
	ErrIntentUnavailable = errors.New("pull request is unavailable or access is denied")
	ErrPRClosed          = errors.New("build requests require an open pull request")
	ErrIntentConflict    = errors.New("build intent changed; refresh before requesting work")
	ErrIntentAction      = errors.New("build action is not allowed for the current intent state")
	ErrIntentKeyConflict = errors.New("idempotency key was used for a different build command")
	ErrIntentNotDue      = errors.New("build intent is not eligible yet")
)

// BuildIntent is current coordination state, not a historical attempt record.
// Idempotency keys are opaque and must not be logged or exposed in views.
type BuildIntent struct {
	PullRequestID  int64
	Revision       int64
	BaseRevision   int64
	Generation     int64
	HeadSHA        string
	Source         string
	Action         string
	RequestedBy    int64
	IdempotencyKey string
	State          IntentState
	AvailableAt    time.Time
	RequestedAt    time.Time
	StartedAt      time.Time
	CompletedAt    time.Time
}

type IntentCommand struct {
	UserID           int64
	PullRequestID    int64
	ExpectedRevision int64
	IdempotencyKey   string
	Action           IntentAction
}

var intentKey = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

func (c IntentCommand) validate() error {
	if c.UserID <= 0 || c.PullRequestID <= 0 || c.ExpectedRevision < 0 || !intentKey.MatchString(c.IdempotencyKey) {
		return errors.New("build command requires an owner, PR, nonnegative expected revision and a 16–128 character URL-safe idempotency key")
	}
	if c.Action != BuildLatest && c.Action != RetryBuild && c.Action != Rebuild {
		return ErrIntentAction
	}
	return nil
}

type IntentOutcome struct {
	Intent    BuildIntent
	Coalesced bool
}

type IntentView struct {
	PullRequestID int64
	Generation    int64
	HeadSHA       string
	PRState       string
	BuildMode     string
	Current       *BuildIntent
}

type IntentTransition struct {
	UserID        int64
	PullRequestID int64
	Revision      int64
	Generation    int64
	Next          IntentState
}

func (c IntentTransition) validate() error {
	if c.UserID <= 0 || c.PullRequestID <= 0 || c.Revision <= 0 || c.Generation < 0 {
		return ErrIntentConflict
	}
	switch c.Next {
	case IntentQueued, IntentRunning, IntentSucceeded, IntentFailed:
		return nil
	default:
		return ErrIntentAction
	}
}

// ObservationOutcome describes authorization only. TeardownRequired must become
// durable teardown work in Prompt 3; this foundation does not enqueue any job.
type ObservationOutcome struct {
	Intent           *BuildIntent
	TeardownRequired bool
}
