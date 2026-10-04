package egress

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

const (
	// ContainerName and NetworkName identify the shared egress container and
	// the bridge network that is its only route outward.
	ContainerName = "previa-egress"
	NetworkName   = "previa-egress"
	binary        = "/previa-egress"
)

// Gateway runs the shared egress container. It is the only route out of build
// and preview networks and the origin of readiness probes into previews. It
// runs only Previa's proxy, holds no credentials, and reaches nothing but
// public addresses.
type Gateway struct {
	docker *client.Client
	image  string
}

func NewGateway(docker *client.Client, image string) (*Gateway, error) {
	if docker == nil || strings.TrimSpace(image) == "" {
		return nil, errors.New("egress gateway requires a Docker client and an image")
	}
	return &Gateway{docker: docker, image: image}, nil
}

// Ensure returns the running egress container, creating or starting it.
func (g *Gateway) Ensure(ctx context.Context) (string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		inspected, err := g.docker.ContainerInspect(ctx, ContainerName, client.ContainerInspectOptions{})
		if err == nil {
			if inspected.Container.State == nil || !inspected.Container.State.Running {
				if _, err := g.docker.ContainerStart(ctx, inspected.Container.ID, client.ContainerStartOptions{}); err != nil {
					return "", fmt.Errorf("start egress proxy: %w", err)
				}
			}
			return inspected.Container.ID, nil
		}
		if !cerrdefs.IsNotFound(err) {
			return "", fmt.Errorf("inspect egress proxy: %w", err)
		}
		if _, err := g.docker.ImageInspect(ctx, g.image); err != nil {
			return "", fmt.Errorf("egress proxy image %s is not available (run make egress-image): %w", g.image, err)
		}
		if err := g.ensureNetwork(ctx); err != nil {
			return "", err
		}
		pids := int64(512)
		created, err := g.docker.ContainerCreate(ctx, client.ContainerCreateOptions{
			Name: ContainerName,
			Config: &container.Config{
				Image:  g.image,
				User:   "65532:65532",
				Labels: map[string]string{dockerx.LabelManaged: "true", dockerx.LabelKind: dockerx.KindEgress},
			},
			HostConfig: &container.HostConfig{
				NetworkMode:    container.NetworkMode(NetworkName),
				RestartPolicy:  container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
				ReadonlyRootfs: true,
				CapDrop:        []string{"ALL"},
				SecurityOpt:    []string{"no-new-privileges"},
				Resources:      container.Resources{Memory: 256 << 20, MemorySwap: 256 << 20, NanoCPUs: 1e9, PidsLimit: &pids},
			},
		})
		if cerrdefs.IsConflict(err) {
			continue // Another worker created it first.
		}
		if err != nil {
			return "", fmt.Errorf("create egress proxy: %w", err)
		}
		if _, err := g.docker.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
			return "", fmt.Errorf("start egress proxy: %w", err)
		}
		return created.ID, nil
	}
	return "", errors.New("egress proxy could not be created")
}

func (g *Gateway) ensureNetwork(ctx context.Context) error {
	_, err := g.docker.NetworkInspect(ctx, NetworkName, client.NetworkInspectOptions{})
	if err == nil {
		return nil
	}
	if !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("inspect egress network: %w", err)
	}
	_, err = g.docker.NetworkCreate(ctx, NetworkName, client.NetworkCreateOptions{
		Driver: "bridge", Labels: map[string]string{dockerx.LabelManaged: "true", dockerx.LabelKind: dockerx.KindEgress},
	})
	if err != nil && !cerrdefs.IsConflict(err) {
		return fmt.Errorf("create egress network: %w", err)
	}
	return nil
}

// Attach connects the egress container to a build or preview network and
// returns the proxy URL for containers on it. Attaching twice is harmless.
func (g *Gateway) Attach(ctx context.Context, networkID string) (string, error) {
	id, err := g.Ensure(ctx)
	if err != nil {
		return "", err
	}
	if addr, ok, err := g.address(ctx, networkID, id); err != nil {
		return "", err
	} else if ok {
		return proxyURL(addr), nil
	}
	if _, err := g.docker.NetworkConnect(ctx, networkID, client.NetworkConnectOptions{Container: id, EndpointConfig: &network.EndpointSettings{}}); err != nil {
		// A concurrent attach may have won the race.
		if addr, ok, inspectErr := g.address(ctx, networkID, id); inspectErr == nil && ok {
			return proxyURL(addr), nil
		}
		return "", fmt.Errorf("connect egress proxy: %w", err)
	}
	addr, ok, err := g.address(ctx, networkID, id)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("egress proxy has no address on the network")
	}
	return proxyURL(addr), nil
}

func proxyURL(addr netip.Addr) string { return "http://" + addr.String() + ":" + Port }

func (g *Gateway) address(ctx context.Context, networkID, containerID string) (netip.Addr, bool, error) {
	inspected, err := g.docker.NetworkInspect(ctx, networkID, client.NetworkInspectOptions{})
	if err != nil {
		return netip.Addr{}, false, fmt.Errorf("inspect network: %w", err)
	}
	endpoint, ok := inspected.Network.Containers[containerID]
	if !ok || !endpoint.IPv4Address.IsValid() {
		return netip.Addr{}, false, nil
	}
	return endpoint.IPv4Address.Addr(), true, nil
}

// Detach disconnects the egress container from a network. A network or
// endpoint that is already gone counts as detached.
func (g *Gateway) Detach(ctx context.Context, networkID string) error {
	_, err := g.docker.NetworkDisconnect(ctx, networkID, client.NetworkDisconnectOptions{Container: ContainerName, Force: true})
	if err != nil && !cerrdefs.IsNotFound(err) && !strings.Contains(err.Error(), "not connected") {
		return fmt.Errorf("disconnect egress proxy: %w", err)
	}
	return nil
}

// ProbeFailure is a probe that ran and found the preview not ready yet.
type ProbeFailure struct{ Reason string }

func (p *ProbeFailure) Error() string { return p.Reason }

// Probe checks a preview from inside the egress container, which shares the
// preview's network. It returns nil when the preview is ready, a
// *ProbeFailure with the probe's reason when it is not, and another error
// when the probe could not run.
func (g *Gateway) Probe(ctx context.Context, target netip.AddrPort, path string, timeout time.Duration) error {
	cmd := []string{binary, "probe", "-target", target.String(), "-timeout", timeout.String()}
	if path != "" {
		cmd = append(cmd, "-path", path)
	}
	out, code, err := dockerx.Exec(ctx, g.docker, ContainerName, cmd...)
	switch {
	case err != nil:
		return fmt.Errorf("run readiness probe: %w", err)
	case code == 1:
		reason, _, _ := strings.Cut(out, "\n")
		return &ProbeFailure{Reason: reason}
	case code != 0:
		return fmt.Errorf("readiness probe exited with code %d", code)
	}
	return nil
}
