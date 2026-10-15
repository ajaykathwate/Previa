package runtime

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// sleepingKinds are the containers an idle preview stops: its app and its
// processes. Managed services keep running, so their data survives a sleep.
var sleepingKinds = []string{dockerx.KindPreview, dockerx.KindProcess}

// candidateContainers lists this instance's containers of one published
// candidate whose kind is in kinds, in any state.
func (r *Runtime) candidateContainers(ctx context.Context, pullRequestID, generation, revision int64, kinds []string) ([]container.Summary, error) {
	filter := client.Filters{}.Add("label",
		dockerx.LabelManaged+"=true", dockerx.LabelInstance+"="+r.instance,
		dockerx.LabelPR+"="+strconv.FormatInt(pullRequestID, 10),
		dockerx.LabelGeneration+"="+strconv.FormatInt(generation, 10),
		dockerx.LabelRevision+"="+strconv.FormatInt(revision, 10))
	listed, err := r.docker.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filter})
	if err != nil {
		return nil, fmt.Errorf("list preview containers: %w", err)
	}
	var out []container.Summary
	for _, c := range listed.Items {
		if slices.Contains(kinds, c.Labels[dockerx.LabelKind]) {
			out = append(out, c)
		}
	}
	return out, nil
}

// SleepPreview stops an idle preview's app and processes, freeing their
// memory and CPU. Docker does not restart a stopped container.
func (r *Runtime) SleepPreview(ctx context.Context, pullRequestID, generation, revision int64) error {
	containers, err := r.candidateContainers(ctx, pullRequestID, generation, revision, sleepingKinds)
	if err != nil {
		return err
	}
	timeout := stopTimeout
	var errs []error
	for _, c := range containers {
		if c.State != container.StateRunning && c.State != container.StateRestarting {
			continue
		}
		if _, err := r.docker.ContainerStop(ctx, c.ID, client.ContainerStopOptions{Timeout: &timeout}); err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("stop preview container: %w", err))
		}
	}
	return errors.Join(errs...)
}

// WakePreview starts a sleeping preview again, processes first, and returns
// once its app passes its readiness check. It refuses when the host's
// preview budget has no room for it now, and when its app is gone (the
// preview must be built again).
func (r *Runtime) WakePreview(ctx context.Context, pullRequestID, generation, revision int64) error {
	containers, err := r.candidateContainers(ctx, pullRequestID, generation, revision, sleepingKinds)
	if err != nil {
		return failure(CodeUnavailable, "The preview's containers could not be listed.", true, err)
	}
	var app *container.Summary
	var cpuMillis, memory int64
	for i, c := range containers {
		if c.Labels[dockerx.LabelKind] == dockerx.KindPreview {
			app = &containers[i]
		}
		if c.State != container.StateRunning && c.State != container.StateRestarting {
			cpu, _ := strconv.ParseInt(c.Labels[dockerx.LabelCPUMillis], 10, 64)
			mem, _ := strconv.ParseInt(c.Labels[dockerx.LabelMemory], 10, 64)
			cpuMillis, memory = cpuMillis+cpu, memory+mem
		}
	}
	if app == nil {
		return failure(CodeNotCurrent, "The preview's app is gone; build the pull request again.", false, nil)
	}
	if memory > 0 || cpuMillis > 0 {
		b, err := r.budget(ctx)
		if err != nil {
			return failure(CodeUnavailable, "The host's resources could not be read.", true, err)
		}
		u, err := r.usage(ctx, Target{})
		if err != nil {
			return failure(CodeUnavailable, "Running previews could not be counted.", true, err)
		}
		if u.memory+memory > b.memory || u.cpuMillis+cpuMillis > b.cpuMillis {
			return failure(CodeCapacity, "The server has no room to wake this preview right now; try again in a few minutes.", true, nil)
		}
	}
	for _, c := range containers {
		if c.ID == app.ID || c.State == container.StateRunning {
			continue
		}
		if _, err := r.docker.ContainerStart(ctx, c.ID, client.ContainerStartOptions{}); err != nil {
			return failure(CodeUnavailable, "A process of the preview could not be started.", true, err)
		}
	}
	if app.State != container.StateRunning {
		if _, err := r.docker.ContainerStart(ctx, app.ID, client.ContainerStartOptions{}); err != nil {
			return failure(CodeUnavailable, "The preview could not be started.", true, err)
		}
	}
	port, err := strconv.Atoi(app.Labels[dockerx.LabelPort])
	if err != nil {
		return failure(CodeUnavailable, "The preview's port is unknown.", false, err)
	}
	return r.waitReady(ctx, app.ID, r.networkName(pullRequestID), port, app.Labels[dockerx.LabelHealthPath])
}
