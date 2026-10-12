package builds

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/platform/egress"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// These tests build real images on the local Docker Engine. They need Docker,
// the egress image (make egress-image) and internet access for alpine.
// Run them with make test-builder.

// memorySink records build output like the log store would receive it.
type memorySink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (m *memorySink) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buf.Write(p)
}

func (m *memorySink) System(message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buf.WriteString("[previa] " + message + "\n")
}

func (m *memorySink) String() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buf.String()
}

type realFixture struct {
	builder *Builder
	docker  *client.Client
	base    int64 // unique per test run, so caches never collide
}

func newRealFixture(t *testing.T, tweak func(*Config)) *realFixture {
	t.Helper()
	if os.Getenv("TEST_BUILDER") != "1" {
		t.Skip("set TEST_BUILDER=1 (make test-builder) to build real images")
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
	cfg := ConfigFrom(settings.BuilderConfig, "tb"+strconv.FormatInt(time.Now().UnixNano(), 36), 10*time.Minute)
	// These builds are tiny. Production disk thresholds would refuse the
	// concurrent pair below on a CI runner with about 10 GiB free.
	cfg.MinFreeDisk = 1 << 30
	cfg.DiskBudget = 4 << 30
	if tweak != nil {
		tweak(&cfg)
	}
	b, err := NewBuilder(docker, gateway, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	f := &realFixture{builder: b, docker: docker, base: 800_000_000 + time.Now().UnixNano()%100_000_000}
	t.Cleanup(func() { f.cleanup(t) })
	return f
}

// job writes a build context and returns a job for it.
func (f *realFixture) job(t *testing.T, pr, revision, headRepo int64, files map[string]string) (Job, *memorySink) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sink := &memorySink{}
	return Job{Instance: f.builder.cfg.Instance, PullRequestID: f.base + pr, Generation: 1, Revision: revision, HeadSHA: fmt.Sprintf("%040x", f.base+pr*1000+revision),
		BaseRepoID: f.base, HeadRepoID: headRepo, ContextDir: dir, Dockerfile: "Dockerfile", Logs: sink}, sink
}

// cleanup removes everything the test's PRs and scopes left behind.
func (f *realFixture) cleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for pr := int64(1); pr <= 10; pr++ {
		artifacts, err := f.builder.Resources(ctx, f.base+pr)
		if err != nil {
			t.Errorf("list leftovers: %v", err)
			continue
		}
		for _, a := range artifacts {
			if err := f.builder.Remove(ctx, a); err != nil {
				t.Errorf("remove %s: %v", a.Kind, err)
			}
		}
	}
	volumes, err := f.docker.VolumeList(ctx, client.VolumeListOptions{Filters: client.Filters{}.Add("label", dockerx.LabelBaseRepo+"="+itoa(f.base))})
	if err == nil {
		for _, v := range volumes.Items {
			if _, err := f.docker.VolumeRemove(ctx, v.Name, client.VolumeRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
				t.Errorf("remove cache volume: %v", err)
			}
		}
	}
}

// assertNoSandboxes checks that a finished build left no builder or network.
func (f *realFixture) assertNoSandboxes(t *testing.T, pr int64) {
	t.Helper()
	artifacts, err := f.builder.Resources(t.Context(), pr)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range artifacts {
		if a.Kind != dockerx.KindImage {
			t.Fatalf("build left a %s behind: %s", a.Kind, a.ID)
		}
	}
}

func buildCtx(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), d)
	t.Cleanup(cancel)
	return ctx
}

func TestRealBuilderBuildsCachesReusesAndLabels(t *testing.T) {
	f := newRealFixture(t, nil)
	files := map[string]string{
		"Dockerfile": "FROM alpine:3.22\nRUN echo installing && sleep 1 && echo done > /built.txt\nCOPY app.txt /app.txt\nRUN if [ -n \"$HTTPS_PROXY\" ]; then echo STEP_HAS_PROXY; fi\nCMD [\"cat\", \"/app.txt\"]\n",
		"app.txt":    "hello preview\n",
	}
	first, firstLogs := f.job(t, 1, 1, f.base, files)
	img, err := f.builder.Build(buildCtx(t, 8*time.Minute), first)
	if err != nil {
		t.Fatalf("%v\n%s", err, firstLogs)
	}
	if img.Ref != first.ImageRef() || img.ID == "" || img.Reused || !strings.Contains(firstLogs.String(), "STEP_HAS_PROXY") ||
		!strings.Contains(firstLogs.String(), "[previa] Network: downloaded ") {
		t.Fatalf("first build: %+v\n%s", img, firstLogs)
	}
	inspected, err := f.docker.ImageInspect(t.Context(), img.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.Config.Labels[dockerx.LabelPR] != itoa(first.PullRequestID) || inspected.Config.Labels[dockerx.LabelRevision] != "1" || inspected.Config.Labels[dockerx.LabelSHA] != first.HeadSHA {
		t.Fatalf("image labels: %v", inspected.Config.Labels)
	}
	for _, env := range inspected.Config.Env {
		if strings.Contains(strings.ToLower(env), "proxy") {
			t.Fatalf("proxy settings leaked into the image: %s", env)
		}
	}
	f.assertNoSandboxes(t, first.PullRequestID)

	// A same-SHA rebuild is a new revision: a new image, from the scope's cache.
	second, secondLogs := f.job(t, 1, 2, f.base, files)
	second.HeadSHA = first.HeadSHA
	img2, err := f.builder.Build(buildCtx(t, 8*time.Minute), second)
	if err != nil {
		t.Fatalf("%v\n%s", err, secondLogs)
	}
	if img2.Ref == img.Ref || !strings.Contains(secondLogs.String(), "CACHED") {
		t.Fatalf("rebuild did not use the cache or reused the tag: %s\n%s", img2.Ref, secondLogs)
	}
	// A River retry of the same revision finds the finished image.
	retry, retryLogs := f.job(t, 1, 2, f.base, files)
	retry.HeadSHA = first.HeadSHA
	// An identical revision/SHA must not adopt an image from another source.
	otherSource := retry
	otherSource.HeadRepoID = 0
	if _, ok := f.builder.reusable(t.Context(), otherSource); ok {
		t.Fatal("an unverified source reused a trusted image")
	}
	otherSource.HeadRepoID, otherSource.BaseRepoID = retry.HeadRepoID, retry.BaseRepoID+1
	if _, ok := f.builder.reusable(t.Context(), otherSource); ok {
		t.Fatal("a different base repository reused a trusted image")
	}
	reused, err := f.builder.Build(buildCtx(t, time.Minute), retry)
	if err != nil || !reused.Reused || reused.ID != img2.ID || !strings.Contains(retryLogs.String(), "Reusing the image") {
		t.Fatalf("retry: %+v %v\n%s", reused, err, retryLogs)
	}
	artifacts, err := f.builder.Resources(t.Context(), first.PullRequestID)
	if err != nil || len(artifacts) != 2 {
		t.Fatalf("expected two revision images: %+v %v", artifacts, err)
	}
}

func TestRealBuilderIsolatesBuildSteps(t *testing.T) {
	f := newRealFixture(t, nil)
	ctx := t.Context()
	// A service on Docker's default bridge stands in for Postgres, the control
	// plane and other previews.
	if err := pullImage(ctx, f.docker, "alpine:3.22"); err != nil {
		t.Fatal(err)
	}
	service, err := f.docker.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{Image: "alpine:3.22",
			Cmd: []string{"sh", "-c", `while true; do printf 'HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok' | nc -l -p 80; done`}},
		HostConfig: &container.HostConfig{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.docker.ContainerRemove(context.Background(), service.ID, client.ContainerRemoveOptions{Force: true})
	})
	if _, err := f.docker.ContainerStart(ctx, service.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}
	var serviceIP string
	for deadline := time.Now().Add(10 * time.Second); serviceIP == "" && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		inspected, err := f.docker.ContainerInspect(ctx, service.ID, client.ContainerInspectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if inspected.Container.State == nil || !inspected.Container.State.Running {
			continue
		}
		for _, endpoint := range inspected.Container.NetworkSettings.Networks {
			if endpoint != nil && endpoint.IPAddress.IsValid() {
				serviceIP = endpoint.IPAddress.String()
			}
		}
	}
	if serviceIP == "" {
		t.Fatal("probe service has no address")
	}
	probe := func(name, command string) string {
		return fmt.Sprintf("(%s >/dev/null 2>&1 && echo 'PROBE %s REACHED') || echo 'PROBE %s blocked'; ", command, name, name)
	}
	script := probe("direct-internet", "wget -T 5 -Y off -q -O- http://example.com") +
		probe("direct-service", "wget -T 3 -Y off -q -O- http://"+serviceIP) +
		probe("proxied-service", "wget -T 5 -q -O- http://"+serviceIP) +
		probe("direct-host", "wget -T 3 -Y off -q -O- http://host.docker.internal") +
		probe("proxied-host", "wget -T 5 -q -O- http://host.docker.internal") +
		probe("direct-metadata", "wget -T 3 -Y off -q -O- http://169.254.169.254/latest/meta-data/") +
		probe("proxied-metadata", "wget -T 5 -q -O- http://169.254.169.254/latest/meta-data/") +
		probe("proxied-public", "wget -T 15 -q -O- http://example.com") +
		"(test -S /var/run/docker.sock && echo 'PROBE docker-socket REACHED') || echo 'PROBE docker-socket blocked'; " +
		"(env | grep -iE 'token|secret|password|github|database' >/dev/null && echo 'PROBE credentials REACHED') || echo 'PROBE credentials blocked'"
	job, logs := f.job(t, 2, 1, f.base, map[string]string{"Dockerfile": "FROM alpine:3.22\nRUN " + script + "\n"})
	if _, err := f.builder.Build(buildCtx(t, 8*time.Minute), job); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	output := logs.String()
	for _, name := range []string{"direct-internet", "direct-service", "proxied-service", "direct-host", "proxied-host", "direct-metadata", "proxied-metadata", "docker-socket", "credentials"} {
		if !strings.Contains(output, "PROBE "+name+" blocked") {
			t.Errorf("build step reached %s:\n%s", name, output)
		}
	}
	if !strings.Contains(output, "PROBE proxied-public REACHED") {
		t.Errorf("the documented public route through the proxy failed:\n%s", output)
	}
	for _, line := range strings.Split(output, "\n") {
		if _, probe, found := strings.Cut(line, "PROBE "); found && !strings.Contains(line, "echo") {
			t.Log("probe:", probe)
		}
	}
	f.assertNoSandboxes(t, job.PullRequestID)
}

// With BUILD_EGRESS=registries a build installs packages and pulls its base
// image, but reaches no other host, and its log says which it refused.
func TestRealBuilderReachesOnlyRegistriesWhenLimited(t *testing.T) {
	f := newRealFixture(t, func(c *Config) { c.EgressHosts = egressHosts(config.BuildEgressRegistries, nil) })
	script := "apk add --no-cache jq >/dev/null && echo 'PROBE registry REACHED'; " +
		"(wget -T 15 -q -O- http://example.com >/dev/null 2>&1 && echo 'PROBE other-host REACHED') || echo 'PROBE other-host blocked'"
	job, logs := f.job(t, 5, 1, f.base, map[string]string{"Dockerfile": "FROM alpine:3.22\nRUN " + script + "\n"})
	if _, err := f.builder.Build(buildCtx(t, 8*time.Minute), job); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	output := logs.String()
	for _, want := range []string{"PROBE registry REACHED", "PROBE other-host blocked", "[previa] Network: downloaded ",
		"It refused 1 request(s) to other hosts: example.com."} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %q:\n%s", want, output)
		}
	}
	f.assertNoSandboxes(t, job.PullRequestID)
}

// A Dockerfile step reads an allowed build secret through a secret mount;
// the value never reaches the image, and without it the step fails.
func TestRealBuilderMountsBuildSecrets(t *testing.T) {
	f := newRealFixture(t, nil)
	const value = "build-secret-value-42"
	files := map[string]string{"Dockerfile": "FROM alpine:3.22\n" +
		"RUN --mount=type=secret,id=NPM_TOKEN,required=true sha256sum /run/secrets/NPM_TOKEN\n" +
		"RUN test ! -e /run/secrets/NPM_TOKEN && echo IMAGE_CLEAN\n"}
	job, logs := f.job(t, 6, 1, f.base, files)
	job.Secrets = map[string][]byte{"NPM_TOKEN": []byte(value)}
	img, err := f.builder.Build(buildCtx(t, 8*time.Minute), job)
	if err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	if out := logs.String(); !strings.Contains(out, fmt.Sprintf("%x", sha256.Sum256([]byte(value)))) || !strings.Contains(out, "IMAGE_CLEAN") || strings.Contains(out, value) {
		t.Fatalf("secret mount:\n%s", out)
	}
	inspected, err := f.docker.ImageInspect(t.Context(), img.Ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range inspected.Config.Env {
		if strings.Contains(env, value) {
			t.Fatal("the build secret reached the image's environment")
		}
	}
	without, logs := f.job(t, 7, 1, f.base+1, files)
	// Deliberately give a fork credentials: the builder must withhold them
	// and use a separate cache instead of a trusted cached secret step.
	without.Secrets = job.Secrets
	if _, err := f.builder.Build(buildCtx(t, 8*time.Minute), without); CodeOf(err) != CodeBuildFailed {
		t.Fatalf("a step needing a secret the build was not given must fail: %v\n%s", err, logs)
	}
}

func pullImage(ctx context.Context, docker *client.Client, ref string) error {
	if _, err := docker.ImageInspect(ctx, ref); err == nil {
		return nil
	}
	res, err := docker.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = res.Close() }()
	_, err = io.Copy(io.Discard, res)
	return err
}

func TestRealBuilderEnforcesLimitsAndClassifiesFailures(t *testing.T) {
	cases := []struct {
		name       string
		tweak      func(*Config)
		dockerfile string
		timeout    time.Duration
		cancel     time.Duration
		code       string
		log        string
	}{
		{name: "malformed Dockerfile", dockerfile: "FORM alpine:3.22\n", code: CodeBuildFailed, log: "Build failed"},
		{name: "failing step", dockerfile: "FROM alpine:3.22\nRUN echo about to fail && exit 3\n", code: CodeBuildFailed, log: "exit code: 3"},
		{name: "memory limit", tweak: func(c *Config) { c.Memory = 256 << 20 }, dockerfile: "FROM alpine:3.22\nRUN awk 'BEGIN { s = \"x\"; while (1) s = s s }'\n", code: CodeBuildFailed, log: "memory limit"},
		{name: "image size limit", tweak: func(c *Config) { c.MaxImageSize = 16 << 20 }, dockerfile: "FROM alpine:3.22\nRUN head -c 40000000 /dev/urandom > /blob\n", code: CodeImageTooLarge, log: "limit"},
		{name: "disk budget", tweak: func(c *Config) { c.DiskBudget = 256 << 20 }, dockerfile: "FROM alpine:3.22\nRUN head -c 400000000 /dev/zero > /big\nRUN sleep 30\n", code: CodeDiskBudget, log: "stored more than"},
		{name: "timeout", dockerfile: "FROM alpine:3.22\nRUN sleep 120\n", timeout: 25 * time.Second, code: CodeTimeout, log: "time limit"},
		{name: "cancellation", dockerfile: "FROM alpine:3.22\nRUN sleep 120\n", cancel: 20 * time.Second, code: CodeCancelled, log: "cancelled"},
		{name: "huge output", dockerfile: "FROM alpine:3.22\nRUN yes 'a noisy build line' | head -n 100000\n", code: "", log: "noisy build line"},
	}
	for i, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			f := newRealFixture(t, tt.tweak)
			job, logs := f.job(t, int64(3+i%7), 1, f.base, map[string]string{"Dockerfile": tt.dockerfile})
			timeout := 8 * time.Minute
			if tt.timeout > 0 {
				timeout = tt.timeout
			}
			ctx, cancel := context.WithTimeout(t.Context(), timeout)
			defer cancel()
			if tt.cancel > 0 {
				time.AfterFunc(tt.cancel, cancel)
			}
			started := time.Now()
			_, err := f.builder.Build(ctx, job)
			if CodeOf(err) != tt.code || (tt.code == "" && err != nil) {
				t.Fatalf("code %q (%v), want %q\n%s", CodeOf(err), err, tt.code, logs)
			}
			if !strings.Contains(logs.String(), tt.log) {
				t.Fatalf("log missing %q:\n%s", tt.log, logs)
			}
			if tt.timeout > 0 && time.Since(started) > tt.timeout+cleanupTimeout {
				t.Fatalf("timeout not enforced: %v", time.Since(started))
			}
			f.assertNoSandboxes(t, job.PullRequestID)
			if tt.code != "" {
				if _, err := f.docker.ImageInspect(t.Context(), job.ImageRef()); err == nil {
					t.Fatal("a failed build left its image behind")
				}
			}
		})
	}
}

func TestRealBuilderSeparatesForkCachesAndSharesNone(t *testing.T) {
	f := newRealFixture(t, nil)
	files := map[string]string{"Dockerfile": "FROM alpine:3.22\nRUN echo scope > /scope\n"}
	trusted, trustedLogs := f.job(t, 8, 1, f.base, files)
	fork, forkLogs := f.job(t, 9, 1, f.base+1, files)
	if _, err := f.builder.Build(buildCtx(t, 8*time.Minute), trusted); err != nil {
		t.Fatalf("%v\n%s", err, trustedLogs)
	}
	if _, err := f.builder.Build(buildCtx(t, 8*time.Minute), fork); err != nil {
		t.Fatalf("%v\n%s", err, forkLogs)
	}
	if strings.Contains(forkLogs.String(), "CACHED") && strings.Contains(forkLogs.String(), "echo scope") {
		t.Fatalf("a fork build reused the trusted scope's cache:\n%s", forkLogs)
	}
	for _, scope := range []string{trusted.cacheScope(), fork.cacheScope()} {
		if _, err := f.docker.VolumeInspect(t.Context(), CacheVolumeName(f.builder.cfg.Instance, scope), client.VolumeInspectOptions{}); err != nil {
			t.Fatalf("cache volume for %s: %v", scope, err)
		}
	}
	// Two builds of one scope at once: the second runs without the cache
	// instead of sharing or waiting for it.
	a, aLogs := f.job(t, 10, 1, f.base, map[string]string{"Dockerfile": "FROM alpine:3.22\nRUN sleep 8\n"})
	b, bLogs := f.job(t, 10, 2, f.base, map[string]string{"Dockerfile": "FROM alpine:3.22\nRUN sleep 8 && echo second\n"})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, job := range []Job{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = f.builder.Build(buildCtx(t, 8*time.Minute), job)
		}()
		time.Sleep(2 * time.Second)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("concurrent builds: %v %v\n%s\n%s", errs[0], errs[1], aLogs, bLogs)
	}
	if !strings.Contains(aLogs.String()+bLogs.String(), "runs without it") {
		t.Fatalf("one concurrent build should have run without the cache:\n%s\n%s", aLogs, bLogs)
	}
}
