package runtime

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/builds"
	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/platform/egress"
	"github.com/ajaykathwatee/previa/internal/repoconfig"
	"github.com/ajaykathwatee/previa/internal/secrets"
	"github.com/ajaykathwatee/previa/internal/source"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// These tests start real previews on the local Docker Engine. They need
// Docker, the egress image (make egress-image) and internet access for alpine.
// Run them with make test-runtime.

// realBase keeps PR and repository IDs unique per run; one base per run lets
// the build cache serve every test.
var realBase = 700_000_000 + time.Now().UnixNano()%100_000_000

const secretValue = "tok_live_supersecret_7f3a91"

// TestMain removes the build caches the real tests created, once they all ran.
func TestMain(m *testing.M) {
	code := m.Run()
	if os.Getenv("TEST_RUNTIME") == "1" {
		removeTestCaches()
	}
	os.Exit(code)
}

func removeTestCaches() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	docker, err := dockerx.New(ctx)
	if err != nil {
		return
	}
	defer func() { _ = docker.Close() }()
	volumes, err := docker.VolumeList(ctx, client.VolumeListOptions{Filters: client.Filters{}.Add("label",
		dockerx.LabelKind+"="+dockerx.KindBuildCache, dockerx.LabelBaseRepo+"="+fmt.Sprint(realBase))})
	if err != nil {
		return
	}
	for _, v := range volumes.Items {
		_, _ = docker.VolumeRemove(ctx, v.Name, client.VolumeRemoveOptions{Force: true})
	}
}

const appScript = `#!/bin/sh
echo "UID=$(id -u)"
if touch /rootfile 2>/dev/null; then echo "ROOTFS=writable"; else echo "ROOTFS=read-only"; fi
if touch /tmp/probe 2>/dev/null; then echo "TMP=writable"; fi
echo "CAPEFF=$(grep CapEff /proc/self/status | awk '{print $2}')"
echo "NONEWPRIVS=$(grep NoNewPrivs /proc/self/status | awk '{print $2}')"
if [ "$(ip -6 addr 2>/dev/null | grep -c inet6)" = "0" ]; then echo "IPV6=none"; else echo "IPV6=present"; fi
if [ -n "$API_TOKEN" ]; then echo "TOKEN=present value=$API_TOKEN"; else echo "TOKEN=absent"; fi
if [ -n "$PROBES" ]; then sh /probes.sh; fi
if [ -n "$CRASH" ]; then echo "crashing on purpose"; exit 3; fi
if [ -n "$SILENT" ]; then exec sleep 3600; fi
while true; do
  printf "HTTP/1.1 ${STATUS:-200 OK}\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok" | nc -l -p "$PORT" >/dev/null 2>&1
done
`

const probeScript = `#!/bin/sh
probe() { if sh -c "$2" >/dev/null 2>&1; then echo "PROBE $1 REACHED"; else echo "PROBE $1 blocked"; fi; }
probe direct-internet "wget -T 5 -Y off -q -O- http://example.com"
probe proxied-public "wget -T 15 -q -O- http://example.com"
probe direct-service "wget -T 3 -Y off -q -O- http://$SERVICE_IP"
probe proxied-service "wget -T 5 -q -O- http://$SERVICE_IP"
probe direct-other-preview "wget -T 3 -Y off -q -O- http://$OTHER_IP:3000/health"
probe proxied-other-preview "wget -T 5 -q -O- http://$OTHER_IP:3000/health"
probe direct-host "wget -T 3 -Y off -q -O- http://host.docker.internal"
probe proxied-host "wget -T 5 -q -O- http://host.docker.internal"
probe direct-metadata "wget -T 3 -Y off -q -O- http://169.254.169.254/latest/meta-data/"
probe proxied-metadata "wget -T 5 -q -O- http://169.254.169.254/latest/meta-data/"
probe dns-external "nslookup example.com"
`

var appFiles = map[string]string{
	"Dockerfile": "FROM alpine:3.22\nCOPY app.sh probes.sh /\nCMD [\"sh\", \"/app.sh\"]\n",
	"app.sh":     appScript,
	"probes.sh":  probeScript,
}

// memorySealed is a SealedStore holding values sealed with a real keyring.
type memorySealed struct {
	mu     sync.Mutex
	values map[int64][]secrets.SealedSecret
}

func (m *memorySealed) SealedSecrets(_ context.Context, repoID int64) ([]secrets.SealedSecret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]secrets.SealedSecret(nil), m.values[repoID]...), nil
}

// dirSources prepares a local tree instead of a GitHub archive.
type dirSources struct {
	t        *testing.T
	settings repoconfig.Config
}

func (d dirSources) Prepare(context.Context, source.Request) (*source.Prepared, error) {
	dir := d.t.TempDir()
	for name, body := range appFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			return nil, err
		}
	}
	return &source.Prepared{ContextDir: dir, Dockerfile: "Dockerfile", Config: repoconfig.Result{Config: d.settings}}, nil
}

func (dirSources) Release(*source.Prepared) error { return nil }

// redactingSink redacts whole writes, which is all captureOutput issues.
type redactingSink struct {
	inner    *memorySink
	redactor *secrets.Redactor
}

func (s redactingSink) Write(p []byte) (int, error) {
	if _, err := s.inner.Write(s.redactor.Redact(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}
func (s redactingSink) System(message string) {
	s.inner.System(string(s.redactor.Redact([]byte(message))))
}
func (s redactingSink) Close(context.Context) error { return nil }

type fixedTarget struct{ target Target }

func (f fixedTarget) Current(context.Context, environments.Request) (Target, error) {
	return f.target, nil
}

func (f fixedTarget) AdmissionScope(context.Context, int64) (Target, error) {
	return f.target, nil
}

// processLock serializes admissions within the test process, as
// AdmissionLock does across workers.
type processLock struct{ mu sync.Mutex }

func (l *processLock) Lock(context.Context) (func(), error) {
	l.mu.Lock()
	return l.mu.Unlock, nil
}

type realFixture struct {
	admission *processLock
	instance  string
	docker    *client.Client
	gateway   *egress.Gateway
	builder   *builds.Builder
	provider  *secrets.Provider
	settings  config.ExecutionConfig
}

func newRealFixture(t *testing.T) *realFixture {
	t.Helper()
	if os.Getenv("TEST_RUNTIME") != "1" {
		t.Skip("set TEST_RUNTIME=1 (make test-runtime) to start real previews")
	}
	docker, err := dockerx.New(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = docker.Close() })
	settings, err := config.LoadExecution()
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := egress.NewGateway(docker, settings.EgressImage)
	if err != nil {
		t.Fatal(err)
	}
	instance := "tr" + strconv.FormatInt(time.Now().UnixNano(), 36)
	builder, err := builds.NewBuilder(docker, gateway, builds.ConfigFrom(settings.BuilderConfig, instance, 10*time.Minute), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	keys, err := secrets.ParseKeyring("1:"+base64.StdEncoding.EncodeToString(key), "1")
	if err != nil {
		t.Fatal(err)
	}
	sealedValue, version, err := keys.Seal(42, "API_TOKEN", []byte(secretValue))
	if err != nil {
		t.Fatal(err)
	}
	provider, err := secrets.NewProvider(&memorySealed{values: map[int64][]secrets.SealedSecret{42: {{Name: "API_TOKEN", Value: sealedValue, Version: version}}}}, keys)
	if err != nil {
		t.Fatal(err)
	}
	return &realFixture{admission: &processLock{}, instance: instance, docker: docker, gateway: gateway, builder: builder, provider: provider, settings: settings}
}

// networkName is a PR's preview network for this fixture's instance.
func (f *realFixture) networkName(pullRequestID int64) string {
	return (&Runtime{instance: f.instance}).networkName(pullRequestID)
}

// target is PR pr of the test repository; fork gives it another head repository.
func (f *realFixture) target(pr int64, fork bool) Target {
	t := Target{PullRequestID: realBase + pr, RepoID: 42, InstallationID: 99, BaseRepoID: realBase, HeadRepoID: realBase}
	if fork {
		t.HeadRepoID, t.Fork = realBase+1, true
	}
	return t
}

func request(pr int64) environments.Request {
	return environments.Request{PullRequestID: realBase + pr, Generation: 1, Revision: 1, HeadSHA: fmt.Sprintf("%040x", realBase+pr), State: "open"}
}

// runtime assembles a Runtime for one test; cleanup removes the PR's objects.
func (f *realFixture) runtime(t *testing.T, target Target, settings repoconfig.Config, tweak func(*Policy)) (*Runtime, *memorySink) {
	t.Helper()
	policy := PolicyFrom(f.settings.PreviewConfig)
	if tweak != nil {
		tweak(&policy)
	}
	if settings.Port == 0 {
		settings.Port = 3000
	}
	if settings.Resources == (repoconfig.Resources{}) {
		settings.Resources = repoconfig.Resources{CPUMillis: 500, MemoryBytes: 256 << 20}
	}
	sink := &memorySink{}
	r, err := New(Parts{
		Instance: f.instance, Admission: f.admission, Docker: f.docker, Egress: f.gateway, Targets: fixedTarget{target}, Sources: dirSources{t: t, settings: settings}, Builder: f.builder, Secrets: f.provider,
		// Like buildlogs.Writer, the sink redacts with the redactor the runtime
		// hands it, so the tests prove that redactor knows the injected secrets.
		Logs: func(_ context.Context, _ environments.Attempt, redactor *secrets.Redactor) (LogSink, error) {
			return redactingSink{inner: sink, redactor: redactor}, nil
		},
	}, policy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.cleanup(t, r, target.PullRequestID) })
	return r, sink
}

func (f *realFixture) cleanup(t *testing.T, r *Runtime, pullRequestID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	resources, err := r.Resources(ctx, pullRequestID)
	if err != nil {
		t.Errorf("list leftovers: %v", err)
	}
	for _, res := range resources {
		if res.Artifact.ContainerID != "" {
			if err := r.RemoveContainer(ctx, res.Artifact.ContainerID); err != nil {
				t.Errorf("remove container: %v", err)
			}
		}
	}
	for _, res := range resources {
		if res.Artifact.ImageTag != "" {
			if err := r.RemoveImage(ctx, res.Artifact.ImageTag); err != nil {
				t.Errorf("remove image: %v", err)
			}
		}
	}
	name := r.networkName(pullRequestID)
	_ = f.gateway.Detach(ctx, name)
	if _, err := f.docker.NetworkRemove(ctx, name, client.NetworkRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
		t.Errorf("remove preview network: %v", err)
	}
}

func buildCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func requireLines(t *testing.T, output string, lines ...string) {
	t.Helper()
	for _, line := range lines {
		if !strings.Contains(output, line) {
			t.Errorf("log missing %q:\n%s", line, output)
		}
	}
}

func TestRealRuntimeStartsAHardenedHealthyCandidate(t *testing.T) {
	f := newRealFixture(t)
	target := f.target(1, false)
	r, logs := f.runtime(t, target, repoconfig.Config{HealthCheckPath: "/health", Env: []string{"MODE=preview"}}, nil)
	artifact, err := r.Build(buildCtx(t), request(1), environments.Attempt{EnvironmentID: 7, LogEpoch: 1})
	if err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	output := logs.String()
	requireLines(t, output, "UID=65532", "ROOTFS=read-only", "TMP=writable", "CAPEFF=0000000000000000", "NONEWPRIVS=1", "IPV6=none",
		"TOKEN=present value=[redacted]", "runs as the unprivileged user 65532", "The preview is healthy. It is not routed yet.")
	if strings.Contains(output, secretValue) {
		t.Fatal("a secret value reached the log")
	}
	inspected, err := f.docker.ContainerInspect(t.Context(), artifact.ContainerID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c, host := inspected.Container, inspected.Container.HostConfig
	switch {
	case c.Config.User != unprivilegedUser || !host.ReadonlyRootfs || host.Privileged || len(host.PortBindings) > 0 || len(host.Binds) > 0 || len(host.Mounts) > 0:
		t.Fatalf("hardening: user %q read-only %v privileged %v ports %v binds %v", c.Config.User, host.ReadonlyRootfs, host.Privileged, host.PortBindings, host.Binds)
	case !slices.Equal(host.CapDrop, []string{"ALL"}) || !slices.Contains(host.SecurityOpt, "no-new-privileges"):
		t.Fatalf("capabilities %v opts %v", host.CapDrop, host.SecurityOpt)
	case host.Memory != 256<<20 || host.NanoCPUs != 5e8 || host.PidsLimit == nil || *host.PidsLimit != 512 || host.CPUShares != cpuWeight || host.OomScoreAdj != oomPriority:
		t.Fatalf("limits %+v oom %d", host.Resources, host.OomScoreAdj)
	case c.Config.Labels[dockerx.LabelKind] != dockerx.KindPreview || c.Config.Labels[dockerx.LabelImage] != artifact.ImageTag || c.Config.Labels[dockerx.LabelEnvironment] != "7":
		t.Fatalf("labels %v", c.Config.Labels)
	}
	for key := range c.Config.Labels {
		if strings.HasPrefix(key, "traefik.") {
			t.Fatalf("a candidate must not be routed yet: %s", key)
		}
	}
	if _, ok := c.NetworkSettings.Networks[f.networkName(target.PullRequestID)]; !ok || len(c.NetworkSettings.Networks) != 1 {
		t.Fatalf("a preview joins only its own network: %v", c.NetworkSettings.Networks)
	}
	health, err := r.Health(t.Context(), artifact.ContainerID)
	if err != nil || !health.Running || health.CrashLoop {
		t.Fatalf("health: %+v %v", health, err)
	}
	resources, err := r.Resources(t.Context(), target.PullRequestID)
	if err != nil || len(resources) != 1 || resources[0].Artifact != artifact || resources[0].Revision != 1 {
		t.Fatalf("inventory: %+v %v", resources, err)
	}
	// Removal is idempotent and owner-verified; the PR's preview network goes
	// with its last preview.
	resource := environments.Resource{PullRequestID: target.PullRequestID, Generation: 1, Revision: 1, Artifact: artifact}
	for range 2 {
		if err := r.RemoveResource(t.Context(), resource); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.docker.NetworkInspect(t.Context(), f.networkName(target.PullRequestID), client.NetworkInspectOptions{}); !cerrdefs.IsNotFound(err) {
		t.Fatalf("idle preview network kept: %v", err)
	}
	if health, err := r.Health(t.Context(), artifact.ContainerID); err != nil || health.State != "missing" {
		t.Fatalf("after removal: %+v %v", health, err)
	}
	if resources, _ := r.Resources(t.Context(), target.PullRequestID); len(resources) != 0 {
		t.Fatalf("leftovers after removal: %+v", resources)
	}
}

func TestRealRuntimeWithholdsSecretsFromForks(t *testing.T) {
	f := newRealFixture(t)
	r, logs := f.runtime(t, f.target(2, true), repoconfig.Config{HealthCheckPath: "/health"}, nil)
	artifact, err := r.Build(buildCtx(t), request(2), environments.Attempt{})
	if err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	requireLines(t, logs.String(), "TOKEN=absent", "Repository secrets were withheld")
	inspected, err := f.docker.ContainerInspect(t.Context(), artifact.ContainerID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range inspected.Container.Config.Env {
		if strings.HasPrefix(env, "API_TOKEN=") {
			t.Fatal("a fork's preview received a repository secret")
		}
	}
}

func TestRealRuntimeDeniesNetworkAccess(t *testing.T) {
	f := newRealFixture(t)
	ctx := t.Context()
	// A service on Docker's default bridge stands in for Postgres and the
	// control plane; another PR's preview stands in for other previews.
	service, err := f.docker.ContainerCreate(ctx, client.ContainerCreateOptions{Config: &container.Config{Image: "alpine:3.22",
		Cmd: []string{"sh", "-c", `while true; do printf 'HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok' | nc -l -p 80; done`}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.docker.ContainerRemove(context.Background(), service.ID, client.ContainerRemoveOptions{Force: true})
	})
	if _, err := f.docker.ContainerStart(ctx, service.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}
	serviceIP := addressOf(t, f.docker, service.ID, "")
	other, otherLogs := f.runtime(t, f.target(3, false), repoconfig.Config{HealthCheckPath: "/health"}, nil)
	otherArtifact, err := other.Build(buildCtx(t), request(3), environments.Attempt{})
	if err != nil {
		t.Fatalf("%v\n%s", err, otherLogs)
	}
	otherIP := addressOf(t, f.docker, otherArtifact.ContainerID, f.networkName(realBase+3))
	r, logs := f.runtime(t, f.target(4, false), repoconfig.Config{HealthCheckPath: "/health", Env: []string{"PROBES=1", "SERVICE_IP=" + serviceIP, "OTHER_IP=" + otherIP}}, nil)
	if _, err := r.Build(buildCtx(t), request(4), environments.Attempt{}); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	output := logs.String()
	for _, name := range []string{"direct-internet", "direct-service", "proxied-service", "direct-other-preview", "proxied-other-preview",
		"direct-host", "proxied-host", "direct-metadata", "proxied-metadata", "dns-external"} {
		if !strings.Contains(output, "PROBE "+name+" blocked") {
			t.Errorf("a running preview reached %s:\n%s", name, output)
		}
	}
	requireLines(t, output, "PROBE proxied-public REACHED", "IPV6=none")
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "PROBE ") {
			t.Log("probe:", strings.TrimPrefix(line, "PROBE "))
		}
	}
}

func addressOf(t *testing.T, docker *client.Client, id, network string) string {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		inspected, err := docker.ContainerInspect(t.Context(), id, client.ContainerInspectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for name, endpoint := range inspected.Container.NetworkSettings.Networks {
			if (network == "" || name == network) && endpoint != nil && endpoint.IPAddress.IsValid() {
				return endpoint.IPAddress.String()
			}
		}
	}
	t.Fatal("container has no address")
	return ""
}

func TestRealRuntimeFailsUnhealthyCrashingAndCancelledStartsCleanly(t *testing.T) {
	f := newRealFixture(t)
	cases := []struct {
		name    string
		pr      int64
		env     []string
		startup time.Duration
		cancel  time.Duration
		code    string
		log     string
	}{
		{"unhealthy", 5, []string{"STATUS=500 Internal Server Error"}, 15 * time.Second, 0, CodeUnhealthy, "HTTP 500"},
		{"crash loop", 6, []string{"CRASH=1"}, 15 * time.Second, 0, CodeCrashed, "crashing on purpose"},
		// Cancelled while waiting for an app that never listens.
		{"cancelled", 7, []string{"SILENT=1"}, 2 * time.Minute, 20 * time.Second, CodeCancelled, "cancelled"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			target := f.target(tt.pr, false)
			r, logs := f.runtime(t, target, repoconfig.Config{HealthCheckPath: "/health", Env: tt.env}, func(p *Policy) { p.StartupTimeout = tt.startup })
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
			defer cancel()
			if tt.cancel > 0 {
				time.AfterFunc(tt.cancel, cancel)
			}
			_, err := r.Build(ctx, request(tt.pr), environments.Attempt{})
			if CodeOf(err) != tt.code || !strings.Contains(logs.String(), tt.log) {
				t.Fatalf("code %q (%v), want %q with log %q:\n%s", CodeOf(err), err, tt.code, tt.log, logs)
			}
			resources, err := r.Resources(t.Context(), target.PullRequestID)
			if err != nil {
				t.Fatal(err)
			}
			for _, res := range resources {
				if res.Artifact.ContainerID != "" {
					t.Fatalf("a failed candidate was left running: %+v", res)
				}
			}
		})
	}
}

func TestRealRuntimeHonoursOperatorOptOuts(t *testing.T) {
	f := newRealFixture(t)
	r, logs := f.runtime(t, f.target(8, false), repoconfig.Config{HealthCheckPath: "/health"}, func(p *Policy) { p.AllowRoot, p.WritableRootfs = true, true })
	if _, err := r.Build(buildCtx(t), request(8), environments.Attempt{}); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	requireLines(t, logs.String(), "UID=0", "ROOTFS=writable", "CAPEFF=0000000000000000", "NONEWPRIVS=1")
}

func TestRealRuntimeEnforcesCapacity(t *testing.T) {
	f := newRealFixture(t)
	small, logs := f.runtime(t, f.target(9, false), repoconfig.Config{HealthCheckPath: "/health"}, func(p *Policy) { p.MemoryBudget = 128 << 20 })
	if _, err := small.Build(buildCtx(t), request(9), environments.Attempt{}); CodeOf(err) != CodeCapacity {
		t.Fatalf("memory budget: %v\n%s", err, logs)
	}
	first, firstLogs := f.runtime(t, f.target(10, false), repoconfig.Config{HealthCheckPath: "/health"}, func(p *Policy) { p.MaxPerRepo = 1 })
	if _, err := first.Build(buildCtx(t), request(10), environments.Attempt{}); err != nil {
		t.Fatalf("%v\n%s", err, firstLogs)
	}
	second, secondLogs := f.runtime(t, f.target(11, false), repoconfig.Config{HealthCheckPath: "/health"}, func(p *Policy) { p.MaxPerRepo = 1 })
	if _, err := second.Build(buildCtx(t), request(11), environments.Attempt{}); CodeOf(err) != CodeCapacity {
		t.Fatalf("per-repository limit: %v\n%s", err, secondLogs)
	}
}

func TestRealRuntimeRefusesForeignObjectsAndFindsLeftovers(t *testing.T) {
	f := newRealFixture(t)
	ctx := t.Context()
	r, _ := f.runtime(t, f.target(12, false), repoconfig.Config{}, nil)
	foreign, err := f.docker.ContainerCreate(ctx, client.ContainerCreateOptions{Config: &container.Config{Image: "alpine:3.22", Cmd: []string{"sleep", "60"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.docker.ContainerRemove(context.Background(), foreign.ID, client.ContainerRemoveOptions{Force: true})
	})
	if err := r.RemoveContainer(ctx, foreign.ID); CodeOf(err) != CodeNotOwned {
		t.Fatalf("foreign container: %v", err)
	}
	if _, err := f.docker.ContainerInspect(ctx, foreign.ID, client.ContainerInspectOptions{}); err != nil {
		t.Fatal("a foreign container was removed")
	}
	if err := r.RemoveImage(ctx, "alpine:3.22"); CodeOf(err) != CodeNotOwned {
		t.Fatalf("foreign image: %v", err)
	}
	// A builder left behind by a crashed worker is found and removed.
	leftover, err := f.docker.ContainerCreate(ctx, client.ContainerCreateOptions{Config: &container.Config{Image: "alpine:3.22", Cmd: []string{"sleep", "60"},
		Labels: map[string]string{dockerx.LabelManaged: "true", dockerx.LabelInstance: f.instance, dockerx.LabelKind: dockerx.KindBuilder, dockerx.LabelPR: fmt.Sprint(realBase + 12), dockerx.LabelGeneration: "1", dockerx.LabelRevision: "1"}}})
	if err != nil {
		t.Fatal(err)
	}
	// Another Previa instance's object for the same PR ID is invisible and untouchable.
	neighbour, err := f.docker.ContainerCreate(ctx, client.ContainerCreateOptions{Config: &container.Config{Image: "alpine:3.22", Cmd: []string{"sleep", "60"},
		Labels: map[string]string{dockerx.LabelManaged: "true", dockerx.LabelInstance: "zz" + f.instance, dockerx.LabelKind: dockerx.KindBuilder, dockerx.LabelPR: fmt.Sprint(realBase + 12), dockerx.LabelGeneration: "1", dockerx.LabelRevision: "1"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.docker.ContainerRemove(context.Background(), neighbour.ID, client.ContainerRemoveOptions{Force: true})
	})
	if err := r.RemoveContainer(ctx, neighbour.ID); CodeOf(err) != CodeNotOwned {
		t.Fatalf("another instance's container: %v", err)
	}
	resources, err := r.Resources(ctx, realBase+12)
	if err != nil || len(resources) != 1 || resources[0].Artifact.ContainerID != leftover.ID {
		t.Fatalf("leftover builder not found: %+v %v", resources, err)
	}
	if err := r.RemoveContainer(ctx, leftover.ID); err != nil {
		t.Fatal(err)
	}
	// A same-named network Previa did not create is never used.
	squatter := f.networkName(realBase + 13)
	if _, err := f.docker.NetworkCreate(ctx, squatter, client.NetworkCreateOptions{Driver: "bridge"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = f.docker.NetworkRemove(context.Background(), squatter, client.NetworkRemoveOptions{}) })
	squatted, logs := f.runtime(t, f.target(13, false), repoconfig.Config{HealthCheckPath: "/health"}, nil)
	if _, err := squatted.Build(buildCtx(t), request(13), environments.Attempt{}); CodeOf(err) != CodeNotOwned {
		t.Fatalf("squatted network: %v\n%s", err, logs)
	}
}

// Two candidates race for capacity that fits one: the PostgreSQL admission
// lock lets exactly one start, whichever worker it runs on.
func TestRealRuntimeAdmitsConcurrentCandidatesAtomically(t *testing.T) {
	f := newRealFixture(t)
	lock := NewAdmissionLock(isolatedPool(t))
	settings := repoconfig.Config{Port: 8080, Resources: repoconfig.Resources{CPUMillis: 500, MemoryBytes: 256 << 20}}
	budget := func(p *Policy) { p.MemoryBudget, p.CPUBudget = 400<<20, 64 }
	first, firstLogs := f.runtime(t, f.target(20, false), settings, budget)
	second, secondLogs := f.runtime(t, f.target(21, false), settings, budget)
	first.admission, second.admission = lock, lock
	image := f.listeningImage(t)
	// Host capacity includes previews from every instance. Keep existing
	// previews running and leave room for exactly one test candidate above
	// their reservations, rather than assuming an empty Docker host.
	used, err := first.usage(t.Context(), f.target(20, false))
	if err != nil {
		t.Fatal(err)
	}
	first.policy.MemoryBudget += used.memory
	second.policy.MemoryBudget += used.memory
	results := make(chan error, 2)
	for i, r := range []*Runtime{first, second} {
		target := f.target(int64(20+i), false)
		id := requestIdentity{PullRequestID: target.PullRequestID, Generation: 1, Revision: 1, HeadSHA: request(int64(20 + i)).HeadSHA}
		sink := firstLogs
		if i == 1 {
			sink = secondLogs
		}
		plan, err := planBackend(fmt.Sprintf("previa-%s-pr%d-g1-r1-%d", r.instance, target.PullRequestID, time.Now().UnixNano()), settings)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			_, err := r.startCandidate(buildCtx(t), id, target, image, settings, nil, plan, dataPlan{}, sink)
			results <- err
		}()
	}
	var started, refused int
	for range 2 {
		switch err := <-results; {
		case err == nil:
			started++
		case CodeOf(err) == CodeCapacity:
			refused++
		default:
			t.Fatalf("unexpected start failure: %v\n%s\n%s", err, firstLogs, secondLogs)
		}
	}
	if started != 1 || refused != 1 {
		t.Fatalf("%d candidates started and %d were refused with room for one\n%s\n%s", started, refused, firstLogs, secondLogs)
	}
}

// listeningImage commits an image whose command answers TCP on port 8080.
func (f *realFixture) listeningImage(t *testing.T) builds.Image {
	t.Helper()
	ctx := t.Context()
	created, err := f.docker.ContainerCreate(ctx, client.ContainerCreateOptions{Config: &container.Config{Image: "alpine:3.22"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = f.docker.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	}()
	ref := "previa/" + f.instance + "-capacity:test"
	committed, err := f.docker.ContainerCommit(ctx, created.ID, client.ContainerCommitOptions{Reference: ref, Changes: []string{
		"LABEL " + dockerx.LabelManaged + "=true", "LABEL " + dockerx.LabelInstance + "=" + f.instance, "LABEL " + dockerx.LabelKind + "=" + dockerx.KindImage,
		`CMD ["sh", "-c", "while true; do echo ok | nc -l -p 8080 >/dev/null 2>&1; done"]`,
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.docker.ImageRemove(context.Background(), ref, client.ImageRemoveOptions{Force: true})
	})
	return builds.Image{Ref: ref, ID: committed.ID}
}
