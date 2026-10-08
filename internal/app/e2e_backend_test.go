package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/housekeeping"
	"github.com/ajaykathwatee/previa/internal/outbound"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/platform/egress"
	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/ajaykathwatee/previa/internal/pullrequests"
	"github.com/ajaykathwatee/previa/internal/source"
	"github.com/ajaykathwatee/previa/internal/webhooks"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/riverqueue/river"
)

const (
	backendCommit1 = "b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1"
	backendCommit2 = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	backendCommit3 = "b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3"
	forkCommit     = "f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8"
	forkPRNumber   = 8
)

// backendState is what the fixture's API answers at /.
type backendState struct {
	Marker    string   `json:"marker"`
	Identity  string   `json:"identity"`
	Items     int      `json:"items"`
	Processed []string `json:"processed"`
}

// TestRealBackendEndToEnd runs a backend repository through the worker's own
// pipeline on real Docker, BuildKit and Traefik: an HTTP API on managed
// PostgreSQL and Redis with a migration-and-seed setup command, a queue
// worker and a docs path. It proves that the preview becomes ready and serves
// over HTTPS, a rebuild gets a fresh database, a failed migration keeps the
// old preview serving, services are isolated both ways, approved outbound
// connections work while unapproved, private and DNS-to-metadata ones fail,
// a fork gets none, and crash leftovers and teardown leave no owned
// container, volume or network behind.
func TestRealBackendEndToEnd(t *testing.T) {
	if os.Getenv("TEST_E2E") != "1" {
		t.Skip("set TEST_E2E=1 (make test-e2e) to run the backend pipeline on the local Docker Engine")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Minute)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if os.Getenv("TEST_E2E_LOG") == "1" {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	pool := e2eSchema(t)
	docker, err := dockerx.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = docker.Close() })
	volumesBefore := e2eVolumes(t, ctx, docker)
	execution, err := config.LoadExecution()
	if err != nil {
		t.Fatal(err)
	}
	execution.SourceWorkspaceRoot = t.TempDir()
	router := startE2ERouter(t, docker)
	github := newFakeGitHub(t)
	binary := buildBackendFixture(t, ctx, docker)
	backendFiles := func(marker string, env ...string) map[string]string {
		yaml := "port: 8080\nresource_tier: small\nhealth_check_path: /health\nservices:\n  postgres: 17\n  redis: 7\nsetup: [backend, migrate]\n" +
			"processes:\n  - name: worker\n    command: [backend, worker]\ndocs_path: /docs\nenv:\n  - MARKER=" + marker + "\n"
		for _, e := range env {
			yaml += "  - " + e + "\n"
		}
		return map[string]string{".previewrc.yaml": yaml, "backend": binary,
			"Dockerfile": "FROM alpine:3.22\nCOPY --chmod=0755 backend /usr/local/bin/backend\nCMD [\"backend\", \"serve\"]\n"}
	}
	github.push(backendCommit1, backendFiles("b1"))

	cfg := config.WorkerConfig{
		ExecutionConfig: execution,
		RoutingConfig: config.RoutingConfig{PreviewBaseDomain: e2eDomain, AppBaseURL: "http://localhost:8080", RouterContainer: router.name,
			RouterDynamicDir: router.dynamic, RouterEntryPoint: "websecure", RouterAPIAddress: "127.0.0.1:8080"},
		HousekeepingConfig:  config.HousekeepingConfig{EnvTTLHours: 48, HousekeepingGrace: time.Minute, BuildCacheBudget: 40 << 30},
		MaxConcurrentBuilds: 1, BuildTimeout: 10 * time.Minute,
	}
	cfg.PreviewOutboundPorts = "22,5432,6380,9092"
	// Two backend previews and a swap exceed Docker Desktop's default share;
	// capacity has its own tests.
	cfg.PreviewMemoryBudget, cfg.PreviewCPUBudget = 24<<30, 32
	producer, err := queue.NewProducer(pool, logger)
	if err != nil {
		t.Fatal(err)
	}
	p, err := buildPipeline(ctx, pipelineInput{cfg: cfg, pool: pool, producer: producer, docker: docker, github: github, logger: logger,
		archiveOptions: []source.ArchiveOption{source.WithDownloadClient(github.server.Client()), source.WithArchiveHosts(github.Host())}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.capabilities.Services) == 0 {
		t.Fatalf("the worker offers no managed services: %+v", p.capabilities)
	}
	for _, step := range p.startup {
		if err := step(ctx); err != nil {
			t.Fatal(err)
		}
	}
	worker, err := queue.NewWorker(pool, cfg, logger, p.register...)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Start(context.WithoutCancel(ctx)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stopRiver(worker, logger) })
	instance, err := database.Instance(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e2eCleanup(t, docker, instance) })

	operations, err := environments.NewOperationBroker(producer)
	if err != nil {
		t.Fatal(err)
	}
	serverRepo := environments.NewRepository(pool, operations).WithReports()
	broker := environments.NewRequestBroker(pool, producer).WithReports(serverRepo)
	sync := pullrequests.NewSyncService(pool, producer, pullrequests.NewGitHubSource(github), broker)
	events := webhooks.NewService(pool, broker)
	var user, repoID int64
	e2eScan(t, pool, "INSERT INTO users(github_id, username) VALUES (1, 'owner') RETURNING id", &user)
	e2eScan(t, pool, `WITH i AS (INSERT INTO installations(github_installation_id, owner_user_id, status) VALUES (900, $1, 'active') RETURNING id)
		INSERT INTO repos(installation_id, full_name, github_repo_id) SELECT id, 'octo/app', $2 FROM i RETURNING id`, &repoID, user, e2eRepoID)

	// The owner approves destinations before building: a public SSH server,
	// a name that resolves to loopback, and one that resolves to the cloud
	// metadata address. Port 9092 is allowed but approves nothing.
	approvals, err := outbound.NewService(outbound.NewRepository(pool, producer), []int{22, 5432, 6380, 9092})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range [][2]string{{"github.com", "22"}, {"localtest.me", "5432"}, {"169.254.169.254.nip.io", "6380"}} {
		if _, err := approvals.Add(ctx, user, repoID, d[0], d[1]); err != nil {
			t.Fatalf("approve %s: %v", d[0], err)
		}
	}

	open := func(number int32, sha string) int64 {
		t.Helper()
		opened, err := events.ProcessPR(ctx, webhooks.PREvent{RepoID: repoID, Number: number, HeadSHA: sha, Action: "opened", MergeableState: "clean"})
		if err != nil {
			t.Fatal(err)
		}
		return opened.PullRequestID
	}
	keys := 0
	request := func(pr int64, action environments.IntentAction) {
		t.Helper()
		if _, err := sync.VerifyUpstream(ctx, pr); err != nil {
			t.Fatalf("verify upstream: %v", err)
		}
		var revision int64
		_ = pool.QueryRow(ctx, "SELECT revision FROM build_intents WHERE pull_request_id=$1", pr).Scan(&revision)
		keys++
		if _, err := broker.RequestBuild(ctx, environments.IntentCommand{UserID: user, PullRequestID: pr, ExpectedRevision: revision,
			IdempotencyKey: fmt.Sprintf("backend-e2e-request-%04d", keys), Action: action}); err != nil {
			t.Fatalf("request build: %v", err)
		}
	}
	published := func(pr int64, sha string) string {
		t.Helper()
		return e2eWait(t, ctx, "the backend preview of "+sha[:7]+" to be published", 10*time.Minute, func() (string, error) {
			var status, label, deployed, failure string
			if err := pool.QueryRow(ctx, "SELECT status, subdomain, coalesce(head_sha, ''), coalesce(last_error_code, '') FROM environments WHERE pull_request_id=$1", pr).
				Scan(&status, &label, &deployed, &failure); err != nil {
				return "", err
			}
			if status == "failed" && deployed != sha {
				return "", errStop(failure + "\n" + e2eLog(t, pool, pr))
			}
			if status != "running" || deployed != sha {
				return "", fmt.Errorf("environment %s at %s", status, deployed)
			}
			return label + "." + e2eDomain, nil
		})
	}
	state := func(host string) backendState {
		t.Helper()
		var s backendState
		body := e2eWait(t, ctx, "the backend API at "+host, time.Minute, func() (string, error) { return router.get(host) })
		if err := json.Unmarshal([]byte(body), &s); err != nil {
			t.Fatalf("API answered %q: %v", body, err)
		}
		return s
	}

	// 1. It builds, runs setup, becomes ready and serves over HTTPS.
	pr := open(e2ePRNumber, backendCommit1)
	request(pr, environments.BuildLatest)
	host := published(pr, backendCommit1)
	first := state(host)
	if first.Marker != "b1" || first.Identity == "" || first.Items != 2 {
		t.Fatalf("first preview: %+v", first)
	}
	if logs := e2eLog(t, pool, pr); !strings.Contains(logs, "PostgreSQL 17 is ready") || !strings.Contains(logs, "migrated and seeded 2 items") || !strings.Contains(logs, "Starting the process worker") {
		t.Fatalf("build log:\n%s", logs)
	}
	app, companions := candidate(t, ctx, docker, pool, instance, pr)
	if len(companions) != 3 || companions["postgres"] == "" || companions["redis"] == "" || companions["worker"] == "" {
		t.Fatalf("companions %v", companions)
	}
	// The queue worker moves a job from Redis into PostgreSQL.
	execIn(t, ctx, docker, app, "wget", "-Y", "off", "-q", "-O-", "http://127.0.0.1:8080/jobs?id=job-1")
	execIn(t, ctx, docker, app, "wget", "-Y", "off", "-q", "-O-", "http://127.0.0.1:8080/items")
	e2eWait(t, ctx, "the worker to process the job", time.Minute, func() (string, error) {
		if s := state(host); !slices.Contains(s.Processed, "job-1") || s.Items != 3 {
			return "", fmt.Errorf("state %+v", s)
		}
		return "", nil
	})
	e2eWait(t, ctx, "the PR comment to name the backend and docs", 2*time.Minute, func() (string, error) {
		comment := github.livingComment()
		if !strings.Contains(comment, "Runs with PostgreSQL 17 and Redis 7 · setup ran · process worker") || !strings.Contains(comment, "https://"+host+"/docs") {
			return "", errors.New("not reported yet")
		}
		return "", nil
	})
	t.Logf("backend preview at %s: database %s, %d items, job processed by the worker", host, first.Identity, first.Items)

	// 2. A fork's preview of the same app: services, but no outbound connections.
	github.pushFork(forkPRNumber, forkCommit, backendFiles("fork"))
	forkPR := open(forkPRNumber, forkCommit)
	request(forkPR, environments.BuildLatest)
	forkHost := published(forkPR, forkCommit)
	if s := state(forkHost); s.Marker != "fork" || s.Identity == first.Identity {
		t.Fatalf("fork preview %+v", s)
	}
	forkApp, _ := candidate(t, ctx, docker, pool, instance, forkPR)
	var withheld bool
	e2eScan(t, pool, "SELECT outbound_withheld FROM preview_components c JOIN environments e ON e.pull_request_id = c.pull_request_id AND c.generation = e.applied_generation AND c.revision = e.applied_revision WHERE c.pull_request_id = $1", &withheld, forkPR)
	if !withheld {
		t.Fatal("the fork's approvals were not recorded as withheld")
	}

	// 3. Services reach nothing beyond their candidate; other previews cannot
	// reach them.
	pgIP, redisIP := serviceAddress(t, ctx, docker, companions["postgres"]), serviceAddress(t, ctx, docker, companions["redis"])
	egressAddr := egressAddress(t, ctx, docker, instance, pr)
	forkEgress := egressAddress(t, ctx, docker, instance, forkPR)
	forkIP := previewAddress(t, ctx, docker, instance, forkPR, forkApp)
	for _, target := range []string{"1.1.1.1 443", "93.184.215.14 80", egressAddr.String() + " 3128", "host.docker.internal 5432", forkIP.String() + " 8080"} {
		if out, reached := connect(t, ctx, docker, companions["postgres"], target); reached {
			t.Fatalf("PostgreSQL reached %s: %q", target, out)
		}
	}
	for _, target := range []string{pgIP.String() + " 5432", redisIP.String() + " 6379"} {
		if out, reached := connect(t, ctx, docker, forkApp, target); reached {
			t.Fatalf("another preview reached a service at %s: %q", target, out)
		}
	}
	t.Log("isolation: services reach neither the internet, the egress proxy, the host nor other previews, and no other preview reaches them")

	// 4. Outbound: the approved destination answers through the egress
	// proxy; unapproved, loopback and metadata destinations do not; the fork
	// gets nothing.
	if out, _ := connect(t, ctx, docker, app, "github.com 22"); !strings.HasPrefix(out, "SSH-2.0") {
		t.Fatalf("the approved destination answered %q", out)
	}
	for _, target := range []string{"localtest.me 5432", "169.254.169.254.nip.io 6380", egressAddr.String() + " 9092"} {
		if out, _ := connect(t, ctx, docker, app, target); out != "" {
			t.Fatalf("%s answered %q", target, out)
		}
	}
	for _, target := range []string{"github.com 22", forkEgress.String() + " 22"} {
		if out, _ := connect(t, ctx, docker, forkApp, target); out != "" {
			t.Fatalf("the fork reached %s: %q", target, out)
		}
	}
	gateway, err := egress.NewGateway(docker, execution.EgressImage)
	if err != nil {
		t.Fatal(err)
	}
	stats := e2eWait(t, ctx, "the egress counters", time.Minute, func() (string, error) {
		s, err := gateway.Stats(ctx)
		if err != nil || s.Connections[egress.OutcomeForwarded] < 1 || s.Connections[egress.OutcomeDenied] < 2 || s.Connections[egress.OutcomeUnapproved] < 2 {
			return "", fmt.Errorf("counters %+v %v", s, err)
		}
		return fmt.Sprint(s.Connections), nil
	})
	t.Logf("outbound: approved destination reached; refused the rest (%s)", stats)

	// 5. A rebuild gets a fresh database.
	github.push(backendCommit2, backendFiles("b2"))
	if _, err := events.ProcessPR(ctx, webhooks.PREvent{RepoID: repoID, Number: e2ePRNumber, HeadSHA: backendCommit2, Action: "synchronize", MergeableState: "clean"}); err != nil {
		t.Fatal(err)
	}
	request(pr, environments.BuildLatest)
	published(pr, backendCommit2)
	second := state(host)
	if second.Marker != "b2" || second.Identity == first.Identity || second.Items != 2 || len(second.Processed) != 0 {
		t.Fatalf("the rebuilt preview kept data: %+v (before %+v)", second, first)
	}
	// The retired candidate goes after the swap converged, its services with it.
	e2eWait(t, ctx, "the replaced candidate's services to be removed", 3*time.Minute, func() (string, error) {
		for _, id := range companions {
			if _, err := docker.ContainerInspect(ctx, id, client.ContainerInspectOptions{}); err == nil {
				return "", errors.New("still there")
			}
		}
		return "", nil
	})
	t.Logf("rebuild: fresh database %s replaced %s", second.Identity, first.Identity)

	// 6. A failed migration fails the build and keeps the old preview serving.
	github.push(backendCommit3, backendFiles("b3", "MIGRATION_FAIL=1"))
	if _, err := events.ProcessPR(ctx, webhooks.PREvent{RepoID: repoID, Number: e2ePRNumber, HeadSHA: backendCommit3, Action: "synchronize", MergeableState: "clean"}); err != nil {
		t.Fatal(err)
	}
	request(pr, environments.BuildLatest)
	e2eWait(t, ctx, "the failing migration to fail the build", 5*time.Minute, func() (string, error) {
		var state, code string
		if err := pool.QueryRow(ctx, "SELECT b.state, coalesce(e.last_error_code, '') FROM build_intents b JOIN environments e ON e.pull_request_id = b.pull_request_id WHERE b.pull_request_id=$1", pr).Scan(&state, &code); err != nil {
			return "", err
		}
		if state != "failed" || code != "setup_failed" {
			return "", fmt.Errorf("request %s, failure %q", state, code)
		}
		return "", nil
	})
	if s := state(host); s.Identity != second.Identity || s.Marker != "b2" {
		t.Fatalf("a failed migration replaced the preview: %+v", s)
	}
	if logs := e2eLog(t, pool, pr); !strings.Contains(logs, "migration 0002_broken.sql failed") || !strings.Contains(logs, "The setup command exited with code 1") {
		t.Fatalf("build log:\n%s", logs)
	}
	t.Log("failed migration: setup_failed, the earlier preview and its data keep serving")

	// 7. Crash leftovers: a candidate whose worker died after starting its
	// database (its app never existed) is found and removed by housekeeping.
	var image string
	e2eScan(t, pool, "SELECT image_tag FROM environments WHERE pull_request_id=$1", &image, pr)
	leftover := crashLeftover(t, ctx, docker, instance, pr, image)
	time.Sleep(cfg.HousekeepingGrace + 5*time.Second)
	// The inventory walks PRs with a cursor (a pass at its end only resets
	// it), so passes are requested until one finds the leftover.
	lastPass := time.Time{}
	e2eWait(t, ctx, "housekeeping to remove the crashed candidate", 3*time.Minute, func() (string, error) {
		if _, err := docker.ContainerInspect(ctx, leftover, client.ContainerInspectOptions{}); err != nil {
			return "", nil
		}
		if time.Since(lastPass) > 15*time.Second {
			if _, err := producer.Insert(ctx, housekeeping.ResourcesArgs{}, &river.InsertOpts{Queue: housekeeping.Queue}); err != nil {
				return "", err
			}
			lastPass = time.Now()
		}
		return "", errors.New("still there")
	})
	if s := state(host); s.Identity != second.Identity {
		t.Fatal("removing the leftover touched the live preview")
	}

	// 8. Teardown leaves no container, image, network or volume.
	for _, number := range []int32{e2ePRNumber, forkPRNumber} {
		sha := backendCommit3
		if number == forkPRNumber {
			sha = forkCommit
		}
		if _, err := events.ProcessPR(ctx, webhooks.PREvent{RepoID: repoID, Number: number, HeadSHA: sha, Action: "closed", MergeableState: "clean"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []int64{pr, forkPR} {
		e2eWait(t, ctx, "the teardown to remove everything", 3*time.Minute, func() (string, error) {
			var status string
			if err := pool.QueryRow(ctx, "SELECT status FROM environments WHERE pull_request_id=$1", id).Scan(&status); err != nil {
				return "", err
			}
			if status != "torn_down" {
				return "", fmt.Errorf("environment %s", status)
			}
			return "", e2eLeftovers(ctx, docker, instance, id)
		})
	}
	if added := slices.DeleteFunc(slices.Collect(maps.Keys(e2eVolumes(t, ctx, docker))), func(v string) bool { return volumesBefore[v] }); len(added) > 0 {
		t.Fatalf("volumes left behind: %v", added)
	}
	t.Log("teardown: no container, image, network or volume of either preview remains")
}

// buildBackendFixture compiles the fixture for the Docker host's platform.
func buildBackendFixture(t *testing.T, ctx context.Context, docker *client.Client) string {
	t.Helper()
	info, err := docker.Info(ctx, client.InfoOptions{})
	if err != nil {
		t.Fatal(err)
	}
	arch := map[string]string{"x86_64": "amd64", "aarch64": "arm64"}[info.Info.Architecture]
	if arch == "" {
		t.Fatalf("unsupported Docker architecture %s", info.Info.Architecture)
	}
	out := filepath.Join(t.TempDir(), "backend")
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", out, "./testdata/backend")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the backend fixture: %v\n%s", err, output)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// candidate returns the PR's deployed app container and its companions by
// component.
func candidate(t *testing.T, ctx context.Context, docker *client.Client, pool *pgxpool.Pool, instance string, pr int64) (string, map[string]string) {
	t.Helper()
	var app string
	e2eScan(t, pool, "SELECT container_id FROM environments WHERE pull_request_id=$1", &app, pr)
	inspected, err := docker.ContainerInspect(ctx, app, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := docker.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: client.Filters{}.Add("label",
		dockerx.LabelInstance+"="+instance, dockerx.LabelCandidate+"="+inspected.Container.Config.Labels[dockerx.LabelCandidate])})
	if err != nil {
		t.Fatal(err)
	}
	companions := map[string]string{}
	for _, c := range listed.Items {
		if c.Labels[dockerx.LabelKind] != dockerx.KindPreview {
			companions[c.Labels[dockerx.LabelComponent]] = c.ID
		}
	}
	return app, companions
}

func execIn(t *testing.T, ctx context.Context, docker *client.Client, id string, cmd ...string) string {
	t.Helper()
	result, err := dockerx.Run(ctx, docker, id, cmd...)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("%v: exit %d: %v %s", cmd, result.ExitCode, err, result.Stderr)
	}
	return result.Stdout
}

// connect opens a TCP connection from inside a container with busybox nc
// and returns what the other side sent first; reached says whether the
// connection was accepted.
func connect(t *testing.T, ctx context.Context, docker *client.Client, id, target string) (string, bool) {
	t.Helper()
	// stdin stays open long enough for a server that speaks first.
	result, err := dockerx.Run(ctx, docker, id, "sh", "-c", "(sleep 3; echo) | timeout 10 nc -w 5 "+target+" | head -c 64")
	if err != nil {
		t.Fatalf("connect to %s: %v", target, err)
	}
	probe, err := dockerx.Run(ctx, docker, id, "sh", "-c", "timeout 8 nc -z -w 5 "+target)
	if err != nil {
		t.Fatalf("probe %s: %v", target, err)
	}
	return strings.TrimSpace(result.Stdout), probe.ExitCode == 0
}

func serviceAddress(t *testing.T, ctx context.Context, docker *client.Client, id string) netip.Addr {
	t.Helper()
	inspected, err := docker.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range inspected.Container.NetworkSettings.Networks {
		if endpoint.IPAddress.IsValid() {
			return endpoint.IPAddress
		}
	}
	t.Fatal("the service has no address")
	return netip.Addr{}
}

func previewNetworkName(instance string, pr int64) string {
	return "previa-" + instance + "-pr-" + strconv.FormatInt(pr, 10)
}

func previewAddress(t *testing.T, ctx context.Context, docker *client.Client, instance string, pr int64, id string) netip.Addr {
	t.Helper()
	inspected, err := docker.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return inspected.Container.NetworkSettings.Networks[previewNetworkName(instance, pr)].IPAddress
}

func egressAddress(t *testing.T, ctx context.Context, docker *client.Client, instance string, pr int64) netip.Addr {
	t.Helper()
	inspected, err := docker.ContainerInspect(ctx, egress.ContainerName, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := inspected.Container.NetworkSettings.Networks[previewNetworkName(instance, pr)]
	if endpoint == nil || !endpoint.IPAddress.IsValid() {
		t.Fatal("the egress proxy has no address on the preview network")
	}
	return endpoint.IPAddress
}

// crashLeftover stands in for a candidate whose worker died after creating
// its database: a labelled service container of an earlier request with no
// app container.
func crashLeftover(t *testing.T, ctx context.Context, docker *client.Client, instance string, pr int64, image string) string {
	t.Helper()
	labels := map[string]string{
		dockerx.LabelManaged: "true", dockerx.LabelInstance: instance, dockerx.LabelKind: dockerx.KindService,
		dockerx.LabelPR: strconv.FormatInt(pr, 10), dockerx.LabelGeneration: "1", dockerx.LabelRevision: "1",
		dockerx.LabelCandidate: "previa-" + instance + "-crashed", dockerx.LabelComponent: "postgres",
		dockerx.LabelCPUMillis: "100", dockerx.LabelMemory: strconv.Itoa(32 << 20),
	}
	created, err := docker.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:       "previa-" + instance + "-crashed-postgres",
		Config:     &container.Config{Image: image, Entrypoint: []string{"sleep", "600"}, Labels: labels},
		HostConfig: &container.HostConfig{NetworkMode: "none", Resources: container.Resources{Memory: 32 << 20}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := docker.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}
	return created.ID
}

// e2eVolumes lists Docker's volumes other than build caches, which builds
// keep by design.
func e2eVolumes(t *testing.T, ctx context.Context, docker *client.Client) map[string]bool {
	t.Helper()
	listed, err := docker.VolumeList(ctx, client.VolumeListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	volumes := map[string]bool{}
	for _, v := range listed.Items {
		if v.Labels[dockerx.LabelKind] != dockerx.KindBuildCache {
			volumes[v.Name] = true
		}
	}
	return volumes
}
