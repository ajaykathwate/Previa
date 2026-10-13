package egress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	// labelSpec records the image the running container was created from,
	// so an upgrade replaces it.
	labelSpec = "io.previa.egress-spec"
)

// Gateway runs the shared egress container. It is the only route out of build
// and preview networks and the origin of readiness probes into previews. It
// runs only Previa's proxy, holds no credentials, and reaches nothing but
// public addresses: through HTTP(S) proxying, and on the operator's
// outbound ports through owner-approved TCP forwarding.
type Gateway struct {
	docker *client.Client
	image  string
	name   string // container and outward network identity
	// managed: this gateway owns owner-approved forwarding (the worker's
	// does); ports are what it forwards, none meaning off.
	managed bool
	ports   []int

	mu sync.Mutex // serializes Ensure, so one process never replaces it twice
}

// NewGateway runs image as the egress container.
func NewGateway(docker *client.Client, image string) (*Gateway, error) {
	return NewNamedGateway(docker, image, ContainerName)
}

var gatewayNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)

// NewNamedGateway gives an independent pipeline its own egress policy and
// listeners. Production uses NewGateway; end-to-end tests must not share a
// development worker's forwarding state.
func NewNamedGateway(docker *client.Client, image, name string) (*Gateway, error) {
	if docker == nil || strings.TrimSpace(image) == "" {
		return nil, errors.New("egress gateway requires a Docker client and an image")
	}
	if !gatewayNamePattern.MatchString(name) {
		return nil, errors.New("egress gateway requires a valid container and network name")
	}
	return &Gateway{docker: docker, image: image, name: name}, nil
}

// Name identifies this gateway's container and outward network.
func (g *Gateway) Name() string { return g.name }

// ManageForwarding makes this gateway own owner-approved outbound forwarding
// (PREVIEW_OUTBOUND_PORTS): every time it records approvals it also tells the
// egress container which ports to forward, and none turns forwarding off.
// Only the worker's gateway manages it; tools and tests leave it alone.
func (g *Gateway) ManageForwarding(ports []int) error {
	for _, port := range ports {
		if port < 1 || port > 65535 || strconv.Itoa(port) == Port {
			return fmt.Errorf("egress gateway cannot forward port %d", port)
		}
	}
	g.managed, g.ports = true, slices.Sorted(slices.Values(ports))
	return nil
}

// Managed reports whether this gateway owns forwarding.
func (g *Gateway) Managed() bool { return g.managed }

// Forwarding reports whether owner-approved outbound connections are on.
func (g *Gateway) Forwarding() bool { return g.managed && len(g.ports) > 0 }

// Ensure returns the running egress container, creating or starting it. A
// container created from another image (an upgrade) is replaced and joined to
// the same build and preview networks again. Its addresses on them may
// change, so previews started before then reach the proxy again only after a
// rebuild.
func (g *Gateway) Ensure(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	image, err := g.docker.ImageInspect(ctx, g.image)
	if err != nil {
		return "", fmt.Errorf("egress proxy image %s is not available (run make egress-image): %w", g.image, err)
	}
	spec := g.spec(image.ID)
	for attempt := 0; attempt < 2; attempt++ {
		inspected, err := g.docker.ContainerInspect(ctx, g.name, client.ContainerInspectOptions{})
		if err == nil {
			return g.reuse(ctx, inspected.Container, spec)
		}
		if !cerrdefs.IsNotFound(err) {
			return "", fmt.Errorf("inspect egress proxy: %w", err)
		}
		id, err := g.create(ctx, spec)
		if cerrdefs.IsConflict(err) {
			continue // Another worker created it first.
		}
		return id, err
	}
	return "", errors.New("egress proxy could not be created")
}

// reuse starts the existing proxy if it runs the current image, and replaces
// it otherwise.
func (g *Gateway) reuse(ctx context.Context, c container.InspectResponse, spec string) (string, error) {
	if c.Config == nil || c.Config.Labels[labelSpec] != spec {
		return g.replace(ctx, c, spec)
	}
	if c.State == nil || !c.State.Running {
		if _, err := g.docker.ContainerStart(ctx, c.ID, client.ContainerStartOptions{}); err != nil {
			return "", fmt.Errorf("start egress proxy: %w", err)
		}
	}
	return c.ID, nil
}

// spec identifies what the container runs: the image's content.
func (g *Gateway) spec(imageID string) string {
	sum := sha256.Sum256([]byte(imageID))
	return hex.EncodeToString(sum[:])[:32]
}

func portList(ports []int) string {
	text := make([]string, len(ports))
	for i, port := range ports {
		text[i] = strconv.Itoa(port)
	}
	return strings.Join(text, ",")
}

func (g *Gateway) create(ctx context.Context, spec string) (string, error) {
	if err := g.ensureNetwork(ctx); err != nil {
		return "", err
	}
	pids := int64(512)
	cmd := []string{"serve", "-state", StateDir}
	created, err := g.docker.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: g.name,
		Config: &container.Config{
			Image:  g.image,
			User:   "65532:65532",
			Cmd:    cmd,
			Labels: map[string]string{dockerx.LabelManaged: "true", dockerx.LabelKind: dockerx.KindEgress, labelSpec: spec},
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    container.NetworkMode(g.name),
			RestartPolicy:  container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
			ReadonlyRootfs: true,
			// Forwarded ports, allow files and counters; lost on restart and
			// written again by the worker (at each preview start and every
			// minute).
			Tmpfs:       map[string]string{StateDir: "rw,nosuid,nodev,noexec,size=4m,uid=65532,gid=65532,mode=0700"},
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges"},
			// Lets the unprivileged proxy listen on an approved port below 1024.
			Sysctls:   map[string]string{"net.ipv4.ip_unprivileged_port_start": "0"},
			Resources: container.Resources{Memory: 256 << 20, MemorySwap: 256 << 20, NanoCPUs: 1e9, PidsLimit: &pids},
		},
	})
	if err != nil {
		if cerrdefs.IsConflict(err) {
			return "", err
		}
		return "", fmt.Errorf("create egress proxy: %w", err)
	}
	if _, err := g.docker.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return "", fmt.Errorf("start egress proxy: %w", err)
	}
	return created.ID, nil
}

// replace removes an outdated egress container and creates the current one,
// joined to the build and preview networks the old one served.
func (g *Gateway) replace(ctx context.Context, old container.InspectResponse, spec string) (string, error) {
	var networks []string
	if old.NetworkSettings != nil {
		for name := range old.NetworkSettings.Networks {
			if name != g.name {
				networks = append(networks, name)
			}
		}
	}
	if _, err := g.docker.ContainerRemove(ctx, old.ID, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return "", fmt.Errorf("replace egress proxy: %w", err)
	}
	id, err := g.create(ctx, spec)
	if err != nil {
		return "", err
	}
	for _, name := range networks {
		if _, err := g.docker.NetworkConnect(ctx, name, client.NetworkConnectOptions{Container: id, EndpointConfig: &network.EndpointSettings{}}); err != nil && !cerrdefs.IsNotFound(err) {
			return "", fmt.Errorf("join egress proxy to %s again: %w", name, err)
		}
	}
	return id, nil
}

func (g *Gateway) ensureNetwork(ctx context.Context) error {
	_, err := g.docker.NetworkInspect(ctx, g.name, client.NetworkInspectOptions{})
	if err == nil {
		return nil
	}
	if !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("inspect egress network: %w", err)
	}
	_, err = g.docker.NetworkCreate(ctx, g.name, client.NetworkCreateOptions{
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
	addr, err := g.Join(ctx, networkID)
	if err != nil {
		return "", err
	}
	return ProxyURL(addr), nil
}

// Join connects the egress container to a network and returns its address
// there: the proxy's address for containers on that network, and the
// address approved outbound connections arrive on.
func (g *Gateway) Join(ctx context.Context, networkID string) (netip.Addr, error) {
	id, err := g.Ensure(ctx)
	if err != nil {
		return netip.Addr{}, err
	}
	if addr, ok, err := g.address(ctx, networkID, id); err != nil {
		return netip.Addr{}, err
	} else if ok {
		return addr, nil
	}
	if _, err := g.docker.NetworkConnect(ctx, networkID, client.NetworkConnectOptions{Container: id, EndpointConfig: &network.EndpointSettings{}}); err != nil {
		// A concurrent attach may have won the race.
		if addr, ok, inspectErr := g.address(ctx, networkID, id); inspectErr == nil && ok {
			return addr, nil
		}
		return netip.Addr{}, fmt.Errorf("connect egress proxy: %w", err)
	}
	addr, ok, err := g.address(ctx, networkID, id)
	if err != nil {
		return netip.Addr{}, err
	}
	if !ok {
		return netip.Addr{}, errors.New("egress proxy has no address on the network")
	}
	// A new address may have belonged to a removed network: whatever was
	// approved there never carries over. Approvals for a preview network are
	// written by the runtime after this.
	if g.Forwarding() {
		if err := g.Allow(ctx, addr, nil); err != nil {
			return netip.Addr{}, err
		}
	}
	return addr, nil
}

// ProxyURL is the HTTP(S) proxy URL for containers on a network where the
// egress container has address addr.
func ProxyURL(addr netip.Addr) string { return "http://" + addr.String() + ":" + Port }

// Address returns the egress container's address on a network it joined.
func (g *Gateway) Address(ctx context.Context, networkID string) (netip.Addr, bool, error) {
	return g.address(ctx, networkID, g.name)
}

// Allow records the destinations approved for the preview network on which
// the egress container has address network; none withdraws every approval
// there. Connections arriving on that network to another destination are
// refused.
func (g *Gateway) Allow(ctx context.Context, network netip.Addr, destinations []Destination) error {
	cmd := append([]string{binary, "allow", "-state", StateDir, "-address", network.String()}, g.portsFlag()...)
	for _, d := range destinations {
		cmd = append(cmd, "-dest", d.String())
	}
	result, err := dockerx.Run(ctx, g.docker, g.name, cmd...)
	switch {
	case err != nil:
		return fmt.Errorf("record approved destinations: %w", err)
	case result.ExitCode != 0:
		return fmt.Errorf("record approved destinations: exit code %d: %s", result.ExitCode, result.Stderr)
	}
	return nil
}

// SyncAllow replaces every approval with desired, keyed by the egress
// container's address on each preview network; approvals of networks not
// listed are withdrawn.
func (g *Gateway) SyncAllow(ctx context.Context, desired map[netip.Addr][]Destination) error {
	cmd := append([]string{binary, "sync", "-state", StateDir}, g.portsFlag()...)
	networks := slices.SortedFunc(func(yield func(netip.Addr) bool) {
		for network := range desired {
			if !yield(network) {
				return
			}
		}
	}, netip.Addr.Compare)
	for _, network := range networks {
		if len(desired[network]) == 0 {
			continue
		}
		hosts := make([]string, len(desired[network]))
		for i, d := range desired[network] {
			hosts[i] = d.String()
		}
		cmd = append(cmd, "-network", network.String()+"="+strings.Join(hosts, ","))
	}
	result, err := dockerx.Run(ctx, g.docker, g.name, cmd...)
	switch {
	case err != nil:
		return fmt.Errorf("synchronize approved destinations: %w", err)
	case result.ExitCode != 0:
		return fmt.Errorf("synchronize approved destinations: exit code %d: %s", result.ExitCode, result.Stderr)
	}
	return nil
}

// portsFlag re-asserts the forwarded ports with every approval update, so a
// restarted egress container (its tmpfs empty) forwards again within a
// minute; an unmanaged gateway leaves them alone.
func (g *Gateway) portsFlag() []string {
	switch {
	case !g.managed:
		return nil
	case len(g.ports) == 0:
		return []string{"-ports", "none"}
	}
	return []string{"-ports", portList(g.ports)}
}

// Stats reads the egress container's forwarding counters.
func (g *Gateway) Stats(ctx context.Context) (Stats, error) {
	result, err := dockerx.Run(ctx, g.docker, g.name, binary, "stats", "-state", StateDir)
	if err != nil {
		return Stats{}, fmt.Errorf("read forwarding counters: %w", err)
	}
	if result.ExitCode != 0 {
		return Stats{}, fmt.Errorf("read forwarding counters: exit code %d", result.ExitCode)
	}
	var s Stats
	if err := json.Unmarshal([]byte(result.Stdout), &s); err != nil {
		return Stats{}, fmt.Errorf("read forwarding counters: %w", err)
	}
	return s, nil
}

// StartMetering counts a build network's traffic from now on, where the
// egress container has address network; hosts, when given, are the only
// destinations the network may reach.
func (g *Gateway) StartMetering(ctx context.Context, network netip.Addr, hosts []string) error {
	cmd := []string{binary, "meter", "-network", network.String(), "-start"}
	if len(hosts) > 0 {
		cmd = append(cmd, "-hosts", strings.Join(hosts, ","))
	}
	result, err := dockerx.Run(ctx, g.docker, g.name, cmd...)
	if err != nil {
		return fmt.Errorf("start metering build traffic: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("start metering build traffic: exit code %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

// EndMetering returns a build network's traffic and stops counting it.
func (g *Gateway) EndMetering(ctx context.Context, network netip.Addr) (Usage, error) {
	result, err := dockerx.Run(ctx, g.docker, g.name, binary, "meter", "-network", network.String(), "-end")
	if err != nil {
		return Usage{}, fmt.Errorf("read build traffic: %w", err)
	}
	if result.ExitCode != 0 {
		return Usage{}, fmt.Errorf("read build traffic: exit code %d", result.ExitCode)
	}
	var u Usage
	if err := json.Unmarshal([]byte(result.Stdout), &u); err != nil {
		return Usage{}, fmt.Errorf("read build traffic: %w", err)
	}
	return u, nil
}

func (g *Gateway) address(ctx context.Context, networkID, containerID string) (netip.Addr, bool, error) {
	inspected, err := g.docker.NetworkInspect(ctx, networkID, client.NetworkInspectOptions{})
	if err != nil {
		return netip.Addr{}, false, fmt.Errorf("inspect network: %w", err)
	}
	endpoint, ok := inspected.Network.Containers[containerID]
	if !ok && containerID == g.name {
		for _, e := range inspected.Network.Containers {
			if e.Name == g.name {
				endpoint, ok = e, true
				break
			}
		}
	}
	if !ok || !endpoint.IPv4Address.IsValid() {
		return netip.Addr{}, false, nil
	}
	return endpoint.IPv4Address.Addr(), true, nil
}

// Detach disconnects the egress container from a network, withdrawing any
// approvals there first. A network or endpoint that is already gone counts
// as detached.
func (g *Gateway) Detach(ctx context.Context, networkID string) error {
	if g.Forwarding() {
		if addr, ok, err := g.address(ctx, networkID, g.name); err == nil && ok {
			if err := g.Allow(ctx, addr, nil); err != nil {
				return err
			}
		}
	}
	_, err := g.docker.NetworkDisconnect(ctx, networkID, client.NetworkDisconnectOptions{Container: g.name, Force: true})
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
	out, code, err := dockerx.Exec(ctx, g.docker, g.name, cmd...)
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
