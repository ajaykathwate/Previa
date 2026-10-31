package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/installations"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/ajaykathwatee/previa/internal/pullrequests"
	"github.com/ajaykathwatee/previa/internal/source"
	"github.com/ajaykathwatee/previa/internal/webhooks"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

const (
	e2eTraefik   = "traefik:v3.7.13@sha256:24841fe2de7304c149343d877d2923b4c8800a38ba015dea9174c23b20e344a0"
	e2eHTTPImage = "busybox:1.37@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"
	e2eDomain    = "preview.test"
	e2eCommit1   = "1111111111111111111111111111111111111111"
	e2eCommit2   = "2222222222222222222222222222222222222222"
	e2eCommit3   = "3333333333333333333333333333333333333333"
	e2eCommit4   = "4444444444444444444444444444444444444444"
	e2eCommit5   = "5555555555555555555555555555555555555555"
	e2eMarker    = "previa-e2e-v1"
)

// e2eApp is a tiny preview: it answers HTTP on 8080 with its marker.
func e2eApp(marker string) map[string]string {
	return map[string]string{
		".previewrc.yaml": "port: 8080\nresource_tier: small\n",
		"Dockerfile": "FROM " + e2eHTTPImage + "\nENV MARKER=" + marker + "\n" +
			"RUN mkdir -p /srv && printf '%s' \"$MARKER\" > /srv/index.html\n" +
			`CMD ["httpd", "-f", "-p", "8080", "-h", "/srv"]` + "\n",
	}
}

// TestRealEndToEnd runs the worker's own pipeline (buildPipeline) against
// real Docker, BuildKit and Traefik, with a fake GitHub over TLS and a
// throwaway database schema: Manual mode waits, a requested build goes from
// exact-commit source through config, image and a healthy candidate to an
// HTTPS route, a PR report and logs; a failed rebuild keeps the old preview;
// an owner's teardown removes route and containers.
func TestRealEndToEnd(t *testing.T) {
	if os.Getenv("TEST_E2E") != "1" {
		t.Skip("set TEST_E2E=1 (make test-e2e) to run the pipeline on the local Docker Engine")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
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
	execution, err := config.LoadExecution()
	if err != nil {
		t.Fatal(err)
	}
	execution.SourceWorkspaceRoot = t.TempDir()
	// The fixture's images are tiny; the production free-disk floor (15 GiB)
	// would refuse every build on a CI runner.
	execution.BuildMinFreeDisk = 1 << 30
	gateway := newE2EGateway(t, docker, execution.EgressImage)
	router := startE2ERouter(t, docker)
	github := newFakeGitHub(t)
	github.push(e2eCommit1, e2eApp(e2eMarker))

	cfg := config.WorkerConfig{
		ExecutionConfig: execution,
		RoutingConfig: config.RoutingConfig{PreviewBaseDomain: e2eDomain, AppBaseURL: "http://localhost:8080", RouterContainer: router.name,
			RouterDynamicDir: router.dynamic, RouterEntryPoint: "websecure", RouterAPIAddress: "127.0.0.1:8080"},
		HousekeepingConfig:  config.HousekeepingConfig{EnvTTLHours: 48, HousekeepingGrace: 15 * time.Minute, BuildCacheBudget: 40 << 30},
		MaxConcurrentBuilds: 1, BuildTimeout: 10 * time.Minute,
	}
	producer, err := queue.NewProducer(pool, logger)
	if err != nil {
		t.Fatal(err)
	}
	p, err := buildPipeline(ctx, pipelineInput{cfg: cfg, pool: pool, producer: producer, docker: docker, gateway: gateway, github: github, logger: logger,
		archiveOptions: []source.ArchiveOption{source.WithDownloadClient(github.server.Client()), source.WithArchiveHosts(github.Host())}})
	if err != nil {
		t.Fatal(err)
	}
	if !p.capabilities.Execution || !p.capabilities.Reporting || !p.capabilities.Housekeeping {
		t.Fatalf("pipeline capabilities %+v", p.capabilities)
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

	// The server side: webhooks, verification and owner requests.
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

	// Manual mode: opening (and pushing to) the PR builds nothing.
	opened, err := events.ProcessPR(ctx, webhooks.PREvent{RepoID: repoID, Number: e2ePRNumber, HeadSHA: e2eCommit1, Action: "opened", MergeableState: "clean"})
	if err != nil {
		t.Fatal(err)
	}
	pr := opened.PullRequestID
	time.Sleep(2 * time.Second)
	if n := e2eCount(t, pool, "SELECT count(*) FROM build_intents WHERE pull_request_id=$1", pr); n != 0 {
		t.Fatal("a Manual PR built without a request")
	}

	// The owner builds: fresh read from GitHub, then the request and its wake-up.
	request := func(action environments.IntentAction, key string) {
		t.Helper()
		if _, err := sync.VerifyUpstream(ctx, pr); err != nil {
			t.Fatalf("verify upstream: %v", err)
		}
		var revision int64
		_ = pool.QueryRow(ctx, "SELECT revision FROM build_intents WHERE pull_request_id=$1", pr).Scan(&revision)
		if _, err := broker.RequestBuild(ctx, environments.IntentCommand{UserID: user, PullRequestID: pr, ExpectedRevision: revision, IdempotencyKey: key, Action: action}); err != nil {
			t.Fatalf("request build: %v", err)
		}
	}
	request(environments.BuildLatest, "e2e-build-request-0001")
	host := e2eWait(t, ctx, "the preview to be published", 10*time.Minute, func() (string, error) {
		var status, label string
		var expires *time.Time
		if err := pool.QueryRow(ctx, "SELECT status, subdomain, expires_at FROM environments WHERE pull_request_id=$1", pr).Scan(&status, &label, &expires); err != nil {
			return "", err
		}
		if status == "failed" {
			return "", errStop(e2eLog(t, pool, pr))
		}
		if status != "running" || expires == nil {
			return "", fmt.Errorf("environment %s", status)
		}
		return label + "." + e2eDomain, nil
	})
	if body := e2eWait(t, ctx, "the route to serve the preview", time.Minute, func() (string, error) { return router.get(host) }); body != e2eMarker {
		t.Fatalf("preview answered %q", body)
	}
	for range 50 {
		if body, err := router.get(host); err != nil || body != e2eMarker {
			t.Fatalf("the published preview did not consistently serve: %q %v", body, err)
		}
	}
	e2eWait(t, ctx, "the PR report to say Ready", 2*time.Minute, func() (string, error) {
		comment := github.livingComment()
		run := github.run(e2eCommit1, 1)
		if !strings.Contains(comment, "Previa preview: Ready") || run == nil || run.Conclusion != "success" || !strings.Contains(comment, "https://"+host) {
			return "", errors.New("not reported yet")
		}
		return "", nil
	})
	if logs := e2eLog(t, pool, pr); !strings.Contains(logs, "The preview is healthy") || !strings.Contains(logs, "Preparing commit "+e2eCommit1[:12]) {
		t.Fatalf("build log:\n%s", logs)
	}
	t.Logf("published %s from %s; reported Ready; log has %d lines", host, e2eCommit1[:7], e2eCount(t, pool, "SELECT count(*) FROM build_logs l JOIN environments e ON e.id=l.environment_id WHERE e.pull_request_id=$1", pr))

	// A new commit whose build fails: the old preview keeps serving.
	github.push(e2eCommit2, map[string]string{".previewrc.yaml": "port: 8080\n", "Dockerfile": "FROM alpine:3.22\nRUN exit 3\n"})
	if _, err := events.ProcessPR(ctx, webhooks.PREvent{RepoID: repoID, Number: e2ePRNumber, HeadSHA: e2eCommit2, Action: "synchronize", MergeableState: "clean"}); err != nil {
		t.Fatal(err)
	}
	request(environments.BuildLatest, "e2e-build-request-0002")
	e2eWait(t, ctx, "the failing rebuild to fail", 5*time.Minute, func() (string, error) {
		var state string
		if err := pool.QueryRow(ctx, "SELECT state FROM build_intents WHERE pull_request_id=$1", pr).Scan(&state); err != nil {
			return "", err
		}
		if state != "failed" {
			return "", fmt.Errorf("request %s", state)
		}
		return "", nil
	})
	if body, err := router.get(host); err != nil || body != e2eMarker {
		t.Fatalf("the earlier preview stopped serving after a failed rebuild: %q %v", body, err)
	}
	e2eWait(t, ctx, "the failure to be reported", 2*time.Minute, func() (string, error) {
		comment := github.livingComment()
		run := github.run(e2eCommit2, 2)
		if !strings.Contains(comment, "Failed") || !strings.Contains(comment, "keeps serving") || run == nil || run.Conclusion != "failure" {
			return "", errors.New("not reported yet")
		}
		return "", nil
	})

	// Failure injection: GitHub's comment and check-run API fails while a
	// fixed commit builds. Publication never waits for GitHub; the report
	// catches up once GitHub answers again.
	marker5 := "previa-e2e-" + e2eCommit5[:4]
	github.push(e2eCommit5, e2eApp(marker5))
	if _, err := events.ProcessPR(ctx, webhooks.PREvent{RepoID: repoID, Number: e2ePRNumber, HeadSHA: e2eCommit5, Action: "synchronize", MergeableState: "clean"}); err != nil {
		t.Fatal(err)
	}
	github.setReportsDown(true)
	request(environments.BuildLatest, "e2e-build-request-0003")
	e2eWait(t, ctx, "the build to publish during a GitHub outage", 10*time.Minute, func() (string, error) {
		body, err := router.get(host)
		if err == nil && body != marker5 {
			err = fmt.Errorf("still serving %q", body)
		}
		return body, err
	})
	if github.rejectedReports() == 0 {
		t.Fatal("the outage rejected no report: the injection did not happen")
	}
	if run := github.run(e2eCommit5, 3); run != nil {
		t.Fatal("a check run was created during the outage")
	}
	github.setReportsDown(false)
	e2eWait(t, ctx, "the report to catch up after the outage", 8*time.Minute, func() (string, error) {
		comment, run := github.livingComment(), github.run(e2eCommit5, 3)
		if !strings.Contains(comment, "Previa preview: Ready") || run == nil || run.Conclusion != "success" || github.commentCount() != 1 {
			return "", errors.New("not reported yet")
		}
		return "", nil
	})
	t.Logf("GitHub outage: %d report calls rejected; the preview published anyway and the report caught up", github.rejectedReports())

	// The owner tears the preview down: route first, then containers.
	if started, err := serverRepo.RequestTeardown(ctx, pr, environments.TeardownRequested); err != nil || !started {
		t.Fatalf("teardown: %v %v", started, err)
	}
	e2eWait(t, ctx, "the teardown to finish", 3*time.Minute, func() (string, error) {
		var status string
		if err := pool.QueryRow(ctx, "SELECT status FROM environments WHERE pull_request_id=$1", pr).Scan(&status); err != nil {
			return "", err
		}
		if status != "torn_down" {
			return "", fmt.Errorf("environment %s", status)
		}
		return "", e2eLeftovers(ctx, docker, instance, pr)
	})
	if body, err := router.get(host); err == nil && body == e2eMarker {
		t.Fatal("the route still serves the removed preview")
	}
	e2eWait(t, ctx, "the removal to be reported", 2*time.Minute, func() (string, error) {
		if !strings.Contains(github.livingComment(), "Preview removed") || github.commentCount() != 1 {
			return "", errors.New("not reported yet")
		}
		return "", nil
	})
	t.Log("torn down: route removed, containers removed, reported")

	// Automatic: a later relevant event brings the preview back at the same
	// address, and a burst of pushes builds only the newest commit.
	policyStore := installations.NewRepository(pool).WithReports(broker)
	policy, err := installations.NewBuildPolicyService(policyStore)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := policy.Set(ctx, user, repoID, installations.AutomaticBuilds); err != nil {
		t.Fatal(err)
	}
	for _, commit := range []string{e2eCommit3, e2eCommit4} {
		github.push(commit, e2eApp("previa-e2e-"+commit[:4]))
		if _, err := events.ProcessPR(ctx, webhooks.PREvent{RepoID: repoID, Number: e2ePRNumber, HeadSHA: commit, Action: "synchronize", MergeableState: "clean"}); err != nil {
			t.Fatal(err)
		}
	}
	var state, sha, source string
	if err := pool.QueryRow(ctx, "SELECT state, head_sha, source FROM build_intents WHERE pull_request_id=$1", pr).Scan(&state, &sha, &source); err != nil {
		t.Fatal(err)
	}
	if state != "pending" || sha != e2eCommit4 || source != "automatic" {
		t.Fatalf("after a burst of pushes the request is %s %s %s", state, sha[:7], source)
	}
	// Skip the five quiet minutes: the deadline and its durable wake-up.
	if _, err := pool.Exec(ctx, "UPDATE build_intents SET available_at = now() WHERE pull_request_id=$1", pr); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE river_job SET scheduled_at = now() WHERE kind = 'environment_wake' AND state = 'scheduled'"); err != nil {
		t.Fatal(err)
	}
	marker := "previa-e2e-" + e2eCommit4[:4]
	if body := e2eWait(t, ctx, "the Automatic build to publish the newest commit", 10*time.Minute, func() (string, error) { return router.get(host) }); body != marker {
		t.Fatalf("the preview came back with %q", body)
	}
	if github.downloads(e2eCommit3) != 0 || github.downloads(e2eCommit4) != 1 {
		t.Fatalf("source downloads: %d of the intermediate commit, %d of the newest", github.downloads(e2eCommit3), github.downloads(e2eCommit4))
	}
	if github.openRuns(e2eCommit3) != 0 {
		t.Fatal("the superseded commit's check run was left open")
	}
	e2eWait(t, ctx, "the Automatic preview to be reported", 2*time.Minute, func() (string, error) {
		if !strings.Contains(github.livingComment(), "Previa preview: Ready") || github.commentCount() != 1 {
			return "", errors.New("not reported yet")
		}
		return "", nil
	})
	t.Log("Automatic: the burst built only the newest commit, back at " + host + "; one living comment throughout")

	proveMissedUninstall(t, ctx, pool, github, broker, docker, router, instance, user, repoID, pr, host, marker)

	// Closing the PR tears the preview down through the close path and leaves
	// no container, image or network of it behind.
	if _, err := events.ProcessPR(ctx, webhooks.PREvent{RepoID: repoID, Number: e2ePRNumber, HeadSHA: e2eCommit4, Action: "closed", MergeableState: "clean"}); err != nil {
		t.Fatal(err)
	}
	e2eWait(t, ctx, "the close to remove everything", 3*time.Minute, func() (string, error) {
		var status string
		if err := pool.QueryRow(ctx, "SELECT status FROM environments WHERE pull_request_id=$1", pr).Scan(&status); err != nil {
			return "", err
		}
		if status != "torn_down" {
			return "", fmt.Errorf("environment %s", status)
		}
		return "", e2eLeftovers(ctx, docker, instance, pr)
	})
	if body, err := router.get(host); err == nil && body == marker {
		t.Fatal("the route still serves the closed PR's preview")
	}
	t.Log("closed: route, containers, image and network removed")
}

// e2eLeftovers reports any container, image or network of this instance's PR.
func e2eLeftovers(ctx context.Context, docker *client.Client, instance string, pr int64) error {
	filter := client.Filters{}.Add("label", dockerx.LabelInstance+"="+instance, dockerx.LabelPR+"="+strconv.FormatInt(pr, 10))
	containers, err := docker.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filter})
	if err != nil {
		return err
	}
	images, err := docker.ImageList(ctx, client.ImageListOptions{Filters: filter})
	if err != nil {
		return err
	}
	networks, err := docker.NetworkList(ctx, client.NetworkListOptions{Filters: filter})
	if err != nil {
		return err
	}
	if n := len(containers.Items) + len(images.Items) + len(networks.Items); n > 0 {
		var names []string
		for _, network := range networks.Items {
			names = append(names, network.Name)
		}
		return fmt.Errorf("%d containers, %d images, %d networks %v left", len(containers.Items), len(images.Items), len(networks.Items), names)
	}
	return nil
}

type errStop string

func (e errStop) Error() string { return "stopped: " + string(e) }

// e2eWait polls check until it succeeds, fails with errStop, or the timeout.
func e2eWait(t *testing.T, ctx context.Context, what string, timeout time.Duration, check func() (string, error)) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) && ctx.Err() == nil {
		value, err := check()
		if err == nil {
			return value
		}
		var stop errStop
		if errors.As(err, &stop) {
			t.Fatalf("waiting for %s: %v", what, err)
		}
		last = err
		time.Sleep(time.Second)
	}
	t.Fatalf("timed out waiting for %s: %v", what, last)
	return ""
}

func e2eLog(t *testing.T, pool *pgxpool.Pool, pr int64) string {
	t.Helper()
	rows, err := pool.Query(t.Context(), "SELECT l.line FROM build_logs l JOIN environments e ON e.id=l.environment_id WHERE e.pull_request_id=$1 ORDER BY l.epoch, l.sequence", pr)
	if err != nil {
		return err.Error()
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if rows.Scan(&line) == nil {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func e2eScan(t *testing.T, pool *pgxpool.Pool, sql string, dest any, args ...any) {
	t.Helper()
	if err := pool.QueryRow(t.Context(), sql, args...).Scan(dest); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func e2eCount(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	e2eScan(t, pool, sql, &n, args...)
	return n
}

// e2eSchema is a throwaway schema with every migration and River's tables.
func e2eSchema(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required")
	}
	admin, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("e2e_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = admin.Exec(c, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	files, _ := filepath.Glob("../../db/migrations/*.sql")
	sort.Strings(files)
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(data), "-- +goose Down")
		if _, err := pool.Exec(t.Context(), up); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
	}
	// Test allowances are explicit fixture inputs, independent of the owner's
	// production business decisions. Every real worker still meters its work.
	if _, err := pool.Exec(t.Context(), `UPDATE free_plan_policy SET included_seconds=600000,concurrent_builds=8,preview_slots=20`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var mismatches int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_usage_periods p WHERE p.consumed_us<>(
			SELECT coalesce(sum(s.consumed_us),0)::bigint FROM workspace_usage_segments s WHERE s.workspace_id=p.workspace_id AND s.period_start=p.starts_at)`).Scan(&mismatches)
		if err != nil || mismatches != 0 {
			t.Errorf("end-to-end usage does not reconcile: %d %v", mismatches, err)
		}
		if strings.HasPrefix(t.Name(), "TestReal") {
			var attempts, baselines int
			var consumed int64
			err = pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE baseline_refresh_id IS NOT NULL),
				coalesce((SELECT sum(consumed_us) FROM workspace_usage_segments),0)::bigint FROM workspace_usage_attempts`).Scan(&attempts, &baselines, &consumed)
			if err != nil || attempts == 0 || consumed <= 0 {
				t.Errorf("real execution was not accounted: attempts=%d consumed_us=%d error=%v", attempts, consumed, err)
			}
			if t.Name() == "TestRealTestDataJourneys" && baselines < 2 {
				t.Errorf("baseline execution was not accounted: %d attempts", baselines)
			}
			t.Logf("usage: %d physical attempts (%d baselines), %.6f executed seconds; periods reconcile with segments", attempts, baselines, float64(consumed)/1000000)
		}
	})
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrator.Migrate(t.Context(), rivermigrate.DirectionUp, nil); err != nil {
		t.Fatal(err)
	}
	return pool
}

type e2eRouter struct {
	name, dynamic, https string
}

// startE2ERouter runs Traefik with the file provider and its default
// certificate (certificate issuance is proven by make test-routing).
func startE2ERouter(t *testing.T, docker *client.Client) *e2eRouter {
	t.Helper()
	ctx := t.Context()
	if _, err := docker.ImageInspect(ctx, e2eTraefik); err != nil {
		pulled, err := docker.ImagePull(ctx, e2eTraefik, client.ImagePullOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := pulled.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(t.TempDir(), "dynamic")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	_, port, _ := net.SplitHostPort(address)
	r := &e2eRouter{name: fmt.Sprintf("previa-e2e-router-%d", time.Now().UnixNano()), dynamic: dir, https: address}
	logLevel := "ERROR"
	if os.Getenv("TEST_E2E_LOG") == "1" {
		logLevel = "DEBUG"
	}
	created, err := docker.ContainerCreate(ctx, client.ContainerCreateOptions{Name: r.name, Config: &container.Config{
		Image: e2eTraefik, ExposedPorts: network.PortSet{network.MustParsePort("443/tcp"): {}},
		Cmd: []string{"--global.checknewversion=false", "--global.sendanonymoususage=false",
			"--log.level=" + logLevel,
			"--providers.file.directory=/etc/traefik/dynamic", "--providers.file.watch=true", "--providers.providersthrottleduration=250ms",
			"--entrypoints.websecure.address=:443", "--entrypoints.websecure.http.tls=true",
			"--entrypoints.traefik.address=127.0.0.1:8080", "--api=true", "--api.insecure=true", "--api.dashboard=false",
			"--ping=true", "--ping.entrypoint=traefik"},
	}, HostConfig: &container.HostConfig{
		PortBindings: network.PortMap{network.MustParsePort("443/tcp"): {{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: port}}},
		Mounts:       []mount.Mount{{Type: mount.TypeBind, Source: dir, Target: "/etc/traefik/dynamic", ReadOnly: true}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			logE2ERouter(t, docker, created.ID)
		}
		_, _ = docker.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	})
	if _, err := docker.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}
	return r
}

func logE2ERouter(t *testing.T, docker *client.Client, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reader, err := docker.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Tail: "80"})
	if err != nil {
		t.Logf("read test router logs: %v", err)
		return
	}
	defer func() { _ = reader.Close() }()
	var output strings.Builder
	if _, err := stdcopy.StdCopy(&output, &output, io.LimitReader(reader, 256<<10)); err != nil {
		t.Logf("decode test router logs: %v", err)
	}
	t.Logf("test router logs:\n%s", output.String())
}

// client reaches host through the router over HTTPS and does not follow
// redirects.
func (r *e2eRouter) client(host string) *http.Client {
	return &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{ServerName: host, InsecureSkipVerify: true}, //nolint:gosec // Traefik's default certificate in a test
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, r.https)
			},
		}}
}

// get fetches / from host through the router over HTTPS.
func (r *e2eRouter) get(host string) (string, error) {
	response, err := r.client(host).Get("https://" + host + "/")
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<16))
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d: %s", response.StatusCode, body[:min(len(body), 512)])
	}
	return string(body), nil
}

// e2eCleanup removes whatever the run left of this instance, detaching the
// shared egress proxy from its networks first, and fails the test on anything
// it could not remove: a leak must never pass silently.
func e2eCleanup(t *testing.T, docker *client.Client, instance string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	filter := client.Filters{}.Add("label", dockerx.LabelInstance+"="+instance)
	fail := func(what string, err error) {
		if err != nil && !cerrdefs.IsNotFound(err) {
			t.Errorf("cleanup: %s: %v", what, err)
		}
	}
	listed, err := docker.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filter})
	fail("list containers", err)
	if err == nil {
		for _, c := range listed.Items {
			_, err := docker.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true})
			fail("remove container", err)
		}
	}
	images, err := docker.ImageList(ctx, client.ImageListOptions{Filters: filter})
	fail("list images", err)
	if err == nil {
		for _, i := range images.Items {
			_, err := docker.ImageRemove(ctx, i.ID, client.ImageRemoveOptions{Force: true, PruneChildren: true})
			fail("remove image", err)
		}
	}
	networks, err := docker.NetworkList(ctx, client.NetworkListOptions{Filters: filter})
	fail("list networks", err)
	if err == nil {
		for _, n := range networks.Items {
			inspected, err := docker.NetworkInspect(ctx, n.ID, client.NetworkInspectOptions{})
			fail("inspect network", err)
			if err == nil {
				for id := range inspected.Network.Containers {
					_, err := docker.NetworkDisconnect(ctx, n.ID, client.NetworkDisconnectOptions{Container: id, Force: true})
					fail("disconnect from network", err)
				}
			}
			_, err = docker.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{})
			fail("remove network", err)
		}
	}
	volumes, err := docker.VolumeList(ctx, client.VolumeListOptions{Filters: filter})
	fail("list volumes", err)
	if err == nil {
		for _, v := range volumes.Items {
			_, err := docker.VolumeRemove(ctx, v.Name, client.VolumeRemoveOptions{Force: true})
			fail("remove volume", err)
		}
	}
}
