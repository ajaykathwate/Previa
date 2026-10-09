package runtime

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// Resources lists everything Docker holds for a PR, including leftovers of
// crashed attempts: candidates (an app container with its image, or the
// companions a candidate left before its app existed), images no container
// uses, builder containers, build networks, and the PR's preview and services
// networks once no candidate uses them. A candidate is one resource: its
// app's artifact stands for its managed services, setup and processes, which
// are referenced, kept and removed with it. Ownership comes from labels set
// at creation. Build caches are per source repository and pruned by
// housekeeping.
func (r *Runtime) Resources(ctx context.Context, pullRequestID int64) ([]environments.Resource, error) {
	filter := client.Filters{}.Add("label", dockerx.LabelManaged+"=true", dockerx.LabelInstance+"="+r.instance, dockerx.LabelPR+"="+strconv.FormatInt(pullRequestID, 10))
	inv := &prInventory{pullRequestID: pullRequestID, used: map[string]bool{}}
	if err := r.inventoryContainers(ctx, filter, inv); err != nil {
		return nil, err
	}
	if err := r.inventoryNetworks(ctx, filter, inv); err != nil {
		return nil, err
	}
	if err := r.inventoryImages(ctx, filter, inv); err != nil {
		return nil, err
	}
	return inv.found, nil
}

// prInventory collects one PR's resources while Resources lists Docker.
type prInventory struct {
	pullRequestID int64
	found         []environments.Resource
	used          map[string]bool // image tags a candidate container uses
	candidates    bool            // whether any candidate container exists
}

func (inv *prInventory) add(labels map[string]string, artifact environments.Artifact, created time.Time) {
	inv.found = append(inv.found, resource(inv.pullRequestID, labels, artifact, created))
}

// inventoryContainers lists candidates (each app with its companions) and
// builder containers.
func (r *Runtime) inventoryContainers(ctx context.Context, filter client.Filters, inv *prInventory) error {
	containers, err := r.docker.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filter})
	if err != nil {
		return fmt.Errorf("list preview containers: %w", err)
	}
	apps := map[string]int{}                       // candidate key -> index of its resource in found
	companions := map[string][]container.Summary{} // candidate key -> its companion containers
	for _, c := range containers.Items {
		switch c.Labels[dockerx.LabelKind] {
		case dockerx.KindPreview:
			inv.candidates = true
			inv.used[c.Labels[dockerx.LabelImage]] = true
			inv.add(c.Labels, environments.Artifact{ContainerID: c.ID, ImageTag: c.Labels[dockerx.LabelImage]}, time.Unix(c.Created, 0))
			if key := c.Labels[dockerx.LabelCandidate]; key != "" {
				apps[key] = len(inv.found) - 1
			}
		case dockerx.KindService, dockerx.KindSetup, dockerx.KindProcess:
			inv.candidates = true
			if image := c.Labels[dockerx.LabelImage]; image != "" {
				inv.used[image] = true
			}
			key := c.Labels[dockerx.LabelCandidate]
			if key == "" {
				key = c.ID // a companion without a key stands alone
			}
			companions[key] = append(companions[key], c)
		case dockerx.KindBuilder:
			inv.add(c.Labels, environments.Artifact{ContainerID: c.ID}, time.Unix(c.Created, 0))
		}
	}
	for key, group := range companions {
		inv.addCompanions(apps, key, group)
	}
	return nil
}

// addCompanions accounts for one candidate's companions. With its app, the
// candidate is as young as its youngest container. Without one (the app was
// never created, or is gone) the leftovers are one resource, named by one of
// them; removing it removes them all.
func (inv *prInventory) addCompanions(apps map[string]int, key string, group []container.Summary) {
	newest := time.Time{}
	for _, c := range group {
		if created := time.Unix(c.Created, 0); created.After(newest) {
			newest = created
		}
	}
	if at, ok := apps[key]; ok {
		if newest.After(inv.found[at].CreatedAt) {
			inv.found[at].CreatedAt = newest
		}
		return
	}
	inv.add(group[0].Labels, environments.Artifact{ContainerID: group[0].ID}, newest)
}

// inventoryNetworks lists build networks, and the PR's preview and services
// networks once no candidate uses them.
func (r *Runtime) inventoryNetworks(ctx context.Context, filter client.Filters, inv *prInventory) error {
	networks, err := r.docker.NetworkList(ctx, client.NetworkListOptions{Filters: filter})
	if err != nil {
		return fmt.Errorf("list preview networks: %w", err)
	}
	for _, n := range networks.Items {
		switch n.Labels[dockerx.LabelKind] {
		case dockerx.KindBuildNetwork:
			network := resource(inv.pullRequestID, n.Labels, environments.Artifact{}, n.Created)
			network.Network = n.ID
			inv.found = append(inv.found, network)
		case dockerx.KindPreviewNetwork, dockerx.KindServiceNetwork:
			if !inv.candidates {
				inv.found = append(inv.found, environments.Resource{PullRequestID: inv.pullRequestID, Network: n.ID, CreatedAt: n.Created})
			}
		}
	}
	return nil
}

// inventoryImages lists the PR's built images no candidate uses.
func (r *Runtime) inventoryImages(ctx context.Context, filter client.Filters, inv *prInventory) error {
	images, err := r.docker.ImageList(ctx, client.ImageListOptions{Filters: filter})
	if err != nil {
		return fmt.Errorf("list preview images: %w", err)
	}
	for _, i := range images.Items {
		if i.Labels[dockerx.LabelKind] != dockerx.KindImage {
			continue
		}
		for _, tag := range i.RepoTags {
			if !inv.used[tag] {
				inv.add(i.Labels, environments.Artifact{ImageTag: tag}, time.Unix(i.Created, 0))
			}
		}
	}
	return nil
}

func resource(pullRequestID int64, labels map[string]string, artifact environments.Artifact, created time.Time) environments.Resource {
	generation, _ := strconv.ParseInt(labels[dockerx.LabelGeneration], 10, 64)
	revision, _ := strconv.ParseInt(labels[dockerx.LabelRevision], 10, 64)
	return environments.Resource{PullRequestID: pullRequestID, Generation: generation, Revision: revision, Artifact: artifact, CreatedAt: created}
}

// PullRequests lists the PRs this instance's containers, images and networks
// belong to, with the newest creation time among each PR's objects.
// Housekeeping uses it to find leftovers of PRs the database no longer has.
func (r *Runtime) PullRequests(ctx context.Context) (map[int64]time.Time, error) {
	filter := client.Filters{}.Add("label", dockerx.LabelManaged+"=true", dockerx.LabelInstance+"="+r.instance)
	found := map[int64]time.Time{}
	note := func(labels map[string]string, created time.Time) {
		id, err := strconv.ParseInt(labels[dockerx.LabelPR], 10, 64)
		if err != nil || id <= 0 {
			return
		}
		if created.After(found[id]) {
			found[id] = created
		}
	}
	containers, err := r.docker.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filter})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	for _, c := range containers.Items {
		note(c.Labels, time.Unix(c.Created, 0))
	}
	images, err := r.docker.ImageList(ctx, client.ImageListOptions{Filters: filter})
	if err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}
	for _, i := range images.Items {
		note(i.Labels, time.Unix(i.Created, 0))
	}
	networks, err := r.docker.NetworkList(ctx, client.NetworkListOptions{Filters: filter})
	if err != nil {
		return nil, fmt.Errorf("list networks: %w", err)
	}
	for _, n := range networks.Items {
		note(n.Labels, n.Created)
	}
	return found, nil
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
	if inspected.Container.Config == nil || !dockerx.OwnedBy(inspected.Container.Config.Labels, r.instance, append([]string{dockerx.KindBuilder}, dockerx.CandidateKinds...)...) {
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
	if inspected.Config == nil || !dockerx.OwnedBy(inspected.Config.Labels, r.instance, dockerx.KindImage) {
		return failure(CodeNotOwned, "Refusing to remove an image Previa did not build.", false, nil)
	}
	if _, err := r.docker.ImageRemove(ctx, ref, client.ImageRemoveOptions{PruneChildren: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return failure(CodeUnavailable, "The image could not be removed; a container may still use it.", true, err)
	}
	return nil
}
