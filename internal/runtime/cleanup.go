package runtime

import (
	"context"
	"slices"
	"strconv"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/platform/egress"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// RemoveResource deletes one resource the environments cleanup contract
// scheduled. Each object is inspected by ID or reference first, and anything
// whose labels do not mark it as Previa's and as the resource's PR is refused
// (not_owned, permanent). An object that is already gone counts as removed.
//
// A container is stopped gracefully, bounded by its stop timeout (10 seconds
// for previews), then removed by force with its anonymous volumes. A
// candidate's container takes the whole candidate with it: its app and its
// managed services, setup and processes. An image is never forced: one a
// container still uses is kept (in_use) and retried after that container is
// gone. A network is removed once no candidate container or builder uses it,
// after disconnecting the router and egress proxy. When a candidate goes, the
// PR's preview and services networks go too if nothing else uses them.
func (r *Runtime) RemoveResource(ctx context.Context, resource environments.Resource) error {
	if resource.PullRequestID <= 0 {
		return failure(CodeNotOwned, "The resource names no pull request.", false, nil)
	}
	if id := resource.Artifact.ContainerID; id != "" {
		if err := r.removeCandidate(ctx, resource.PullRequestID, id); err != nil {
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
		for _, name := range []string{r.networkName(resource.PullRequestID), r.servicesNetworkName(resource.PullRequestID)} {
			if err := r.removeOwnedNetwork(ctx, resource.PullRequestID, name); err != nil && CodeOf(err) != CodeInUse {
				// The inventory lists an idle network, so a later pass retries it.
				r.logger.WarnContext(ctx, "remove idle preview network", "pr_id", resource.PullRequestID, "error", err)
			}
		}
	}
	return nil
}

// removeCandidate removes a container and, when it belongs to a candidate,
// every other container of that candidate.
func (r *Runtime) removeCandidate(ctx context.Context, pullRequestID int64, id string) error {
	inspected, err := r.docker.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return failure(CodeUnavailable, "The container could not be inspected.", true, err)
	}
	c := inspected.Container
	if c.Config == nil || !r.ownedByPR(c.Config.Labels, pullRequestID, append([]string{dockerx.KindBuilder}, dockerx.CandidateKinds...)...) {
		return failure(CodeNotOwned, "Refusing to remove a container that is not this pull request's.", false, nil)
	}
	if key := c.Config.Labels[dockerx.LabelCandidate]; key != "" {
		if err := r.removeCandidateByKey(ctx, pullRequestID, key); err != nil {
			return err
		}
	}
	return r.removeOwnedContainer(ctx, pullRequestID, c.ID)
}

// removeCandidateByKey removes every container of one candidate of this PR:
// its app first, so nothing it serves outlives its dependencies' removal
// unnoticed, then its processes, setup and services.
func (r *Runtime) removeCandidateByKey(ctx context.Context, pullRequestID int64, key string) error {
	listed, err := r.docker.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: client.Filters{}.Add("label",
		dockerx.LabelManaged+"=true", dockerx.LabelInstance+"="+r.instance, dockerx.LabelPR+"="+strconv.FormatInt(pullRequestID, 10),
		dockerx.LabelCandidate+"="+key)})
	if err != nil {
		return failure(CodeUnavailable, "The preview's containers could not be listed.", true, err)
	}
	order := map[string]int{dockerx.KindPreview: 0, dockerx.KindProcess: 1, dockerx.KindSetup: 2, dockerx.KindService: 3}
	items := listed.Items
	slices.SortStableFunc(items, func(a, b container.Summary) int {
		return order[a.Labels[dockerx.LabelKind]] - order[b.Labels[dockerx.LabelKind]]
	})
	for _, c := range items {
		if err := r.removeOwnedContainer(ctx, pullRequestID, c.ID); err != nil {
			return err
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
	if c.Config == nil || !r.ownedByPR(c.Config.Labels, pullRequestID, append([]string{dockerx.KindBuilder}, dockerx.CandidateKinds...)...) {
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

// removeOwnedNetwork removes a PR's preview, services or build network once
// no candidate container or builder is attached; Previa's router and egress
// proxy are disconnected (the proxy's approvals there withdrawn first), and so
// is anything else that joined the network.
func (r *Runtime) removeOwnedNetwork(ctx context.Context, pullRequestID int64, id string) error {
	inspected, err := r.docker.NetworkInspect(ctx, id, client.NetworkInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return failure(CodeUnavailable, "The network could not be inspected.", true, err)
	}
	n := inspected.Network
	if !r.ownedByPR(n.Labels, pullRequestID, dockerx.KindPreviewNetwork, dockerx.KindServiceNetwork, dockerx.KindBuildNetwork) {
		return failure(CodeNotOwned, "Refusing to remove a network that is not this pull request's.", false, nil)
	}
	for containerID := range n.Containers {
		attached, err := r.docker.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
		if err == nil && attached.Container.Config != nil && dockerx.Owned(attached.Container.Config.Labels, append([]string{dockerx.KindBuilder}, dockerx.CandidateKinds...)...) {
			return failure(CodeInUse, "The network is still used by a preview or builder.", true, nil)
		}
		if err != nil && !cerrdefs.IsNotFound(err) {
			return failure(CodeUnavailable, "A container on the network could not be inspected.", true, err)
		}
	}
	if r.egress != nil {
		// Withdraw approvals before the proxy's address here can be reused.
		if err := r.egress.Detach(ctx, n.ID); err != nil {
			return failure(CodeUnavailable, "The egress proxy could not leave the network.", true, err)
		}
	}
	for containerID, endpoint := range n.Containers {
		if r.egress != nil && endpoint.Name == egress.ContainerName {
			continue // Detached above.
		}
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
