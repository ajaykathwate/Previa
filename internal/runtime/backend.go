package runtime

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/platform/egress"
	"github.com/ajaykathwatee/previa/internal/repoconfig"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// serviceProfile is the fixed shape of one kind of managed service. Its
// limits count toward preview capacity. Data lives on a tmpfs charged to the
// service's memory limit, so it cannot fill the host disk, and it is gone
// with the candidate: data never survives a rebuild.
type serviceProfile struct {
	title     string // for people
	user      string // the official image's unprivileged user
	port      int
	cpuMillis int64
	memory    int64 // includes the data tmpfs
	dataBytes int64
	dataPath  string // used when the image declares no volume
}

var serviceProfiles = map[string]serviceProfile{
	"postgres": {title: "PostgreSQL", user: "postgres", port: 5432, cpuMillis: 500, memory: 512 << 20, dataBytes: 192 << 20, dataPath: "/var/lib/postgresql/data"},
	"redis":    {title: "Redis", user: "redis", port: 6379, cpuMillis: 250, memory: 256 << 20, dataBytes: 16 << 20, dataPath: "/data"},
}

const (
	// serviceDatabase is the PostgreSQL user and database of every preview.
	serviceDatabase = "preview"
	servicePIDs     = 256
	// redisMaxMemory leaves room inside Redis's memory limit for the server.
	redisMaxMemory = "160mb"
)

// backend is a candidate's companions and what its app needs to reach them.
// Credentials are generated for this candidate only; they live in memory and
// in the containers' configuration, never in PostgreSQL, logs or job
// payloads.
type backend struct {
	key       string // the candidate's key: its app container's name
	nonce     string // makes service hostnames unique to this candidate
	services  []serviceRun
	setup     []string
	processes []repoconfig.Process
	env       map[string]string // the connection settings Previa sets
	secrets   []string          // generated credentials, for the log redactor
}

type serviceRun struct {
	repoconfig.BackendService
	profile  serviceProfile
	alias    string // hostname on the services network
	password string
}

// planBackend generates the candidate's service hostnames and credentials and
// the connection settings its app and processes receive.
func planBackend(key string, settings repoconfig.Config) (backend, error) {
	nonce, err := randomToken(8)
	if err != nil {
		return backend{}, err
	}
	b := backend{key: key, nonce: nonce, setup: settings.Setup, processes: settings.Processes, env: map[string]string{}}
	for _, s := range settings.Services {
		profile, ok := serviceProfiles[s.Kind]
		if !ok {
			return backend{}, fmt.Errorf("unsupported service %q", s.Kind)
		}
		password, err := randomToken(32)
		if err != nil {
			return backend{}, err
		}
		run := serviceRun{BackendService: s, profile: profile, alias: s.Kind + "-" + nonce, password: password}
		b.services = append(b.services, run)
		b.secrets = append(b.secrets, password)
		port := strconv.Itoa(profile.port)
		switch s.Kind {
		case "postgres":
			b.env["DATABASE_URL"] = "postgres://" + serviceDatabase + ":" + password + "@" + run.alias + ":" + port + "/" + serviceDatabase + "?sslmode=disable"
			b.env["PGHOST"], b.env["PGPORT"], b.env["PGUSER"] = run.alias, port, serviceDatabase
			b.env["PGPASSWORD"], b.env["PGDATABASE"], b.env["PGSSLMODE"] = password, serviceDatabase, "disable"
		case "redis":
			b.env["REDIS_URL"] = "redis://:" + password + "@" + run.alias + ":" + port + "/0"
		}
	}
	return b, nil
}

// randomToken is n characters of lowercase base32: safe in hostnames, URLs
// and connection strings without escaping.
func randomToken(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate a random token: %w", err)
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))[:n], nil
}

// resources are what the whole candidate may use: its app, processes,
// services and setup container, which all exist (created) together.
func (b backend) resources(settings repoconfig.Config) (cpuMillis, memory int64) {
	cpuMillis, memory = settings.Resources.CPUMillis, settings.Resources.MemoryBytes
	for _, s := range b.services {
		cpuMillis, memory = cpuMillis+s.profile.cpuMillis, memory+s.profile.memory
	}
	for _, p := range b.processes {
		cpuMillis, memory = cpuMillis+p.Resources.CPUMillis, memory+p.Resources.MemoryBytes
	}
	if len(b.setup) > 0 {
		cpuMillis, memory = cpuMillis+settings.Resources.CPUMillis, memory+settings.Resources.MemoryBytes
	}
	return cpuMillis, memory
}

// injectBackend sets Previa's connection settings over the app's environment
// (YAML and secrets). It returns a notice naming any value it replaced, by
// name only.
func injectBackend(env []string, settings map[string]string) ([]string, string) {
	if len(settings) == 0 {
		return env, ""
	}
	values := map[string]string{}
	for _, entry := range env {
		if name, value, ok := strings.Cut(entry, "="); ok {
			values[name] = value
		}
	}
	var replaced []string
	for name, value := range settings {
		if _, ok := values[name]; ok {
			replaced = append(replaced, name)
		}
		values[name] = value
	}
	names := slices.Sorted(maps.Keys(values))
	merged := make([]string, len(names))
	for i, name := range names {
		merged[i] = name + "=" + values[name]
	}
	if len(replaced) == 0 {
		return merged, ""
	}
	sort.Strings(replaced)
	return merged, "Previa sets " + strings.Join(replaced, ", ") + " for the managed services, replacing the value from .previewrc.yaml or a repository secret."
}

// componentLabels labels a companion like its candidate's app, with its own
// kind, component and limits; only the app carries its port and health path.
func componentLabels(app map[string]string, kind, component string, cpuMillis, memory int64) map[string]string {
	labels := maps.Clone(app)
	delete(labels, dockerx.LabelPort)
	delete(labels, dockerx.LabelHealthPath)
	labels[dockerx.LabelKind], labels[dockerx.LabelComponent] = kind, component
	labels[dockerx.LabelCPUMillis], labels[dockerx.LabelMemory] = strconv.FormatInt(cpuMillis, 10), strconv.FormatInt(memory, 10)
	if kind == dockerx.KindService {
		delete(labels, dockerx.LabelImage) // a catalog image, not one Previa built
	}
	return labels
}

// serviceSpec is a managed service's container: the catalog's pinned image
// as its own unprivileged user, a read-only root, every capability dropped,
// no-new-privileges, IPv6 off, bounded CPU, memory, processes and logs, its
// data on a size-capped tmpfs, and only the candidate's services network,
// reachable there by its own hostname. It has no egress and no route.
func serviceSpec(run serviceRun, volumes []string, networkID string, labels map[string]string, p Policy) (*container.Config, *container.HostConfig, *network.NetworkingConfig) {
	tmpfs := map[string]string{"/tmp": "rw,nosuid,nodev,size=16m,mode=1777"}
	data := run.profile.dataPath
	if len(volumes) == 0 {
		volumes = []string{data}
	}
	for _, path := range volumes {
		// Every declared volume is a tmpfs: Docker creates no volume.
		tmpfs[path] = fmt.Sprintf("rw,nosuid,nodev,size=%d,mode=1777", run.profile.dataBytes)
	}
	if !slices.Contains(volumes, data) {
		data = volumes[0]
	}
	config := &container.Config{
		Image:       run.Image,
		User:        run.profile.user,
		Labels:      labels,
		Hostname:    run.Kind,
		StopTimeout: new(stopTimeout),
	}
	switch run.Kind {
	case "postgres":
		tmpfs["/var/run/postgresql"] = "rw,nosuid,nodev,size=1m,mode=1777"
		// PGDATA is a directory inside the tmpfs, so initdb owns it.
		config.Env = []string{"POSTGRES_USER=" + serviceDatabase, "POSTGRES_PASSWORD=" + run.password, "POSTGRES_DB=" + serviceDatabase, "PGDATA=" + data + "/pgdata"}
		// Data never outlives the candidate, so durability is traded for speed.
		config.Cmd = []string{"postgres", "-c", "fsync=off", "-c", "full_page_writes=off", "-c", "synchronous_commit=off", "-c", "max_connections=50", "-c", "shared_buffers=64MB"}
	case "redis":
		config.Cmd = []string{"redis-server", "--save", "", "--appendonly", "no", "--dir", data, "--requirepass", run.password,
			"--maxmemory", redisMaxMemory, "--maxmemory-policy", "noeviction"}
	}
	pids := int64(servicePIDs)
	host := &container.HostConfig{
		NetworkMode:    container.NetworkMode(networkID),
		RestartPolicy:  container.RestartPolicy{Name: container.RestartPolicyDisabled},
		ReadonlyRootfs: true,
		Tmpfs:          tmpfs,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		Sysctls:        map[string]string{"net.ipv6.conf.all.disable_ipv6": "1", "net.ipv6.conf.default.disable_ipv6": "1"},
		Resources: container.Resources{
			NanoCPUs: run.profile.cpuMillis * 1e6, Memory: run.profile.memory, MemorySwap: run.profile.memory,
			PidsLimit: &pids, CPUShares: cpuWeight,
		},
		OomScoreAdj: oomPriority,
		LogConfig:   container.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "10m", "max-file": "2"}},
		Runtime:     p.OCIRuntime,
	}
	networking := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{networkID: {Aliases: []string{run.alias}}}}
	return config, host, networking
}

// commandSpec turns the app's hardened specification into a setup or process
// container: the same image, user, environment, limits and networks, running
// exactly argv (it replaces the image's entrypoint and command; no shell),
// with its own restart policy and labels and no routed port.
func commandSpec(app *container.Config, appHost *container.HostConfig, appNetworking *network.NetworkingConfig, argv []string, hostname string,
	restart container.RestartPolicy, labels map[string]string, cpuMillis, memory int64) (*container.Config, *container.HostConfig, *network.NetworkingConfig) {
	config := *app
	config.Entrypoint, config.Cmd = slices.Clone(argv), nil
	config.Labels, config.Hostname = labels, hostname
	config.Env = slices.Clone(app.Env)
	host := *appHost
	host.RestartPolicy = restart
	host.NanoCPUs, host.Memory, host.MemorySwap = cpuMillis*1e6, memory, memory
	return &config, &host, appNetworking
}

// servicesNetworkName is the PR's network for its managed services. Only the
// PR's candidates (apps, processes, setup) and their services join it: no
// egress proxy and no router, so services reach nothing beyond it.
func (r *Runtime) servicesNetworkName(pullRequestID int64) string {
	return r.networkName(pullRequestID) + "-services"
}

// servicesNetwork returns the PR's internal services network, creating it
// when missing. It refuses a same-named network Previa did not create.
func (r *Runtime) servicesNetwork(ctx context.Context, id requestIdentity, t Target) (string, error) {
	name := r.servicesNetworkName(id.PullRequestID)
	for attempt := 0; attempt < 2; attempt++ {
		inspected, err := r.docker.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
		if err == nil {
			if !dockerx.OwnedBy(inspected.Network.Labels, r.instance, dockerx.KindServiceNetwork) || !inspected.Network.Internal {
				return "", failure(CodeNotOwned, "A Docker network with this preview's services network name was not created by Previa.", false, nil)
			}
			return inspected.Network.ID, nil
		}
		if !cerrdefs.IsNotFound(err) {
			return "", fmt.Errorf("inspect services network: %w", err)
		}
		created, err := r.docker.NetworkCreate(ctx, name, client.NetworkCreateOptions{Driver: "bridge", Internal: true, Labels: map[string]string{
			dockerx.LabelManaged: "true", dockerx.LabelInstance: r.instance, dockerx.LabelKind: dockerx.KindServiceNetwork,
			dockerx.LabelPR: strconv.FormatInt(id.PullRequestID, 10), dockerx.LabelBaseRepo: strconv.FormatInt(t.BaseRepoID, 10),
		}})
		if cerrdefs.IsConflict(err) {
			continue // Created concurrently; inspect it.
		}
		if err != nil {
			return "", fmt.Errorf("create services network: %w", err)
		}
		return created.ID, nil
	}
	return "", errors.New("services network could not be created")
}

// serviceImages makes sure every service's pinned image is present, pulling
// it when needed, and returns the volumes each image declares (they become
// tmpfs mounts). It runs before admission: a pull may take minutes and never
// holds the admission lock.
func (r *Runtime) serviceImages(ctx context.Context, services []serviceRun, logs LogSink) (map[string][]string, error) {
	volumes := map[string][]string{}
	for _, s := range services {
		inspected, err := r.docker.ImageInspect(ctx, s.Image)
		if cerrdefs.IsNotFound(err) {
			logs.System(fmt.Sprintf("Pulling the %s %s image pinned by the operator.", s.profile.title, s.Version))
			pulled, pullErr := r.docker.ImagePull(ctx, s.Image, client.ImagePullOptions{})
			if pullErr == nil {
				pullErr = pulled.Wait(ctx)
				_ = pulled.Close()
			}
			if pullErr != nil {
				return nil, r.interrupted(ctx, failure(CodeServiceUnavailable, fmt.Sprintf("The %s %s image could not be pulled.", s.profile.title, s.Version), true, pullErr))
			}
			inspected, err = r.docker.ImageInspect(ctx, s.Image)
		}
		if err != nil {
			return nil, failure(CodeServiceUnavailable, fmt.Sprintf("The %s %s image could not be inspected.", s.profile.title, s.Version), true, err)
		}
		if inspected.Config != nil {
			volumes[s.Kind] = slices.Sorted(maps.Keys(inspected.Config.Volumes))
		}
	}
	return volumes, nil
}

// waitServicesReady waits until every service answers its protocol, not just
// a TCP accept: PostgreSQL runs an authenticated query and Redis answers an
// authenticated PING, each from inside the service's own container.
func (r *Runtime) waitServicesReady(ctx context.Context, services []serviceRun, ids map[string]string, logs LogSink) error {
	deadline, cancel := context.WithTimeout(ctx, r.policy.StartupTimeout)
	defer cancel()
	for _, s := range services {
		logs.System(fmt.Sprintf("Waiting up to %s for %s %s to accept authenticated connections.", r.policy.StartupTimeout, s.profile.title, s.Version))
		reason := "it never answered"
		for {
			inspected, err := r.docker.ContainerInspect(deadline, ids[s.Kind], client.ContainerInspectOptions{})
			if err == nil {
				if h := observedHealth(inspected.Container); h.State == string(container.StateExited) || h.State == string(container.StateDead) {
					message := fmt.Sprintf("%s %s stopped while starting (exit code %d).", s.profile.title, s.Version, h.ExitCode)
					if h.OOMKilled {
						message = fmt.Sprintf("%s %s ran out of memory while starting.", s.profile.title, s.Version)
					}
					return failure(CodeServiceFailed, message, false, nil)
				}
				ready, why, err := r.serviceAnswers(deadline, s, ids[s.Kind], true)
				if err != nil && deadline.Err() == nil {
					return failure(CodeUnavailable, "A service readiness check could not run.", true, err)
				}
				if ready {
					logs.System(fmt.Sprintf("%s %s is ready at %s:%d.", s.profile.title, s.Version, s.alias, s.profile.port))
					break
				}
				if why != "" {
					reason = why
				}
			} else if deadline.Err() == nil {
				return failure(CodeUnavailable, "A service could not be inspected while starting.", true, err)
			}
			select {
			case <-deadline.Done():
				if ctx.Err() != nil {
					return r.interrupted(ctx, failure(CodeCancelled, "The build was cancelled while services were starting.", false, ctx.Err()))
				}
				return failure(CodeServiceFailed, fmt.Sprintf("%s %s was not ready within %s: %s.", s.profile.title, s.Version, r.policy.StartupTimeout, reason), false, nil)
			case <-time.After(probeInterval):
			}
		}
	}
	return nil
}

// serviceAnswers runs a service's protocol check inside its container. With
// credentials it authenticates (PostgreSQL runs SELECT 1, Redis answers PING
// with PONG); without them it checks that the server speaks its protocol
// (pg_isready, or Redis asking for authentication).
func (r *Runtime) serviceAnswers(ctx context.Context, s serviceRun, containerID string, authenticated bool) (bool, string, error) {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var env, cmd []string
	port := strconv.Itoa(s.profile.port)
	switch {
	case s.Kind == "postgres" && authenticated:
		env = []string{"PGPASSWORD=" + s.password, "PGCONNECT_TIMEOUT=3"}
		cmd = []string{"psql", "-h", "127.0.0.1", "-p", port, "-U", serviceDatabase, "-d", serviceDatabase, "-tAc", "SELECT 1"}
	case s.Kind == "postgres":
		cmd = []string{"pg_isready", "-h", "127.0.0.1", "-p", port, "-t", "3"}
	case authenticated:
		env = []string{"REDISCLI_AUTH=" + s.password}
		cmd = []string{"redis-cli", "-h", "127.0.0.1", "-p", port, "PING"}
	default:
		cmd = []string{"redis-cli", "-h", "127.0.0.1", "-p", port, "PING"}
	}
	result, err := dockerx.RunWithEnv(checkCtx, r.docker, containerID, env, cmd...)
	if err != nil {
		if ctx.Err() == nil && checkCtx.Err() != nil {
			return false, "its check timed out", nil
		}
		return false, "", err
	}
	switch {
	case s.Kind == "postgres" && authenticated:
		return result.ExitCode == 0 && result.Stdout == "1", "it does not accept connections yet", nil
	case s.Kind == "postgres":
		return result.ExitCode == 0, "it does not accept connections", nil
	case authenticated:
		return result.Stdout == "PONG", "it does not answer PING yet", nil
	}
	// Unauthenticated, Redis still answers in its protocol: asking for a
	// password is an answer.
	return result.Stdout == "PONG" || strings.HasPrefix(result.Stdout, "NOAUTH"), "it does not answer PING", nil
}

// runSetup runs the setup container to completion within the setup timeout,
// streaming its output into the build log. A non-zero exit or the timeout
// fails the candidate (setup_failed). The container is removed afterwards.
func (r *Runtime) runSetup(ctx context.Context, containerID string, argv []string, logs LogSink) error {
	logs.System(fmt.Sprintf("Running the setup command (%d argument%s) with up to %s.", len(argv), plural(len(argv)), r.policy.SetupTimeout))
	runCtx, cancel := context.WithTimeout(ctx, r.policy.SetupTimeout)
	defer cancel()
	waited := r.docker.ContainerWait(runCtx, containerID, client.ContainerWaitOptions{Condition: container.WaitConditionNextExit})
	if _, err := r.docker.ContainerStart(ctx, containerID, client.ContainerStartOptions{}); err != nil {
		r.logger.WarnContext(ctx, "start setup container", "container_id", containerID, "error", err)
		return r.interrupted(ctx, failure(CodeUnavailable, "The setup container could not be started.", true, err))
	}
	streamed := make(chan struct{})
	go func() {
		defer close(streamed)
		reader, err := r.docker.ContainerLogs(runCtx, containerID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Follow: true})
		if err != nil {
			return
		}
		defer func() { _ = reader.Close() }()
		_, _ = stdcopy.StdCopy(logs, logs, reader)
	}()
	var outcome error
	select {
	case result := <-waited.Result:
		switch {
		case result.Error != nil:
			outcome = failure(CodeUnavailable, "The setup command's result could not be read.", true, errors.New(result.Error.Message))
		case result.StatusCode != 0:
			outcome = failure(CodeSetupFailed, fmt.Sprintf("The setup command exited with code %d.", result.StatusCode), false, nil)
		}
	case err := <-waited.Error:
		switch {
		case ctx.Err() != nil:
			outcome = r.interrupted(ctx, failure(CodeCancelled, "The build was cancelled during setup.", false, err))
		case runCtx.Err() != nil:
			outcome = failure(CodeSetupFailed, fmt.Sprintf("The setup command did not finish within %s.", r.policy.SetupTimeout), false, nil)
		default:
			outcome = failure(CodeUnavailable, "The setup command could not be watched.", true, err)
		}
	}
	select {
	case <-streamed:
	case <-time.After(10 * time.Second):
	}
	if outcome != nil {
		return outcome
	}
	logs.System("The setup command finished.")
	r.discard(ctx, containerID)
	return nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// checkProcesses requires every process to be running: one that exited or is
// restarting fails the candidate (process_failed).
func (r *Runtime) checkProcesses(ctx context.Context, processes map[string]string) error {
	for _, name := range slices.Sorted(maps.Keys(processes)) {
		inspected, err := r.docker.ContainerInspect(ctx, processes[name], client.ContainerInspectOptions{})
		if cerrdefs.IsNotFound(err) {
			return failure(CodeProcessFailed, fmt.Sprintf("The process %s no longer exists.", name), false, nil)
		}
		if err != nil {
			return failure(CodeUnavailable, "A process could not be inspected.", true, err)
		}
		h := observedHealth(inspected.Container)
		if !h.Running || h.Restarts > 0 {
			message := fmt.Sprintf("The process %s stopped (exit code %d).", name, h.ExitCode)
			if h.OOMKilled {
				message = fmt.Sprintf("The process %s ran out of memory.", name)
			}
			return failure(CodeProcessFailed, message, false, nil)
		}
	}
	return nil
}

// outbound decides the candidate's approved outbound connections and records
// them for its preview network on the egress proxy. Untrusted sources (forks,
// unknown heads) get none, the same rule as secrets; withheld then says
// whether the repository approves any.
func (r *Runtime) outbound(ctx context.Context, t Target, egressAddr netip.Addr) (destinations []egress.Destination, withheld bool, err error) {
	if r.destinations == nil || !r.egress.Forwarding() {
		return nil, false, nil
	}
	approved, err := r.destinations.Destinations(ctx, t.RepoID)
	if err != nil {
		return nil, false, failure(CodeUnavailable, "The repository's approved outbound connections could not be read.", true, err)
	}
	if !t.secretSource().Trusted() {
		destinations, withheld = nil, len(approved[t.RepoID]) > 0
	} else {
		destinations = approved[t.RepoID]
	}
	if err := r.egress.Allow(ctx, egressAddr, destinations); err != nil {
		return nil, false, failure(CodeUnavailable, "Approved outbound connections could not be prepared on the egress proxy.", true, err)
	}
	return destinations, withheld, nil
}

// RefreshOutbound brings every preview network's approvals on the egress
// proxy up to date: the current approvals for trusted previews, none for
// anything else. It withdraws removed approvals from running previews within
// a minute and restores them after the egress container restarts.
func (r *Runtime) RefreshOutbound(ctx context.Context) error {
	if r.egress == nil || r.destinations == nil || r.outboundTargets == nil || !r.egress.Forwarding() {
		return nil
	}
	previews, err := r.outboundTargets.OutboundPreviews(ctx)
	if err != nil {
		return err
	}
	var repos []int64
	for _, p := range previews {
		if p.Trusted && !slices.Contains(repos, p.RepoID) {
			repos = append(repos, p.RepoID)
		}
	}
	approved := map[int64][]egress.Destination{}
	if len(repos) > 0 {
		if approved, err = r.destinations.Destinations(ctx, repos...); err != nil {
			return fmt.Errorf("read approved destinations: %w", err)
		}
	}
	desired := map[netip.Addr][]egress.Destination{}
	for _, p := range previews {
		if !p.Trusted || len(approved[p.RepoID]) == 0 {
			continue
		}
		addr, ok, err := r.egress.Address(ctx, r.networkName(p.PullRequestID))
		if cerrdefs.IsNotFound(err) || err == nil && !ok {
			continue // No network yet, or the proxy has not joined it.
		}
		if err != nil {
			return err
		}
		desired[addr] = approved[p.RepoID]
	}
	return r.egress.SyncAllow(ctx, desired)
}
