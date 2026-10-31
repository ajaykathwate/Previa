package environments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/metrics"
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
	UsageKey      string
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
	paused       bool
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

// Paused is the execution kill switch (BUILDS_PAUSED): no build is admitted
// and no GitHub or Docker call is made for one; requests wait with reason
// paused and run once a worker starts unpaused. Teardown and every other
// operation are unaffected.
func (s *Service) Paused() *Service {
	s.paused = true
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
	if request.State != StateOpen {
		return errors.New("build requires an open PR snapshot")
	}
	if proceed, err := s.admit(ctx, request); !proceed {
		return err
	}
	attempt, allowed, err := s.repo.claimBuild(ctx, request, s.now())
	if err != nil {
		return err
	}
	if !allowed {
		return s.superseded(ctx, request)
	}
	started := s.now()
	buildCtx, cancel := context.WithTimeout(ctx, s.buildTimeout)
	var artifact Artifact
	var buildErr error
	if s.repo.usage != nil {
		buildErr = s.repo.usage.Run(buildCtx, attempt.UsageKey, func(execCtx context.Context) error {
			var err error
			artifact, err = s.runtime.Build(execCtx, request, attempt)
			return err
		})
	} else {
		artifact, buildErr = s.runtime.Build(buildCtx, request, attempt)
	}
	cancel()
	// This is the pre-registration gate. Cancellation/timeouts still get a bounded
	// chance to record a CURRENT failure and clean up, never a stale final state.
	completeCtx, completeCancel := context.WithTimeout(context.WithoutCancel(ctx), queue.CompletionGrace)
	defer completeCancel()
	if buildErr != nil && ctx.Err() != nil {
		metrics.BuildFinished("interrupted", "", s.now().Sub(started))
		// The worker is shutting down: this is no failure of the request. It
		// keeps its slot and runs again when a worker starts.
		s.finishLogs(completeCtx, request, attempt)
		return errors.Join(ErrInterrupted, s.cleanup(completeCtx, request))
	}
	s.finishLogs(completeCtx, request, attempt)
	if reason := waitingReason(buildErr); reason != "" {
		if usageWait(reason) {
			return errors.Join(s.wait(completeCtx, request, reason, buildErr), s.cleanup(completeCtx, request))
		}
		// Not a failure: the admitted request keeps its slot and its place,
		// and is retried once capacity or disk frees up.
		return s.wait(completeCtx, request, reason, buildErr)
	}
	return s.record(ctx, completeCtx, request, started, artifact, buildErr)
}

// admit runs the checks a build passes before it claims a slot. When the
// request ends here it returns false with what Build returns: superseded or
// already built (cleanup only), paused or short of capacity (waiting), or no
// longer readable on GitHub.
func (s *Service) admit(ctx context.Context, request Request) (bool, error) {
	before, err := s.repo.Snapshot(ctx, request.PullRequestID)
	if err != nil {
		return false, err
	}
	if !before.matches(request) {
		return false, s.superseded(ctx, request)
	}
	if before.built(request) {
		return false, s.cleanup(ctx, request)
	}
	if s.paused {
		return false, s.wait(ctx, request, WaitPaused, errors.New("builds are paused"))
	}
	if s.verifier != nil {
		if current, err := s.verifyUpstream(ctx, request); !current {
			return false, err
		}
	}
	if s.headroom != nil && before.Revision == request.Revision {
		if err := s.headroom.Admit(ctx, request); err != nil {
			if reason := waitingReason(err); reason != "" {
				return false, s.wait(ctx, request, reason, err)
			}
			return false, err
		}
	}
	return true, nil
}

// verifyUpstream reads the PR fresh from GitHub, outside every lock: a new
// commit, a close or lost access recorded now supersedes this request before
// any work. It returns false with what Build returns when it does.
func (s *Service) verifyUpstream(ctx context.Context, request Request) (bool, error) {
	snapshot, err := s.verifier.VerifyUpstream(ctx, request.PullRequestID)
	switch {
	case errors.Is(err, ErrUpstreamClosed):
		return false, s.superseded(ctx, request)
	case errors.Is(err, ErrUpstreamInaccessible):
		return false, &upstreamFailure{err: err}
	case err != nil:
		return false, err // unavailable, rate limited or still settling: retried
	case !snapshot.Matches(request):
		return false, s.superseded(ctx, request)
	}
	return true, nil
}

// wait records that the request waits for reason and returns the
// WaitingError that retries it without spending an attempt.
func (s *Service) wait(ctx context.Context, request Request, reason string, cause error) error {
	metrics.BuildWaited(reason)
	var retry interface{ RetryAt() time.Time }
	var until time.Time
	if errors.As(cause, &retry) {
		until = retry.RetryAt()
	}
	return errors.Join(&WaitingError{Reason: reason, Err: cause, Until: until}, s.repo.markWaiting(ctx, request, reason, until))
}

// record applies an attempt's outcome, then schedules cleanup of whatever the
// request no longer needs.
func (s *Service) record(ctx, completeCtx context.Context, request Request, started time.Time, artifact Artifact, buildErr error) error {
	result, buildErr := buildResult(artifact, buildErr)
	applied, err := s.repo.Apply(completeCtx, request, result)
	if err != nil {
		return errors.Join(err, s.cleanup(completeCtx, request))
	}
	if !applied {
		metrics.BuildFinished("superseded", "", s.now().Sub(started))
		return s.superseded(completeCtx, request)
	}
	if result.Status == StatusFailed {
		metrics.BuildFinished("failed", result.ErrorCode, s.now().Sub(started))
	} else {
		metrics.BuildFinished("succeeded", "", s.now().Sub(started))
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

// buildResult is the environment result for a build's outcome. An artifact
// without both a container and an image is a failure too.
func buildResult(artifact Artifact, buildErr error) (Result, error) {
	if buildErr != nil {
		code := "build_failed"
		var coded failureCoder
		if errors.As(buildErr, &coded) && safeErrorCode(coded.FailureCode()) != "" {
			code = safeErrorCode(coded.FailureCode())
		}
		return Result{Status: StatusFailed, ErrorCode: code}, buildErr
	}
	if artifact.ContainerID == "" || artifact.ImageTag == "" {
		return Result{Status: StatusFailed, ErrorCode: "incomplete_artifact"}, errors.New("runtime returned an incomplete artifact")
	}
	return Result{Status: StatusRunning, Artifact: artifact}, nil
}

func (s *Service) Teardown(ctx context.Context, request Request) error {
	if err := request.validate(); err != nil {
		return err
	}
	if request.State == StateOpen {
		return errors.New("teardown requires a closed PR snapshot")
	}
	before, err := s.repo.Snapshot(ctx, request.PullRequestID)
	if err != nil {
		return err
	}
	if !before.matches(request) {
		return s.superseded(ctx, request)
	}
	if before.AppliedGeneration == request.Generation && before.Status == StatusTornDown {
		return s.cleanup(ctx, request)
	}
	if before.Status == StatusTearingDown && before.TeardownGeneration == request.Generation {
		return s.cleanup(ctx, request) // A retry: this teardown already started.
	}
	if before.Status == "" || before.Status == StatusTornDown {
		return s.cleanup(ctx, request)
	}
	result := Result{Status: StatusTornDown}
	applied, err := s.repo.Apply(ctx, request, result)
	if err != nil {
		return err
	}
	if !applied {
		return s.superseded(ctx, request)
	}
	return s.cleanup(ctx, request)
}
