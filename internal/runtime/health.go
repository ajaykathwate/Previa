package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/platform/egress"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

const (
	probeInterval = time.Second
	probeTimeout  = 2 * time.Second
)

// Health is a preview container's observed state, for readiness checks,
// recovery and the dashboard.
type Health struct {
	State      string // created, running, restarting, exited, dead or missing
	Running    bool
	ExitCode   int
	Restarts   int
	OOMKilled  bool
	CrashLoop  bool // stopped after exhausting its restarts
	StartedAt  time.Time
	FinishedAt time.Time
}

func observedHealth(c container.InspectResponse) Health {
	h := Health{Restarts: c.RestartCount}
	if c.State != nil {
		h.State = string(c.State.Status)
		h.Running = c.State.Running && !c.State.Restarting
		h.ExitCode, h.OOMKilled = c.State.ExitCode, c.State.OOMKilled
		h.StartedAt, _ = time.Parse(time.RFC3339Nano, c.State.StartedAt)
		h.FinishedAt, _ = time.Parse(time.RFC3339Nano, c.State.FinishedAt)
		stopped := c.State.Status == container.StateExited || c.State.Status == container.StateDead
		h.CrashLoop = stopped && c.RestartCount >= maxRestarts
	}
	return h
}

// Health reports a preview's observed state. It refuses containers Previa did
// not create, and reports a removed container as "missing".
func (r *Runtime) Health(ctx context.Context, containerID string) (Health, error) {
	inspected, err := r.docker.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return Health{State: "missing"}, nil
	}
	if err != nil {
		return Health{}, failure(CodeUnavailable, "The preview could not be inspected.", true, err)
	}
	if inspected.Container.Config == nil || !dockerx.OwnedBy(inspected.Container.Config.Labels, r.instance, dockerx.KindPreview) {
		return Health{}, failure(CodeNotOwned, "This container was not created by Previa.", false, nil)
	}
	return observedHealth(inspected.Container), nil
}

// probeApp checks a starting app once. A crash loop fails it; a running app
// is probed on its address, and a failed probe says why. A check that fails
// only because the deadline passed is no error: the caller reports the
// timeout.
func (r *Runtime) probeApp(deadline context.Context, containerID, network string, port int, path string) (ready bool, why string, err error) {
	inspected, err := r.docker.ContainerInspect(deadline, containerID, client.ContainerInspectOptions{})
	if err != nil {
		if deadline.Err() == nil {
			return false, "", failure(CodeUnavailable, "The preview could not be inspected while starting.", true, err)
		}
		return false, "", nil
	}
	h := observedHealth(inspected.Container)
	if h.CrashLoop {
		message := fmt.Sprintf("The app exited %d times while starting (last exit code %d).", h.Restarts+1, h.ExitCode)
		if h.OOMKilled {
			message = fmt.Sprintf("The app ran out of memory while starting and exited %d times.", h.Restarts+1)
		}
		return false, "", failure(CodeCrashed, message, false, nil)
	}
	addr, ok := candidateAddress(inspected.Container, network)
	if !ok || !h.Running {
		return false, "", nil
	}
	err = r.egress.Probe(deadline, netip.AddrPortFrom(addr, uint16(port)), path, probeTimeout)
	var notReady *egress.ProbeFailure
	switch {
	case err == nil:
		return true, "", nil
	case errors.As(err, &notReady):
		return false, notReady.Reason, nil
	case deadline.Err() == nil:
		return false, "", failure(CodeUnavailable, "The readiness probe could not run.", true, err)
	}
	return false, "", nil
}

// waitReady polls the candidate until a probe succeeds, it crash-loops, or the
// startup deadline passes. Probes run from the egress container, which shares
// only the preview's network, against the candidate's own address and port.
func (r *Runtime) waitReady(ctx context.Context, containerID, network string, port int, path string) error {
	deadline, cancel := context.WithTimeout(ctx, r.policy.StartupTimeout)
	defer cancel()
	reason := "it never accepted connections"
	for {
		ready, why, err := r.probeApp(deadline, containerID, network, port, path)
		if err != nil || ready {
			return err
		}
		if why != "" {
			reason = why
		}
		select {
		case <-deadline.Done():
			switch {
			case errors.Is(ctx.Err(), context.DeadlineExceeded):
				return failure(CodeTimeout, "The build exceeded its time limit while the app was starting.", false, ctx.Err())
			case ctx.Err() != nil:
				return failure(CodeCancelled, "The build was cancelled while the app was starting.", false, ctx.Err())
			}
			return failure(CodeUnhealthy, fmt.Sprintf("The app was not ready within %s: %s.", r.policy.StartupTimeout, reason), false, nil)
		case <-time.After(probeInterval):
		}
	}
}

// candidateAddress is the candidate's IPv4 address on its preview network.
func candidateAddress(c container.InspectResponse, network string) (netip.Addr, bool) {
	if c.NetworkSettings == nil {
		return netip.Addr{}, false
	}
	endpoint, ok := c.NetworkSettings.Networks[network]
	if !ok || endpoint == nil || !endpoint.IPAddress.IsValid() {
		return netip.Addr{}, false
	}
	return endpoint.IPAddress, true
}
