package environments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/queue"
)

// Runtime is the Docker boundary the lifecycle needs; *runtime.Runtime
// implements it. Build must return an unregistered healthy container, honor
// cancellation, and label containers AND uniquely tagged images with PR
// ID/generation/revision. Build and startup output goes only to a log writer
// bound to the given Attempt, and never after Build returns. Resources must
// also discover partial builds after process crashes. Removal happens through
// cleanup operations and their executor, not through this interface.
type Runtime interface {
	Build(context.Context, Request, Attempt) (Artifact, error)
	Resources(context.Context, int64) ([]Resource, error)
}

// Attempt identifies one admitted execution of a request. LogEpoch changes on
// every admission, including a River retry of the same revision, so output from
// a superseded execution can never reach the current attempt's logs.
// EnvironmentID is 0 when the PR has no lifetime environment row yet; such an
// attempt has no log stream.
type Attempt struct {
	EnvironmentID int64
	LogEpoch      int64
}

// BuildLogs closes an attempt's log stream once its execution has returned, so
// viewers know no more output is coming. Later writes are rejected by the epoch.
type BuildLogs interface {
	Finish(ctx context.Context, environmentID, epoch int64) error
}

// Resource is something Docker holds for a PR, attributed by its labels: a
// container and/or image (Artifact), or a network (Network, its ID).
type Resource struct {
	PullRequestID int64    `json:"pull_request_id"`
	Generation    int64    `json:"generation"`
	Revision      int64    `json:"revision"`
	Artifact      Artifact `json:"artifact"`
	Network       string   `json:"network,omitempty"`
	// CreatedAt is when Docker created the object, for housekeeping's grace
	// period. It is not part of a cleanup operation's identity.
	CreatedAt time.Time `json:"-"`
}

type Service struct {
	repo         *Repository
	runtime      Runtime
	headroom     Headroom
	verifier     UpstreamVerifier
	logs         BuildLogs
	logger       *slog.Logger
	buildTimeout time.Duration
	now          func() time.Time
}

func NewService(repo *Repository, runtime Runtime, logger *slog.Logger, buildTimeout time.Duration, clocks ...func() time.Time) (*Service, error) {
	if repo == nil || runtime == nil || logger == nil {
		return nil, errors.New("environment service requires repository, runtime, and logger")
	}
	if buildTimeout == 0 {
		buildTimeout = queue.DefaultBuildTimeout
	}
	if buildTimeout < 0 || buildTimeout > 24*time.Hour {
		return nil, errors.New("invalid build timeout")
	}
	now := time.Now
	if len(clocks) > 0 && clocks[0] != nil {
		now = clocks[0]
	}
	return &Service{repo: repo, runtime: runtime, logger: logger, buildTimeout: buildTimeout, now: now}, nil
}

// WithVerifier reads each PR fresh from GitHub before admitting its build;
// *pullrequests.SyncService implements it.
func (s *Service) WithVerifier(v UpstreamVerifier) *Service {
	s.verifier = v
	return s
}

// ErrInterrupted: the worker stopped while the build ran; the request is
// retried, not failed.
var ErrInterrupted = errors.New("the worker stopped during the build; it runs again")

// WithHeadroom asks h, before admitting a build, whether the host can take
// it; a request that cannot start waits queued instead of failing.
func (s *Service) WithHeadroom(h Headroom) *Service {
	s.headroom = h
	return s
}

// WithBuildLogs closes each attempt's log stream when its execution returns.
// Without it, streams stay open until the next admission replaces them.
func (s *Service) WithBuildLogs(logs BuildLogs) *Service {
	s.logs = logs
	return s
}

func (s *Service) finishLogs(ctx context.Context, request Request, attempt Attempt) {
	if s.logs == nil || attempt.EnvironmentID == 0 {
		return
	}
	if err := s.logs.Finish(ctx, attempt.EnvironmentID, attempt.LogEpoch); err != nil {
		// The build outcome is still recorded; the next admission resets the stream.
		s.logger.WarnContext(ctx, "finish build log stream failed", "pr_id", request.PullRequestID, "generation", request.Generation, "revision", request.Revision, "error", err)
	}
}

func (s *Service) cleanup(ctx context.Context, request Request) error {
	cleanupCtx, cancel := context.WithTimeout(ctx, queue.CompletionGrace)
	defer cancel()
	resources, err := s.runtime.Resources(cleanupCtx, request.PullRequestID)
	if err != nil {
		return fmt.Errorf("discover request resources: %w", err)
	}
	if err := s.repo.ScheduleResourceCleanup(cleanupCtx, request, resources); err != nil {
		return fmt.Errorf("clean generation resources: %w", err)
	}
	return nil
}

func (s *Service) superseded(ctx context.Context, request Request) error {
	s.logger.InfoContext(ctx, "environment job superseded", "pr_id", request.PullRequestID, "generation", request.Generation, "revision", request.Revision)
	// A cleanup error is retryable: never silently forget an orphaned container.
	return s.cleanup(ctx, request)
}

// failureCoder is a classified build or runtime failure (runtime.Error).
type failureCoder interface{ FailureCode() string }

// permanentFailure is a failure that retrying the same request cannot fix
// (runtime.Error knows which of its failures are). Such a request ends after
// one attempt instead of spending River's retries.
type permanentFailure interface{ Permanent() bool }

func permanent(err error) bool {
	var p permanentFailure
	return errors.As(err, &p) && p.Permanent()
}

// upstreamFailure ends a request whose PR GitHub no longer lets the
// installation read.
type upstreamFailure struct{ err error }

func (f *upstreamFailure) Error() string       { return f.err.Error() }
func (f *upstreamFailure) Unwrap() error       { return f.err }
func (f *upstreamFailure) Permanent() bool     { return true }
func (f *upstreamFailure) FailureCode() string { return "inaccessible" }

// Build runs one admitted attempt. Reports show its failure only once the
// request is final (the job handoff marks the intent failed after the last
// River attempt), so intermediate retries never publish a final failure.
func (s *Service) Build(ctx context.Context, request Request) error {
	if err := request.validate(); err != nil {
		return err
	}
	if request.State != "open" {
		return errors.New("build requires an open PR snapshot")
	}
	before, err := s.repo.Snapshot(ctx, request.PullRequestID)
	if err != nil {
		return err
	}
	if before.Generation != request.Generation || before.State != request.State || before.HeadSHA != request.HeadSHA || (request.State == "open" && before.Revision != request.Revision) {
		return s.superseded(ctx, request)
	}
	if before.AppliedGeneration == request.Generation && before.AppliedRevision == request.Revision && before.Status == "running" {
		return s.cleanup(ctx, request)
	}
	if before.PendingGeneration == request.Generation && before.PendingRevision == request.Revision && before.PendingArtifact.ContainerID != "" && before.PendingArtifact.ImageTag != "" {
		return s.cleanup(ctx, request)
	}
	if s.verifier != nil {
		// Fresh from GitHub, outside every lock: a new commit, a close or lost
		// access recorded now supersedes this request before any work.
		snapshot, err := s.verifier.VerifyUpstream(ctx, request.PullRequestID)
		switch {
		case errors.Is(err, ErrUpstreamClosed):
			return s.superseded(ctx, request)
		case errors.Is(err, ErrUpstreamInaccessible):
			return &upstreamFailure{err: err}
		case err != nil:
			return err // unavailable, rate limited or still settling: retried
		case !snapshot.Matches(request):
			return s.superseded(ctx, request)
		}
	}
	if s.headroom != nil && before.Revision == request.Revision {
		if err := s.headroom.Admit(ctx, request); err != nil {
			if reason := waitingReason(err); reason != "" {
				return errors.Join(&WaitingError{Reason: reason, Err: err}, s.repo.markWaiting(ctx, request, reason))
			}
			return err
		}
	}
	attempt, allowed, err := s.repo.claimBuild(ctx, request, s.now())
	if err != nil {
		return err
	}
	if !allowed {
		return s.superseded(ctx, request)
	}
	buildCtx, cancel := context.WithTimeout(ctx, s.buildTimeout)
	artifact, buildErr := s.runtime.Build(buildCtx, request, attempt)
	cancel()
	if buildErr != nil && ctx.Err() != nil {
		// The worker is shutting down: this is no failure of the request. It
		// keeps its slot and runs again when a worker starts.
		completeCtx, completeCancel := context.WithTimeout(context.WithoutCancel(ctx), queue.CompletionGrace)
		defer completeCancel()
		s.finishLogs(completeCtx, request, attempt)
		return errors.Join(ErrInterrupted, s.cleanup(completeCtx, request))
	}
	if reason := waitingReason(buildErr); reason != "" {
		// Not a failure: the admitted request keeps its slot and its place,
		// and is retried once capacity or disk frees up.
		completeCtx, completeCancel := context.WithTimeout(context.WithoutCancel(ctx), queue.CompletionGrace)
		defer completeCancel()
		s.finishLogs(completeCtx, request, attempt)
		return errors.Join(&WaitingError{Reason: reason, Err: buildErr}, s.repo.markWaiting(completeCtx, request, reason))
	}
	result := Result{Status: "running", Artifact: artifact}
	if buildErr != nil {
		code := "build_failed"
		var coded failureCoder
		if errors.As(buildErr, &coded) && safeErrorCode(coded.FailureCode()) != "" {
			code = safeErrorCode(coded.FailureCode())
		}
		result = Result{Status: "failed", ErrorCode: code}
	}
	if buildErr == nil && (artifact.ContainerID == "" || artifact.ImageTag == "") {
		buildErr = errors.New("runtime returned an incomplete artifact")
		result = Result{Status: "failed", ErrorCode: "incomplete_artifact"}
	}
	// This is the pre-registration gate. Cancellation/timeouts still get a bounded
	// chance to record a CURRENT failure and clean up, never a stale final state.
	completeCtx, completeCancel := context.WithTimeout(context.WithoutCancel(ctx), queue.CompletionGrace)
	defer completeCancel()
	s.finishLogs(completeCtx, request, attempt)
	applied, err := s.repo.Apply(completeCtx, request, result)
	if err != nil {
		return errors.Join(err, s.cleanup(completeCtx, request))
	}
	if !applied {
		return s.superseded(completeCtx, request)
	}
	cleanupErr := s.cleanup(completeCtx, request)
	if buildErr != nil {
		return errors.Join(fmt.Errorf("build current generation: %w", buildErr), cleanupErr)
	}
	if cleanupErr != nil {
		// The candidate and publication operation are already durable. Retrying the
		// build could create a different artifact for the same idempotency key;
		// reconciliation can rediscover cleanup work without rebuilding it.
		s.logger.WarnContext(ctx, "post-build cleanup scheduling failed", "pr_id", request.PullRequestID, "generation", request.Generation, "revision", request.Revision, "error", cleanupErr)
	}
	return nil
}

func (s *Service) Teardown(ctx context.Context, request Request) error {
	if err := request.validate(); err != nil {
		return err
	}
	if request.State == "open" {
		return errors.New("teardown requires a closed PR snapshot")
	}
	before, err := s.repo.Snapshot(ctx, request.PullRequestID)
	if err != nil {
		return err
	}
	if before.Generation != request.Generation || before.State != request.State || before.HeadSHA != request.HeadSHA || (request.State == "open" && before.Revision != request.Revision) {
		return s.superseded(ctx, request)
	}
	if before.AppliedGeneration == request.Generation && before.Status == "torn_down" {
		return s.cleanup(ctx, request)
	}
	if before.Status == "tearing_down" && before.TeardownGeneration == request.Generation {
		return s.cleanup(ctx, request) // A retry: this teardown already started.
	}
	if before.Status == "" || before.Status == "torn_down" {
		return s.cleanup(ctx, request)
	}
	result := Result{Status: "torn_down"}
	applied, err := s.repo.Apply(ctx, request, result)
	if err != nil {
		return err
	}
	if !applied {
		return s.superseded(ctx, request)
	}
	return s.cleanup(ctx, request)
}
