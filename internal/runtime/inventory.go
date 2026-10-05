package runtime

import (
	"context"
	"fmt"
	"strconv"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// Resources lists everything Docker holds for a PR, including leftovers of
// crashed attempts: preview containers with their images, images no
// container uses, builder containers, build networks, and the PR's preview
// network once no preview uses it. Ownership comes from labels set at
// creation. Build caches are per source repository and pruned by housekeeping.
func (r *Runtime) Resources(ctx context.Context, pullRequestID int64) ([]environments.Resource, error) {
	filter := client.Filters{}.Add("label", dockerx.LabelManaged+"=true", dockerx.LabelPR+"="+strconv.FormatInt(pullRequestID, 10))
	containers, err := r.docker.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filter})
	if err != nil {
		return nil, fmt.Errorf("list preview containers: %w", err)
	}
	var found []environments.Resource
	used := map[string]bool{}
	previews := false
	for _, c := range containers.Items {
		switch c.Labels[dockerx.LabelKind] {
		case dockerx.KindPreview:
			previews = true
			used[c.Labels[dockerx.LabelImage]] = true
			found = append(found, resource(pullRequestID, c.Labels, environments.Artifact{ContainerID: c.ID, ImageTag: c.Labels[dockerx.LabelImage]}))
		case dockerx.KindBuilder:
			found = append(found, resource(pullRequestID, c.Labels, environments.Artifact{ContainerID: c.ID}))
		}
	}
	networks, err := r.docker.NetworkList(ctx, client.NetworkListOptions{Filters: filter})
	if err != nil {
		return nil, fmt.Errorf("list preview networks: %w", err)
	}
	for _, n := range networks.Items {
		switch n.Labels[dockerx.LabelKind] {
		case dockerx.KindBuildNetwork:
			network := resource(pullRequestID, n.Labels, environments.Artifact{})
			network.Network = n.ID
			found = append(found, network)
		case dockerx.KindPreviewNetwork:
			if !previews {
				found = append(found, environments.Resource{PullRequestID: pullRequestID, Network: n.ID})
			}
		}
	}
	images, err := r.docker.ImageList(ctx, client.ImageListOptions{Filters: filter})
	if err != nil {
		return nil, fmt.Errorf("list preview images: %w", err)
	}
	for _, i := range images.Items {
		if i.Labels[dockerx.LabelKind] != dockerx.KindImage {
			continue
		}
		for _, tag := range i.RepoTags {
			if !used[tag] {
				found = append(found, resource(pullRequestID, i.Labels, environments.Artifact{ImageTag: tag}))
			}
		}
	}
	return found, nil
}

func resource(pullRequestID int64, labels map[string]string, artifact environments.Artifact) environments.Resource {
	generation, _ := strconv.ParseInt(labels[dockerx.LabelGeneration], 10, 64)
	revision, _ := strconv.ParseInt(labels[dockerx.LabelRevision], 10, 64)
	return environments.Resource{PullRequestID: pullRequestID, Generation: generation, Revision: revision, Artifact: artifact}
}

// RemoveContainer deletes a preview or builder container. It verifies Previa's
// ownership labels first and refuses anything else; a container that is
// already gone counts as removed.
func (r *Runtime) RemoveContainer(ctx context.Context, id string) error {
	inspected, err := r.docker.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return failure(CodeUnavailable, "The container could not be inspected.", true, err)
	}
	if inspected.Container.Config == nil || !dockerx.Owned(inspected.Container.Config.Labels, dockerx.KindPreview, dockerx.KindBuilder) {
		return failure(CodeNotOwned, "Refusing to remove a container Previa did not create.", false, nil)
	}
	if _, err := r.docker.ContainerRemove(ctx, inspected.Container.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return failure(CodeUnavailable, "The container could not be removed.", true, err)
	}
	return nil
}

// RemoveImage deletes a built image by reference after verifying Previa's
// labels. It never forces: an image a container still uses is kept and the
// removal is retried after that container is gone. A missing image counts as
// removed.
func (r *Runtime) RemoveImage(ctx context.Context, ref string) error {
	inspected, err := r.docker.ImageInspect(ctx, ref)
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return failure(CodeUnavailable, "The image could not be inspected.", true, err)
	}
	if inspected.Config == nil || !dockerx.Owned(inspected.Config.Labels, dockerx.KindImage) {
		return failure(CodeNotOwned, "Refusing to remove an image Previa did not build.", false, nil)
	}
	if _, err := r.docker.ImageRemove(ctx, ref, client.ImageRemoveOptions{PruneChildren: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return failure(CodeUnavailable, "The image could not be removed; a container may still use it.", true, err)
	}
	return nil
}
