package runtime

import (
	"context"
	"strconv"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// RemoveResource deletes one resource the environments cleanup contract
// scheduled. Each object is inspected by ID or reference first, and anything
// whose labels do not mark it as Previa's and as the resource's PR is refused
// (not_owned, permanent). An object that is already gone counts as removed.
//
// A container is stopped gracefully, bounded by its stop timeout (10 seconds
// for previews), then removed by force with its anonymous volumes. An image is
// never forced: one a container still uses is kept (in_use) and retried after
// that container is gone. A network is removed once no preview or builder uses
// it, after disconnecting the router and egress proxy. When a preview goes,
// the PR's preview network goes too if nothing else uses it.
func (r *Runtime) RemoveResource(ctx context.Context, resource environments.Resource) error {
	if resource.PullRequestID <= 0 {
		return failure(CodeNotOwned, "The resource names no pull request.", false, nil)
	}
	if id := resource.Artifact.ContainerID; id != "" {
		if err := r.removeOwnedContainer(ctx, resource.PullRequestID, id); err != nil {
			return err
		}
	}
	if ref := resource.Artifact.ImageTag; ref != "" {
		if err := r.removeOwnedImage(ctx, resource.PullRequestID, ref); err != nil {
			return err
		}
	}
	if id := resource.Network; id != "" {
		if err := r.removeOwnedNetwork(ctx, resource.PullRequestID, id); err != nil {
			return err
		}
	}
	if resource.Artifact.ContainerID != "" {
		if err := r.removeOwnedNetwork(ctx, resource.PullRequestID, r.networkName(resource.PullRequestID)); err != nil && CodeOf(err) != CodeInUse {
			// The inventory lists an idle network, so a later pass retries it.
			r.logger.WarnContext(ctx, "remove idle preview network", "pr_id", resource.PullRequestID, "error", err)
		}
	}
	return nil
}

func (r *Runtime) ownedByPR(labels map[string]string, pullRequestID int64, kinds ...string) bool {
	return dockerx.OwnedBy(labels, r.instance, kinds...) && labels[dockerx.LabelPR] == strconv.FormatInt(pullRequestID, 10)
}

func (r *Runtime) removeOwnedContainer(ctx context.Context, pullRequestID int64, id string) error {
	inspected, err := r.docker.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return failure(CodeUnavailable, "The container could not be inspected.", true, err)
	}
	c := inspected.Container
	if c.Config == nil || !r.ownedByPR(c.Config.Labels, pullRequestID, dockerx.KindPreview, dockerx.KindBuilder) {
		return failure(CodeNotOwned, "Refusing to remove a container that is not this pull request's.", false, nil)
	}
	if c.State != nil && (c.State.Running || c.State.Restarting) {
		// No timeout here: Docker uses the container's own stop timeout.
		if _, err := r.docker.ContainerStop(ctx, c.ID, client.ContainerStopOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
			r.logger.WarnContext(ctx, "graceful stop failed; removing by force", "pr_id", pullRequestID, "container_id", c.ID, "error", err)
		}
	}
	if _, err := r.docker.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return failure(CodeUnavailable, "The container could not be removed.", true, err)
	}
	return nil
}

func (r *Runtime) removeOwnedImage(ctx context.Context, pullRequestID int64, ref string) error {
	inspected, err := r.docker.ImageInspect(ctx, ref)
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return failure(CodeUnavailable, "The image could not be inspected.", true, err)
	}
	if inspected.Config == nil || !r.ownedByPR(inspected.Config.Labels, pullRequestID, dockerx.KindImage) {
		return failure(CodeNotOwned, "Refusing to remove an image that is not this pull request's.", false, nil)
	}
	if _, err := r.docker.ImageRemove(ctx, ref, client.ImageRemoveOptions{PruneChildren: true}); err != nil && !cerrdefs.IsNotFound(err) {
		if cerrdefs.IsConflict(err) {
			return failure(CodeInUse, "The image is still used by a container.", true, err)
		}
		return failure(CodeUnavailable, "The image could not be removed.", true, err)
	}
	return nil
}

// removeOwnedNetwork removes a PR's preview or build network once no preview
// or builder is attached; Previa's router and egress proxy are disconnected,
// and so is anything else that joined the network.
func (r *Runtime) removeOwnedNetwork(ctx context.Context, pullRequestID int64, id string) error {
	inspected, err := r.docker.NetworkInspect(ctx, id, client.NetworkInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return failure(CodeUnavailable, "The network could not be inspected.", true, err)
	}
	n := inspected.Network
	if !r.ownedByPR(n.Labels, pullRequestID, dockerx.KindPreviewNetwork, dockerx.KindBuildNetwork) {
		return failure(CodeNotOwned, "Refusing to remove a network that is not this pull request's.", false, nil)
	}
	for containerID := range n.Containers {
		attached, err := r.docker.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
		if err == nil && attached.Container.Config != nil && dockerx.Owned(attached.Container.Config.Labels, dockerx.KindPreview, dockerx.KindBuilder) {
			return failure(CodeInUse, "The network is still used by a preview or builder.", true, nil)
		}
		if err != nil && !cerrdefs.IsNotFound(err) {
			return failure(CodeUnavailable, "A container on the network could not be inspected.", true, err)
		}
	}
	for containerID := range n.Containers {
		if _, err := r.docker.NetworkDisconnect(ctx, n.ID, client.NetworkDisconnectOptions{Container: containerID, Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
			return failure(CodeUnavailable, "A container could not be disconnected from the network.", true, err)
		}
	}
	if _, err := r.docker.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
		return failure(CodeUnavailable, "The network could not be removed.", true, err)
	}
	return nil
}

// CleanupExecutor performs cleanup operations for the environments operation
// worker. It needs Docker only: no GitHub token, so cleanup survives revoked
// access.
func (r *Runtime) CleanupExecutor() environments.OperationExecutor {
	return cleanupExecutor{runtime: r}
}

type cleanupExecutor struct{ runtime *Runtime }

func (c cleanupExecutor) Execute(ctx context.Context, op environments.Operation) error {
	if op.Kind != environments.CleanupArtifact || op.Payload.Resource.PullRequestID != op.PullRequestID {
		return &environments.OperationFailure{Code: "invalid_operation", Permanent: true}
	}
	return AsOperationFailure(c.runtime.RemoveResource(ctx, op.Payload.Resource))
}
