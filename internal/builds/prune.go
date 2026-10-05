package builds

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// Cache is one build cache volume: a base repository and the repository its
// code came from (see cacheScope).
type Cache struct {
	Name                   string
	BaseRepoID, HeadRepoID int64
	Size                   int64 // bytes; -1 when Docker cannot tell
	CreatedAt              time.Time
	InUse                  bool // a builder holds it now
}

// Caches lists this instance's build cache volumes, oldest first, with their
// sizes from Docker's disk usage.
func (b *Builder) Caches(ctx context.Context) ([]Cache, error) {
	usage, err := b.docker.DiskUsage(ctx, client.DiskUsageOptions{Volumes: true, Verbose: true})
	if err != nil {
		return nil, fmt.Errorf("read volume disk usage: %w", err)
	}
	var caches []Cache
	for _, v := range usage.Volumes.Items {
		if !dockerx.OwnedBy(v.Labels, b.cfg.Instance, dockerx.KindBuildCache) {
			continue
		}
		c := Cache{Name: v.Name, Size: -1}
		c.BaseRepoID, _ = strconv.ParseInt(v.Labels[dockerx.LabelBaseRepo], 10, 64)
		c.HeadRepoID, _ = strconv.ParseInt(v.Labels[dockerx.LabelHeadRepo], 10, 64)
		c.CreatedAt, _ = time.Parse(time.RFC3339, v.CreatedAt)
		if v.UsageData != nil {
			c.Size, c.InUse = v.UsageData.Size, v.UsageData.RefCount > 0
		}
		caches = append(caches, c)
	}
	sort.Slice(caches, func(i, j int) bool { return caches[i].CreatedAt.Before(caches[j].CreatedAt) })
	return caches, nil
}

// RemoveCache deletes one of this instance's cache volumes. A volume a builder
// uses is never forced; Docker refuses and the next prune tries again.
func (b *Builder) RemoveCache(ctx context.Context, name string) error {
	inspected, err := b.docker.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect build cache: %w", err)
	}
	if !dockerx.OwnedBy(inspected.Volume.Labels, b.cfg.Instance, dockerx.KindBuildCache) {
		return fmt.Errorf("refusing to remove volume %s: not this instance's build cache", name)
	}
	if _, err := b.docker.VolumeRemove(ctx, name, client.VolumeRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove build cache: %w", err)
	}
	return nil
}

// DanglingImages lists this instance's built images that lost their tag (an
// attempt for the same revision re-tagged its result).
func (b *Builder) DanglingImages(ctx context.Context) ([]string, error) {
	listed, err := b.docker.ImageList(ctx, client.ImageListOptions{Filters: client.Filters{}.
		Add("dangling", "true").
		Add("label", dockerx.LabelManaged+"=true", dockerx.LabelInstance+"="+b.cfg.Instance, dockerx.LabelKind+"="+dockerx.KindImage)})
	if err != nil {
		return nil, fmt.Errorf("list dangling images: %w", err)
	}
	ids := make([]string, 0, len(listed.Items))
	for _, i := range listed.Items {
		ids = append(ids, i.ID)
	}
	return ids, nil
}

// RemoveDanglingImage deletes one untagged image after checking its labels.
// It never forces: an image a container still runs is kept.
func (b *Builder) RemoveDanglingImage(ctx context.Context, id string) error {
	inspected, err := b.docker.ImageInspect(ctx, id)
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect dangling image: %w", err)
	}
	if inspected.Config == nil || !dockerx.OwnedBy(inspected.Config.Labels, b.cfg.Instance, dockerx.KindImage) || len(inspected.RepoTags) > 0 {
		return fmt.Errorf("refusing to remove image %s: not an untagged image of this instance", id)
	}
	if _, err := b.docker.ImageRemove(ctx, id, client.ImageRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove dangling image: %w", err)
	}
	return nil
}
