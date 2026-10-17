package builds

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

var errImageTooLarge = errors.New("image exceeds the size limit")

// imageLoader streams BuildKit's docker-format export into the Docker image
// store, failing once more than limit bytes arrive.
type imageLoader struct {
	docker *client.Client
	ctx    context.Context
	limit  int64

	once sync.Once
	done chan error
}

func newImageLoader(ctx context.Context, docker *client.Client, limit int64) *imageLoader {
	return &imageLoader{docker: docker, ctx: ctx, limit: limit, done: make(chan error, 1)}
}

// output is BuildKit's FileOutputFunc. The load starts only if BuildKit
// reaches the export, after every build step succeeded.
func (l *imageLoader) output(map[string]string) (io.WriteCloser, error) {
	var out io.WriteCloser
	l.once.Do(func() {
		reader, writer := io.Pipe()
		go func() {
			err := l.load(reader)
			// Unblock BuildKit if Docker stopped reading early.
			reader.CloseWithError(errors.Join(err, io.ErrClosedPipe))
			l.done <- err
		}()
		out = &cappedWriter{w: writer, limit: l.limit}
	})
	if out == nil {
		return nil, errors.New("image export already started")
	}
	return out, nil
}

func (l *imageLoader) load(r io.Reader) error {
	res, err := l.docker.ImageLoad(l.ctx, r, client.ImageLoadWithQuiet(true))
	if err != nil {
		return fmt.Errorf("load image into Docker: %w", err)
	}
	defer func() { _ = res.Close() }()
	// The response is a JSON message stream; a failed load reports an error in it.
	scanner := bufio.NewScanner(res)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var message struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &message) == nil && message.Error != "" {
			return fmt.Errorf("load image into Docker: %s", message.Error)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read image load response: %w", err)
	}
	return nil
}

// wait returns the load result; it reports nil if no export was attempted.
func (l *imageLoader) wait(ctx context.Context) error {
	l.once.Do(func() { l.done <- nil }) // No export: nothing to wait for.
	select {
	case err := <-l.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// cappedWriter fails once more than limit bytes are written.
type cappedWriter struct {
	w       *io.PipeWriter
	limit   int64
	written int64
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.written+int64(len(p)) > c.limit {
		err := errImageTooLarge
		c.w.CloseWithError(err)
		return 0, err
	}
	n, err := c.w.Write(p)
	c.written += int64(n)
	return n, err
}

func (c *cappedWriter) Close() error { return c.w.Close() }

// reusable returns the image an earlier attempt finished for this exact
// revision, if it exists, carries this job's identity and fits the limit.
func (b *Builder) reusable(ctx context.Context, job Job) (Image, bool) {
	if job.BaselineRefreshID > 0 {
		return Image{}, false // every refresh builds its branch head afresh
	}
	inspected, err := b.docker.ImageInspect(ctx, job.ImageRef())
	if err != nil || inspected.Config == nil {
		return Image{}, false
	}
	labels := inspected.Config.Labels
	if !dockerx.OwnedBy(labels, job.Instance, dockerx.KindImage) || labels[dockerx.LabelPR] != itoa(job.PullRequestID) || labels[dockerx.LabelGeneration] != itoa(job.Generation) ||
		labels[dockerx.LabelRevision] != itoa(job.Revision) || labels[dockerx.LabelSHA] != job.HeadSHA ||
		labels[dockerx.LabelBaseRepo] != itoa(job.BaseRepoID) || labels[dockerx.LabelHeadRepo] != itoa(job.HeadRepoID) || inspected.Size > b.cfg.MaxImageSize {
		return Image{}, false
	}
	return Image{Ref: job.ImageRef(), ID: inspected.ID, Size: inspected.Size, Reused: true}, true
}

// Artifact is something a build left in Docker, found by its labels.
type Artifact struct {
	Kind                 string // dockerx.KindImage, KindBuilder or KindBuildNetwork
	ID                   string
	Generation, Revision int64
}

// Resources lists images, builders and build networks labelled with the PR,
// including leftovers of crashed builds. Shared cache volumes are per source
// repository, not per PR, and are pruned by housekeeping.
func (b *Builder) Resources(ctx context.Context, pullRequestID int64) ([]Artifact, error) {
	filter := client.Filters{}.Add("label", dockerx.LabelManaged+"=true", dockerx.LabelInstance+"="+b.cfg.Instance, dockerx.LabelPR+"="+itoa(pullRequestID))
	var found []Artifact
	images, err := b.docker.ImageList(ctx, client.ImageListOptions{Filters: filter})
	if err != nil {
		return nil, fmt.Errorf("list build images: %w", err)
	}
	for _, i := range images.Items {
		found = append(found, artifact(dockerx.KindImage, i.ID, i.Labels))
	}
	containers, err := b.docker.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filter})
	if err != nil {
		return nil, fmt.Errorf("list builders: %w", err)
	}
	for _, c := range containers.Items {
		found = append(found, artifact(dockerx.KindBuilder, c.ID, c.Labels))
	}
	networks, err := b.docker.NetworkList(ctx, client.NetworkListOptions{Filters: filter})
	if err != nil {
		return nil, fmt.Errorf("list build networks: %w", err)
	}
	for _, n := range networks.Items {
		found = append(found, artifact(dockerx.KindBuildNetwork, n.ID, n.Labels))
	}
	return found, nil
}

func artifact(kind, id string, labels map[string]string) Artifact {
	generation, _ := strconv.ParseInt(labels[dockerx.LabelGeneration], 10, 64)
	revision, _ := strconv.ParseInt(labels[dockerx.LabelRevision], 10, 64)
	return Artifact{Kind: kind, ID: id, Generation: generation, Revision: revision}
}

// Remove deletes an artifact; one that is already gone counts as removed.
func (b *Builder) Remove(ctx context.Context, a Artifact) error {
	var err error
	switch a.Kind {
	case dockerx.KindImage:
		_, err = b.docker.ImageRemove(ctx, a.ID, client.ImageRemoveOptions{Force: true, PruneChildren: true})
	case dockerx.KindBuilder:
		_, err = b.docker.ContainerRemove(ctx, a.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	case dockerx.KindBuildNetwork:
		if err := b.egress.Detach(ctx, a.ID); err != nil {
			return err
		}
		_, err = b.docker.NetworkRemove(ctx, a.ID, client.NetworkRemoveOptions{})
	default:
		return fmt.Errorf("unknown build artifact kind %q", a.Kind)
	}
	if err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove build %s: %w", a.Kind, err)
	}
	return nil
}
