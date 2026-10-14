package runtime

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/moby/moby/api/types/container"
)

const (
	// unprivilegedUser replaces root when the operator has not allowed root.
	unprivilegedUser = "65532:65532"
	// maxRestarts is how often Docker restarts a crashed preview before it is
	// left stopped and counted as a crash loop.
	maxRestarts = 3
	stopTimeout = 10
	// cpuWeight is half Docker's default, so platform containers win CPU
	// contention; oomPriority makes the kernel kill previews first.
	cpuWeight   = 512
	oomPriority = 500
)

// Policy is the operator's hardening and capacity configuration.
type Policy struct {
	AllowRoot      bool
	WritableRootfs bool
	TmpfsBytes     int64
	PIDs           int64
	OCIRuntime     string
	StartupTimeout time.Duration
	SetupTimeout   time.Duration // a preview's setup command
	MemoryBudget   int64         // 0: derived from the Docker host
	CPUBudget      float64       // CPUs; 0: derived from the Docker host
	MaxPerRepo     int
	MaxPerAccount  int // running previews per account; 0: no limit
}

// candidate is everything needed to create one preview container.
type candidate struct {
	Name      string
	ImageID   string
	ImageUser string
	Port      int
	Env       []string // YAML and authorized secrets, already merged
	ProxyURL  string
	CPUMillis int64
	Memory    int64
	NetworkID string
	// ExtraHosts map approved destinations' names to the egress proxy
	// (host:address), so the app keeps using the real name.
	ExtraHosts []string
	Labels     map[string]string
}

// runAs keeps a non-root image user and replaces root, or an unset user (which
// Docker runs as root), with an unprivileged UID unless the operator allows
// root. It reports whether it replaced the image's user.
func runAs(imageUser string, allowRoot bool) (string, bool) {
	if allowRoot {
		return imageUser, false
	}
	name, _, _ := strings.Cut(strings.TrimSpace(imageUser), ":")
	if name == "" || name == "root" || name == "0" {
		return unprivilegedUser, true
	}
	return imageUser, false
}

// environment adds Previa's variables to the app's. Egress settings always
// win, so neither YAML nor secrets can point the app at another proxy; PORT
// and HOME are defaults the app may override.
func environment(app []string, proxyURL string, port int, replacedUser bool) []string {
	values := map[string]string{}
	for _, entry := range app {
		if name, value, ok := strings.Cut(entry, "="); ok && name != "" {
			values[name] = value
		}
	}
	if _, ok := values["PORT"]; !ok {
		values["PORT"] = strconv.Itoa(port)
	}
	if _, ok := values["HOME"]; !ok && replacedUser {
		values["HOME"] = "/tmp" // The replacement UID has no home directory.
	}
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		values[name] = proxyURL
	}
	values["NO_PROXY"], values["no_proxy"] = "localhost,127.0.0.1", "localhost,127.0.0.1"
	values["NODE_USE_ENV_PROXY"] = "1" // Node's fetch honours proxy variables only with this.
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	env := make([]string, len(names))
	for i, name := range names {
		env[i] = name + "=" + values[name]
	}
	return env
}

// containerSpec is the hardened Docker configuration for a preview: no
// privileges, capabilities, host paths, devices or published ports; an
// unprivileged user and read-only root unless the operator opted out; bounded
// CPU, memory, processes, temporary space and logs; IPv6 disabled; and only the
// preview's own network.
func containerSpec(c candidate, p Policy) (*container.Config, *container.HostConfig, bool) {
	user, replaced := runAs(c.ImageUser, p.AllowRoot)
	timeout := stopTimeout
	config := &container.Config{
		Image:       c.ImageID,
		User:        user,
		Env:         environment(c.Env, c.ProxyURL, c.Port, replaced),
		Labels:      c.Labels,
		Hostname:    "preview",
		StopTimeout: &timeout,
	}
	pids := p.PIDs
	host := &container.HostConfig{
		NetworkMode:    container.NetworkMode(c.NetworkID),
		RestartPolicy:  container.RestartPolicy{Name: container.RestartPolicyOnFailure, MaximumRetryCount: maxRestarts},
		ReadonlyRootfs: !p.WritableRootfs,
		Tmpfs:          map[string]string{"/tmp": fmt.Sprintf("rw,nosuid,nodev,size=%d,mode=1777", p.TmpfsBytes)},
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		Sysctls: map[string]string{
			"net.ipv6.conf.all.disable_ipv6":     "1",
			"net.ipv6.conf.default.disable_ipv6": "1",
			// Lets the unprivileged user listen on ports below 1024.
			"net.ipv4.ip_unprivileged_port_start": "0",
		},
		Resources: container.Resources{
			NanoCPUs: c.CPUMillis * 1e6, Memory: c.Memory, MemorySwap: c.Memory,
			PidsLimit: &pids, CPUShares: cpuWeight,
		},
		OomScoreAdj: oomPriority,
		LogConfig:   previewLogConfig,
		Runtime:     p.OCIRuntime,
		ExtraHosts:  c.ExtraHosts,
	}
	return config, host, replaced
}

// previewLabels identifies a candidate for inventory, ownership checks,
// capacity accounting and the router's pre-publication check.
func previewLabels(instance string, t Target, request requestIdentity, imageRef string, port int, healthPath string, cpuMillis, memory int64, now time.Time) map[string]string {
	labels := map[string]string{
		dockerx.LabelManaged: "true", dockerx.LabelInstance: instance, dockerx.LabelKind: dockerx.KindPreview,
		dockerx.LabelPR: strconv.FormatInt(request.PullRequestID, 10), dockerx.LabelGeneration: strconv.FormatInt(request.Generation, 10),
		dockerx.LabelRevision: strconv.FormatInt(request.Revision, 10), dockerx.LabelSHA: request.HeadSHA,
		dockerx.LabelBaseRepo: strconv.FormatInt(t.BaseRepoID, 10), dockerx.LabelHeadRepo: strconv.FormatInt(t.HeadRepoID, 10),
		dockerx.LabelEnvironment: strconv.FormatInt(request.EnvironmentID, 10), dockerx.LabelImage: imageRef,
		dockerx.LabelPort: strconv.Itoa(port), dockerx.LabelCPUMillis: strconv.FormatInt(cpuMillis, 10),
		dockerx.LabelMemory: strconv.FormatInt(memory, 10), dockerx.LabelStarted: now.UTC().Format(time.RFC3339),
	}
	if healthPath != "" {
		labels[dockerx.LabelHealthPath] = healthPath
	}
	if t.WorkspaceID > 0 {
		labels[dockerx.LabelAccount] = strconv.FormatInt(t.WorkspaceID, 10)
		labels[dockerx.LabelWorkspace] = strconv.FormatInt(t.WorkspaceID, 10)
	}

	return labels
}

// requestIdentity is the admitted request and attempt a candidate belongs to.
type requestIdentity struct {
	PullRequestID, Generation, Revision, EnvironmentID int64
	HeadSHA                                            string
}

// previewLogConfig bounds what Docker keeps of each preview container's
// output: two 5 MB files (10 MB), removed with the container. The app,
// each managed service and each process is one container.
var previewLogConfig = container.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "5m", "max-file": "2"}}
