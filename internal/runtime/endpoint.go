package runtime

import (
	"context"
	"errors"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/platform/egress"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// endpointProbes is how many consecutive readiness failures, a second apart,
// fail a candidate at publication time. It passed readiness when it started.
const endpointProbes = 5

// Expected identifies the candidate a route activation wants to publish.
type Expected struct {
	PullRequestID int64
	Generation    int64
	Revision      int64
	ContainerID   string
	ImageTag      string
}

// Endpoint is how the router reaches a verified preview on its PR's network.
type Endpoint struct {
	Name      string // container name, resolved by Docker's DNS on that network
	Port      int
	NetworkID string
}

// Endpoint verifies a candidate immediately before it is routed: a Previa
// preview of exactly this PR, generation, revision and image; running, not
// restarting or crash-looping; and passing its readiness check now, probed
// from the egress container on the PR's network. A missing, foreign, stopped
// or unready candidate fails permanently and is never routed.
func (r *Runtime) Endpoint(ctx context.Context, want Expected) (Endpoint, error) {
	inspected, err := r.docker.ContainerInspect(ctx, want.ContainerID, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return Endpoint{}, failure(CodeMissing, "The candidate no longer exists.", false, nil)
	}
	if err != nil {
		return Endpoint{}, failure(CodeUnavailable, "The candidate could not be inspected.", true, err)
	}
	c := inspected.Container
	if c.Config == nil || !dockerx.OwnedBy(c.Config.Labels, r.instance, dockerx.KindPreview) || !candidateMatches(c.Config.Labels, want) {
		return Endpoint{}, failure(CodeNotOwned, "The container is not this request's candidate.", false, nil)
	}
	health := observedHealth(c)
	switch {
	case health.CrashLoop || health.State == string(container.StateExited) || health.State == string(container.StateDead):
		return Endpoint{}, failure(CodeCrashed, "The candidate stopped before it was published.", false, nil)
	case !health.Running:
		return Endpoint{}, failure(CodeUnhealthy, "The candidate is restarting.", true, nil)
	}
	port, err := strconv.Atoi(c.Config.Labels[dockerx.LabelPort])
	if err != nil || port <= 0 || port > 65535 {
		return Endpoint{}, failure(CodeNotOwned, "The candidate has no valid port label.", false, nil)
	}
	network := r.networkName(want.PullRequestID)
	addr, ok := candidateAddress(c, network)
	endpoint := c.NetworkSettings.Networks[network]
	if !ok || endpoint == nil || endpoint.NetworkID == "" {
		return Endpoint{}, failure(CodeUnavailable, "The candidate has no address on its preview network.", true, nil)
	}
	reason := "it did not answer"
	for attempt := range endpointProbes {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return Endpoint{}, failure(CodeUnavailable, "The readiness check was interrupted.", true, ctx.Err())
			case <-time.After(probeInterval):
			}
		}
		err := r.egress.Probe(ctx, netip.AddrPortFrom(addr, uint16(port)), c.Config.Labels[dockerx.LabelHealthPath], probeTimeout)
		if err == nil {
			return Endpoint{Name: strings.TrimPrefix(c.Name, "/"), Port: port, NetworkID: endpoint.NetworkID}, nil
		}
		var notReady *egress.ProbeFailure
		if !errors.As(err, &notReady) {
			return Endpoint{}, failure(CodeUnavailable, "The readiness check could not run.", true, err)
		}
		reason = notReady.Reason
	}
	return Endpoint{}, failure(CodeUnhealthy, "The candidate failed its readiness check before publication: "+reason+".", false, nil)
}

func candidateMatches(labels map[string]string, want Expected) bool {
	return labels[dockerx.LabelPR] == strconv.FormatInt(want.PullRequestID, 10) &&
		labels[dockerx.LabelGeneration] == strconv.FormatInt(want.Generation, 10) &&
		labels[dockerx.LabelRevision] == strconv.FormatInt(want.Revision, 10) &&
		labels[dockerx.LabelImage] == want.ImageTag
}
