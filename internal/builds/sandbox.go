package builds

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

const (
	egressContainerName = "previa-build-egress"
	egressNetworkName   = "previa-build-egress" // the proxy's only route outward
	builderStateDir     = "/home/user/.local/share/buildkit"
	readyTimeout        = 30 * time.Second
	cleanupTimeout      = 30 * time.Second
	// staleBuilder bounds how long a cached builder may hold its cache when the
	// build time limit is unknown: longer than any build may run.
	staleBuilder = 25 * time.Hour
)

// sandbox is one build's BuildKit container and private network.
type sandbox struct {
	docker      *client.Client
	networkID   string
	egressID    string
	containerID string
	proxyURL    string
	cached      bool
}

// openSandbox creates the private network, connects the egress proxy to it and
// starts a ready builder. On failure everything created is removed.
func (b *Builder) openSandbox(ctx context.Context, job Job) (s *sandbox, err error) {
	s = &sandbox{docker: b.docker}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.close(ctx))
		}
	}()
	if err := b.ensureBuildKitImage(ctx); err != nil {
		return s, err
	}
	if s.egressID, err = b.ensureEgress(ctx); err != nil {
		return s, err
	}
	created, err := b.docker.NetworkCreate(ctx, job.attemptName("previa-build", b.now()), client.NetworkCreateOptions{
		Driver: "bridge", Internal: true, Labels: job.identity(KindNetwork),
	})
	if err != nil {
		return s, fmt.Errorf("create build network: %w", err)
	}
	s.networkID = created.ID
	if _, err := b.docker.NetworkConnect(ctx, s.networkID, client.NetworkConnectOptions{Container: s.egressID, EndpointConfig: &network.EndpointSettings{}}); err != nil {
		return s, fmt.Errorf("connect egress proxy: %w", err)
	}
	inspected, err := b.docker.NetworkInspect(ctx, s.networkID, client.NetworkInspectOptions{})
	if err != nil {
		return s, fmt.Errorf("inspect build network: %w", err)
	}
	endpoint, ok := inspected.Network.Containers[s.egressID]
	if !ok || !endpoint.IPv4Address.IsValid() {
		return s, errors.New("egress proxy has no address on the build network")
	}
	s.proxyURL = "http://" + endpoint.IPv4Address.Addr().String() + ":" + EgressPort
	if s.containerID, s.cached, err = b.createBuilder(ctx, job, s); err != nil {
		return s, err
	}
	if _, err := b.docker.ContainerStart(ctx, s.containerID, client.ContainerStartOptions{}); err != nil {
		return s, fmt.Errorf("start builder: %w", err)
	}
	return s, s.waitReady(ctx)
}

// ensureBuildKitImage pulls the pinned BuildKit image when Docker does not
// have it yet. Docker itself pulls it; builders never reach a registry
// except through the egress proxy.
func (b *Builder) ensureBuildKitImage(ctx context.Context) error {
	if _, err := b.docker.ImageInspect(ctx, b.cfg.BuildKitImage); err == nil {
		return nil
	} else if !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("inspect BuildKit image: %w", err)
	}
	pulled, err := b.docker.ImagePull(ctx, b.cfg.BuildKitImage, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull BuildKit image: %w", err)
	}
	defer func() { _ = pulled.Close() }()
	if err := pulled.Wait(ctx); err != nil {
		return fmt.Errorf("pull BuildKit image: %w", err)
	}
	return nil
}

// ensureEgress returns the running egress proxy, creating it when missing.
func (b *Builder) ensureEgress(ctx context.Context) (string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		inspected, err := b.docker.ContainerInspect(ctx, egressContainerName, client.ContainerInspectOptions{})
		if err == nil {
			if inspected.Container.State == nil || !inspected.Container.State.Running {
				if _, err := b.docker.ContainerStart(ctx, inspected.Container.ID, client.ContainerStartOptions{}); err != nil {
					return "", fmt.Errorf("start egress proxy: %w", err)
				}
			}
			return inspected.Container.ID, nil
		}
		if !cerrdefs.IsNotFound(err) {
			return "", fmt.Errorf("inspect egress proxy: %w", err)
		}
		if _, err := b.docker.ImageInspect(ctx, b.cfg.EgressImage); err != nil {
			return "", fmt.Errorf("egress proxy image %s is not available (run make build-egress-image): %w", b.cfg.EgressImage, err)
		}
		if err := b.ensureEgressNetwork(ctx); err != nil {
			return "", err
		}
		pids := int64(512)
		created, err := b.docker.ContainerCreate(ctx, client.ContainerCreateOptions{
			Name: egressContainerName,
			Config: &container.Config{
				Image:  b.cfg.EgressImage,
				User:   "65532:65532",
				Labels: map[string]string{LabelManaged: "true", LabelKind: KindEgress},
			},
			HostConfig: &container.HostConfig{
				NetworkMode:    container.NetworkMode(egressNetworkName),
				RestartPolicy:  container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
				ReadonlyRootfs: true,
				CapDrop:        []string{"ALL"},
				SecurityOpt:    []string{"no-new-privileges"},
				Resources:      container.Resources{Memory: 256 << 20, MemorySwap: 256 << 20, NanoCPUs: 1e9, PidsLimit: &pids},
			},
		})
		if cerrdefs.IsConflict(err) {
			continue // Another build created it first.
		}
		if err != nil {
			return "", fmt.Errorf("create egress proxy: %w", err)
		}
		if _, err := b.docker.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
			return "", fmt.Errorf("start egress proxy: %w", err)
		}
		return created.ID, nil
	}
	return "", errors.New("egress proxy could not be created")
}

func (b *Builder) ensureEgressNetwork(ctx context.Context) error {
	_, err := b.docker.NetworkInspect(ctx, egressNetworkName, client.NetworkInspectOptions{})
	if err == nil {
		return nil
	}
	if !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("inspect egress network: %w", err)
	}
	_, err = b.docker.NetworkCreate(ctx, egressNetworkName, client.NetworkCreateOptions{
		Driver: "bridge", Labels: map[string]string{LabelManaged: "true", LabelKind: KindEgress},
	})
	if err != nil && !cerrdefs.IsConflict(err) {
		return fmt.Errorf("create egress network: %w", err)
	}
	return nil
}

// createBuilder creates the builder container. It takes the job's cache scope
// when free; the scope's builder name is the lock, so a concurrent build of the
// same source builds without cache instead of waiting.
func (b *Builder) createBuilder(ctx context.Context, job Job, s *sandbox) (string, bool, error) {
	scope := job.cacheScope()
	volume := cacheVolumeName(scope)
	if _, err := b.docker.VolumeCreate(ctx, client.VolumeCreateOptions{Name: volume, Labels: map[string]string{
		LabelManaged: "true", LabelKind: KindCache, LabelBaseRepo: itoa(job.BaseRepoID), LabelHeadRepo: itoa(job.HeadRepoID),
	}}); err != nil {
		return "", false, fmt.Errorf("create build cache volume: %w", err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		id, err := b.createBuilderContainer(ctx, job, s, cachedBuilderName(scope), volume)
		if err == nil {
			return id, true, nil
		}
		if !cerrdefs.IsConflict(err) {
			return "", false, err
		}
		if removed, err := b.removeStaleBuilder(ctx, cachedBuilderName(scope)); err != nil || !removed {
			break
		}
	}
	id, err := b.createBuilderContainer(ctx, job, s, job.attemptName("previa-builder", b.now()), "")
	return id, false, err
}

func (b *Builder) createBuilderContainer(ctx context.Context, job Job, s *sandbox, name, volume string) (string, error) {
	labels := job.identity(KindBuilder)
	labels[LabelStarted] = b.now().UTC().Format(time.RFC3339)
	pids := b.cfg.PIDs
	created, err := b.docker.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image:  b.cfg.BuildKitImage,
			Labels: labels,
			// buildkitd pulls base images through the proxy; it has no other route.
			Env: []string{"HTTP_PROXY=" + s.proxyURL, "HTTPS_PROXY=" + s.proxyURL, "http_proxy=" + s.proxyURL, "https_proxy=" + s.proxyURL, "NO_PROXY=", "no_proxy="},
			// Garbage-collect the retained cache down to a quarter of the disk budget.
			Cmd: []string{"--oci-worker-gc", "--oci-worker-gc-keepstorage", strconv.FormatInt(b.cfg.DiskBudget/4>>20, 10)},
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    container.NetworkMode(s.networkID),
			ReadonlyRootfs: true,
			// Rootless BuildKit needs only these two capabilities (for newuidmap).
			// It creates its own user, mount and PID namespaces for every build
			// step, which Docker's default seccomp and AppArmor profiles forbid;
			// steps still run under BuildKit's own seccomp profile.
			CapDrop:     []string{"ALL"},
			CapAdd:      []string{"SETUID", "SETGID"},
			SecurityOpt: []string{"seccomp=unconfined", "apparmor=unconfined"},
			// Empty lists unmask /proc and /sys paths (the CLI's
			// systempaths=unconfined) so BuildKit can mount procfs for each
			// step's own PID namespace instead of sharing the daemon's.
			MaskedPaths:   []string{},
			ReadonlyPaths: []string{},
			Tmpfs: map[string]string{
				"/tmp":                  "mode=1777",
				"/run/user/1000":        "mode=0700,uid=1000,gid=1000",
				"/home/user/.local/tmp": "mode=0700,uid=1000,gid=1000",
			},
			// An empty Source is an anonymous volume, removed with the container.
			Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: volume, Target: builderStateDir}},
			Resources: container.Resources{
				NanoCPUs: int64(b.cfg.CPUs * 1e9), Memory: b.cfg.Memory, MemorySwap: b.cfg.Memory, PidsLimit: &pids,
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("create builder: %w", err)
	}
	return created.ID, nil
}

// removeStaleBuilder removes a cache-holding builder left by a crashed worker:
// one that stopped, or that started longer ago than any build may run.
func (b *Builder) removeStaleBuilder(ctx context.Context, name string) (bool, error) {
	inspected, err := b.docker.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect cached builder: %w", err)
	}
	c := inspected.Container
	var started time.Time
	if c.Config != nil {
		started, _ = time.Parse(time.RFC3339, c.Config.Labels[LabelStarted])
	}
	running := c.State != nil && c.State.Running
	if running && !started.IsZero() && b.now().Sub(started) < b.staleAfter() {
		return false, nil
	}
	if _, err := b.docker.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return false, fmt.Errorf("remove stale builder: %w", err)
	}
	return true, nil
}

func (s *sandbox) waitReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	for {
		if _, code, err := execOutput(ctx, s.docker, s.containerID, "buildctl", "debug", "workers"); err == nil && code == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("builder did not become ready")
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// freeDisk reports the bytes available to the builder's state, which lives on
// the Docker host's storage.
func (s *sandbox) freeDisk(ctx context.Context) (int64, error) {
	out, code, err := execOutput(ctx, s.docker, s.containerID, "df", "-Pk", builderStateDir)
	if err != nil || code != 0 {
		return 0, fmt.Errorf("measure free disk: %w", errors.Join(err, fmt.Errorf("exit code %d", code)))
	}
	return parseDFAvailable(out)
}

// parseDFAvailable reads the available kilobytes from `df -Pk` output.
func parseDFAvailable(out string) (int64, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return 0, errors.New("unexpected df output")
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		return 0, errors.New("unexpected df output")
	}
	kb, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil || kb < 0 {
		return 0, errors.New("unexpected df output")
	}
	return kb << 10, nil
}

// close removes the builder (and its anonymous state volume) and the private
// network. The scope's cache volume is kept for the next build.
func (s *sandbox) close(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	var errs []error
	if s.containerID != "" {
		if _, err := s.docker.ContainerRemove(ctx, s.containerID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("remove builder: %w", err))
		}
	}
	if s.networkID != "" {
		if s.egressID != "" {
			if _, err := s.docker.NetworkDisconnect(ctx, s.networkID, client.NetworkDisconnectOptions{Container: s.egressID, Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("disconnect egress proxy: %w", err))
			}
		}
		if _, err := s.docker.NetworkRemove(ctx, s.networkID, client.NetworkRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("remove build network: %w", err))
		}
	}
	return errors.Join(errs...)
}
