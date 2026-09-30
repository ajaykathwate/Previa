package environments

import (
	"context"
	"errors"
	"fmt"
)

// removeSupersededResources runs with the PR row locked, so keep cannot change
// underneath deletion. Labels bound deletion to this PR and to old generations;
// newer builds and the currently referenced preview are always preserved.
func (s *Service) removeSupersededResources(ctx context.Context, request Request, keep Artifact) error {
	resources, err := s.runtime.Resources(ctx, request.PullRequestID)
	if err != nil {
		return fmt.Errorf("list generation resources: %w", err)
	}
	var resultErr error
	for _, resource := range resources {
		if resource.PullRequestID != request.PullRequestID || resource.Generation <= 0 || resource.Generation > request.Generation {
			continue
		}
		a := resource.Artifact
		if a.ContainerID != "" && a.ContainerID == keep.ContainerID {
			continue
		}
		if a.ContainerID != "" {
			if err := s.runtime.RemoveContainer(ctx, a.ContainerID); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("remove superseded container: %w", err))
				continue // Do not remove an image while its container may still exist.
			}
		}
		if a.ImageTag != "" && a.ImageTag != keep.ImageTag {
			if err := s.runtime.RemoveImage(ctx, a.ImageTag); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("remove superseded image: %w", err))
			}
		}
	}
	return resultErr
}
