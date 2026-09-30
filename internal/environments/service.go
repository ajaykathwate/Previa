package environments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/queue"
)

// Runtime is the boundary for the unbuilt Docker integration. Build must return
// an unregistered healthy container, honor cancellation, and label containers AND
// uniquely tagged images with PR ID/generation. Resources must also discover
// partial builds after process crashes. Removal is idempotent (not-found succeeds).
type Runtime interface {
	Build(context.Context, Request) (Artifact, error)
	Resources(context.Context, int64) ([]Resource, error)
	RemoveContainer(context.Context, string) error
	RemoveImage(context.Context, string) error
}

type Resource struct {
	PullRequestID, Generation int64
	Artifact                  Artifact
}

// Publisher is the boundary for routing/reporting, not a Docker/GitHub adapter.
// Publish is called under the PR row lock, only for a current result. It must be
// idempotent, preserve the previous route for failed builds, and compensate any
// partial route change when returning an error. It must honor context deadlines.
type Publisher interface {
	Publish(context.Context, Request, Result, Artifact) error
}

type Service struct {
	repo         *Repository
	runtime      Runtime
	publisher    Publisher
	logger       *slog.Logger
	buildTimeout time.Duration
}

func NewService(repo *Repository, runtime Runtime, publisher Publisher, logger *slog.Logger, buildTimeout time.Duration) (*Service, error) {
	if repo == nil || runtime == nil || publisher == nil || logger == nil {
		return nil, errors.New("environment service requires repository, runtime, publisher, and logger")
	}
	if buildTimeout == 0 {
		buildTimeout = queue.DefaultBuildTimeout
	}
	if buildTimeout < 0 || buildTimeout > 24*time.Hour {
		return nil, errors.New("invalid build timeout")
	}
	return &Service{repo: repo, runtime: runtime, publisher: publisher, logger: logger, buildTimeout: buildTimeout}, nil
}

func (s *Service) cleanup(ctx context.Context, request Request) error {
	cleanupCtx, cancel := context.WithTimeout(ctx, queue.CompletionGrace)
	defer cancel()
	if err := s.repo.Cleanup(cleanupCtx, request, s.removeSupersededResources); err != nil {
		return fmt.Errorf("clean generation resources: %w", err)
	}
	return nil
}

func (s *Service) superseded(ctx context.Context, request Request) error {
	s.logger.InfoContext(ctx, "environment job superseded", "pr_id", request.PullRequestID, "generation", request.Generation)
	// A cleanup error is retryable: never silently forget an orphaned container.
	return s.cleanup(ctx, request)
}

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
	if before.Generation != request.Generation {
		return s.superseded(ctx, request)
	}
	if before.AppliedGeneration == request.Generation && before.Status == "running" {
		return s.cleanup(ctx, request)
	}
	buildCtx, cancel := context.WithTimeout(ctx, s.buildTimeout)
	artifact, buildErr := s.runtime.Build(buildCtx, request)
	cancel()
	result := Result{Status: "running", Artifact: artifact}
	if buildErr != nil {
		result = Result{Status: "failed"}
	}
	if buildErr == nil && (artifact.ContainerID == "" || artifact.ImageTag == "") {
		buildErr = errors.New("runtime returned an incomplete artifact")
		result = Result{Status: "failed"}
	}
	// This is the pre-registration gate. Cancellation/timeouts still get a bounded
	// chance to record a CURRENT failure and clean up, never a stale final state.
	completeCtx, completeCancel := context.WithTimeout(context.WithoutCancel(ctx), queue.CompletionGrace)
	defer completeCancel()
	applied, err := s.repo.Apply(completeCtx, request, result, func(ctx context.Context, previous Artifact) error {
		return s.publisher.Publish(ctx, request, result, previous)
	})
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
	return cleanupErr
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
	if before.Generation != request.Generation {
		return s.superseded(ctx, request)
	}
	if before.AppliedGeneration == request.Generation && before.Status == "torn_down" {
		return s.cleanup(ctx, request)
	}
	result := Result{Status: "torn_down"}
	applied, err := s.repo.Apply(ctx, request, result, func(ctx context.Context, previous Artifact) error {
		return s.publisher.Publish(ctx, request, result, previous)
	})
	if err != nil {
		return err
	}
	if !applied {
		return s.superseded(ctx, request)
	}
	return s.cleanup(ctx, request)
}
