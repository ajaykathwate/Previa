package runtime

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/repoconfig"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
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
