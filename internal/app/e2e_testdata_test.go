package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/activity"
	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/ajaykathwatee/previa/internal/platform/workers"
	"github.com/ajaykathwatee/previa/internal/pullrequests"
	"github.com/ajaykathwatee/previa/internal/sampledata"
	"github.com/ajaykathwatee/previa/internal/source"
	"github.com/ajaykathwatee/previa/internal/webhooks"
	"github.com/google/go-github/v91/github"
	"github.com/moby/moby/client"
)

const (
	mainCommit1    = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	mainCommit2    = "a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2"
	dataCommit1    = "b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1"
	dataCommit2    = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	dataCommit3    = "b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3"
	dataCommit4    = "b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4"
	otherCommit    = "e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1"
	dataForkCommit = "f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1"
)

// TestRealTestDataJourneys runs persistent test data
// (docs/32-persistent-test-data.md) through the worker's own pipeline on real
// Docker, BuildKit, PostgreSQL and Traefik. Turning Test data on builds a
// baseline from the default branch with its migrations and seed, stored
// encrypted, and leaves no baseline container behind. A pull request's first
// build restores it and runs only its own migration on the existing rows,
// without seeding; an order a tester adds survives a new commit and a
// Rebuild, and the comment says the data was kept, while a parallel pull
// request never sees it. Reset test data starts again from the baseline, or
// from empty databases and the seed. A branch whose migration history rejects
// copied data falls back to empty and seed with the reasons recorded; a
// genuinely broken migration still fails as setup_failed while the earlier
// deployment serves. A fork gets the baseline. A pull request merged into the
// baseline branch refreshes the baseline. Teardown leaves nothing behind.
func TestRealTestDataJourneys(t *testing.T) {
	if os.Getenv("TEST_E2E") != "1" {
		t.Skip("set TEST_E2E=1 (make test-e2e) to run the test data journeys on the local Docker Engine")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Minute)
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
	execution.PreviewBaselineDir = filepath.Join(t.TempDir(), "baselines")
	execution.BuildMinFreeDisk = 1 << 30
	gateway := newE2EGateway(t, docker, execution.EgressImage)
	router := startE2ERouter(t, docker)
	gh := newFakeGitHub(t)
	binary := buildFixture(t, ctx, docker, "reference")
	files := func(env ...string) map[string]string {
		yaml := "port: 8080\nresource_tier: small\nhealth_check_path: /health\nservices:\n  postgres: 17\n" +
			"setup: [reference, shop, migrate]\nseed: [reference, shop, seed]\n"
		if len(env) > 0 {
			yaml += "env:\n"
			for _, e := range env {
				yaml += "  - " + e + "\n"
			}
		}
		return map[string]string{".previewrc.yaml": yaml, "reference": binary,
			"Dockerfile": "FROM alpine:3.22\nCOPY --chmod=0755 reference /usr/local/bin/reference\nCMD [\"reference\", \"shop\", \"serve\"]\n"}
	}
	gh.pushToBranch("main", mainCommit1, files())

	key := make([]byte, 32)
	_, _ = rand.Read(key)
	cfg := config.WorkerConfig{
		ExecutionConfig: execution,
		RoutingConfig: config.RoutingConfig{PreviewBaseDomain: e2eDomain, AppBaseURL: "http://localhost:8080", RouterContainer: router.name,
			RouterDynamicDir: router.dynamic, RouterEntryPoint: "websecure", RouterAPIAddress: "127.0.0.1:8080"},
		HousekeepingConfig:  config.HousekeepingConfig{EnvTTLHours: 48, HousekeepingGrace: time.Minute, BuildCacheBudget: 40 << 30},
		MaxConcurrentBuilds: 1, BuildTimeout: 10 * time.Minute,
	}
	cfg.SecretsEncryptionKeys, cfg.SecretsActiveKeyVersion = "1:"+base64.StdEncoding.EncodeToString(key), "1"
	cfg.PreviewMemoryBudget, cfg.PreviewCPUBudget = 24<<30, 32
	producer, err := queue.NewProducer(pool, logger)
	if err != nil {
		t.Fatal(err)
	}
	p, err := buildPipeline(ctx, pipelineInput{cfg: cfg, pool: pool, producer: producer, docker: docker, gateway: gateway, github: gh, logger: logger,
		archiveOptions: []source.ArchiveOption{source.WithDownloadClient(gh.server.Client()), source.WithArchiveHosts(gh.Host())}})
	if err != nil {
		t.Fatal(err)
	}
	if !p.capabilities.Baselines || len(p.capabilities.Services) == 0 {
		t.Fatalf("the worker stores no baselines or offers no services: %+v", p.capabilities)
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

	// The server side, wired as the server wires it.
	operations, err := environments.NewOperationBroker(producer)
	if err != nil {
		t.Fatal(err)
	}
	serverRepo := environments.NewRepository(pool, operations).WithReports()
	broker := environments.NewRequestBroker(pool, producer).WithReports(serverRepo).WithResets(resetActivity{recorder: activity.NewRecorder()})
	sync := pullrequests.NewSyncService(pool, producer, pullrequests.NewGitHubSource(gh), broker)
	events := webhooks.NewService(pool, broker)
	testDataStore, err := sampledata.NewStore(pool, producer, environments.BuildQueue)
	if err != nil {
		t.Fatal(err)
	}
	testData, err := sampledata.NewService(testDataStore, testDataActivity{recorder: activity.NewRecorder()}, workerTestData{capabilities: workers.NewReader(pool)})
	if err != nil {
		t.Fatal(err)
	}
	deliveries := webhooks.NewDeliveryService(pool, broker, nil, sync).WithMerges(testDataStore)
	var user, repoID int64
	e2eScan(t, pool, "INSERT INTO users(github_id, username) VALUES (1, 'owner') RETURNING id", &user)
	e2eScan(t, pool, `WITH i AS (INSERT INTO installations(github_installation_id, owner_user_id, status) VALUES (900, $1, 'active') RETURNING id)
		INSERT INTO repos(installation_id, full_name, github_repo_id) SELECT id, 'octo/app', $2 FROM i RETURNING id`, &repoID, user, e2eRepoID)

	pullRequest := func(number int32, sha, action string) int64 {
		t.Helper()
		changed, err := events.ProcessPR(ctx, webhooks.PREvent{RepoID: repoID, Number: number, HeadSHA: sha, Action: action, MergeableState: "clean"})
		if err != nil {
			t.Fatal(err)
		}
		return changed.PullRequestID
	}
	requests := 0
	request := func(pr int64, action environments.IntentAction, reset bool, start string) {
		t.Helper()
		if _, err := sync.VerifyUpstream(ctx, pr); err != nil {
			t.Fatalf("verify upstream: %v", err)
		}
		var revision int64
		_ = pool.QueryRow(ctx, "SELECT revision FROM build_intents WHERE pull_request_id=$1", pr).Scan(&revision)
		requests++
		if _, err := broker.RequestBuild(ctx, environments.IntentCommand{UserID: user, PullRequestID: pr, ExpectedRevision: revision,
			IdempotencyKey: fmt.Sprintf("test-data-e2e-request-%04d", requests), Action: action, Reset: reset, StartSource: start}); err != nil {
			t.Fatalf("request %s: %v", action, err)
		}
	}
	build := func(pr int64) { t.Helper(); request(pr, environments.BuildLatest, false, "") }
	serving := func(pr int64, sha string, deployments int) string {
		t.Helper()
		return e2eWait(t, ctx, fmt.Sprintf("deployment %d of %s", deployments, sha[:7]), 10*time.Minute, func() (string, error) {
			var status, label, deployed, failure string
			if err := pool.QueryRow(ctx, "SELECT status, subdomain, coalesce(head_sha, ''), coalesce(last_error_code, '') FROM environments WHERE pull_request_id=$1", pr).
				Scan(&status, &label, &deployed, &failure); err != nil {
				return "", err
			}
			if status == "failed" && deployed != sha {
				return "", errStop(failure + "\n" + e2eLog(t, pool, pr))
			}
			published := e2eCount(t, pool, "SELECT count(*) FROM preview_publications WHERE pull_request_id=$1", pr)
			if status != "running" || deployed != sha || published != deployments {
				return "", fmt.Errorf("environment %s at %s after %d deployments", status, deployed, published)
			}
			return label + "." + e2eDomain, nil
		})
	}
	send := func(host, method, path string, form url.Values) (*http.Response, string, error) {
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req, err := http.NewRequestWithContext(ctx, method, "https://"+host+path, body)
		if err != nil {
			return nil, "", err
		}
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		response, err := router.client(host).Do(req)
		if err != nil {
			return nil, "", err
		}
		defer func() { _ = response.Body.Close() }()
		data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<16))
		return response, string(data), nil
	}
	answer := func(host, path string) string {
		t.Helper()
		return e2eWait(t, ctx, "https://"+host+path, time.Minute, func() (string, error) {
			response, body, err := send(host, http.MethodGet, path, nil)
			if err != nil {
				return "", err
			}
			if response.StatusCode != http.StatusOK {
				return "", fmt.Errorf("status %d: %s", response.StatusCode, body)
			}
			return body, nil
		})
	}
	orders := func(host string, ok func([]shopOrder) bool) {
		t.Helper()
		e2eWait(t, ctx, "the shop's orders", time.Minute, func() (string, error) {
			var listed []shopOrder
			if err := json.Unmarshal([]byte(answer(host, "/orders")), &listed); err != nil {
				return "", err
			}
			if !ok(listed) {
				return "", fmt.Errorf("orders %+v", listed)
			}
			return "", nil
		})
	}
	seededOnly := func(o []shopOrder) bool {
		return len(o) == 2 && o[0] == shopOrder{"Preview customer A", "seed"} && o[1] == shopOrder{"Preview customer B", "seed"}
	}
	withTester := func(o []shopOrder) bool { return len(o) == 3 && o[2] == shopOrder{"Tester order", "tester"} }
	// started is where the serving deployment's database started.
	started := func(pr int64) environments.StartingData {
		t.Helper()
		var src string
		var detail []byte
		var seeded bool
		if err := pool.QueryRow(ctx, `SELECT coalesce(c.data_source, ''), c.data_detail, c.seed_ms IS NOT NULL FROM preview_components c
			JOIN LATERAL (SELECT x.generation, x.revision FROM preview_publications x WHERE x.pull_request_id = c.pull_request_id ORDER BY x.sequence DESC LIMIT 1) pub
			ON c.generation = pub.generation AND c.revision = pub.revision WHERE c.pull_request_id = $1`, pr).Scan(&src, &detail, &seeded); err != nil {
			t.Fatalf("starting data of the serving deployment: %v", err)
		}
		return environments.ParseStartingData(src, detail, seeded)
	}
	// refreshed waits until refresh number sequence succeeded with a current
	// baseline of commit, and returns its log.
	refreshed := func(sequence int64, commit string) string {
		t.Helper()
		e2eWait(t, ctx, fmt.Sprintf("baseline refresh %d", sequence), 10*time.Minute, func() (string, error) {
			var state, code, detail string
			if err := pool.QueryRow(ctx, "SELECT state, coalesce(failure_code, ''), coalesce(failure_detail, '') FROM baseline_refreshes WHERE repo_id=$1 AND sequence=$2", repoID, sequence).
				Scan(&state, &code, &detail); err != nil {
				return "", err
			}
			if state == "failed" || state == "cancelled" {
				return "", errStop(state + " " + code + ": " + detail)
			}
			if state != "succeeded" || e2eCount(t, pool, "SELECT count(*) FROM repository_baselines WHERE repo_id=$1 AND status='current' AND commit_sha=$2", repoID, commit) != 1 {
				return "", fmt.Errorf("refresh %s", state)
			}
			return "", nil
		})
		var log string
		e2eScan(t, pool, `SELECT coalesce(string_agg(l.line, E'\n' ORDER BY l.sequence), '') FROM baseline_logs l JOIN baseline_refreshes b ON b.id = l.refresh_id
			WHERE b.repo_id = $1 AND b.sequence = $2`, &log, repoID, sequence)
		return log
	}
	noBaselineObjects := func() {
		t.Helper()
		e2eWait(t, ctx, "the baseline's containers and network to be removed", 2*time.Minute, func() (string, error) {
			filter := client.Filters{}.Add("label", dockerx.LabelInstance+"="+instance, dockerx.LabelBaseline)
			containers, err := docker.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filter})
			if err != nil {
				return "", err
			}
			networks, err := docker.NetworkList(ctx, client.NetworkListOptions{Filters: filter})
			if err != nil {
				return "", err
			}
			images, err := docker.ImageList(ctx, client.ImageListOptions{Filters: filter})
			if err != nil {
				return "", err
			}
			if n := len(containers.Items) + len(networks.Items) + len(images.Items); n > 0 {
				return "", fmt.Errorf("%d baseline containers, %d networks, %d images left", len(containers.Items), len(networks.Items), len(images.Items))
			}
			return "", nil
		})
	}

	// 1. Turning Test data on builds a baseline from the default branch: its
	// migrations and seed run once, the dump is stored encrypted, and nothing
	// of the build remains.
	if _, err := testData.Save(ctx, user, repoID, sampledata.Form{Enabled: true, KeepData: true, UseBaseline: true, AutoRefresh: true}); err != nil {
		t.Fatal(err)
	}
	log := refreshed(1, mainCommit1)
	for _, want := range []string{"Building the test data baseline from branch main", "shop: migrated; 0 orders were already there", "shop: seeded 2 orders", "Captured"} {
		if !strings.Contains(log, want) {
			t.Fatalf("baseline log lacks %q:\n%s", want, log)
		}
	}
	stored, err := os.ReadDir(execution.PreviewBaselineDir)
	if err != nil || len(stored) != 1 {
		t.Fatalf("one baseline file: %v %v", stored, err)
	}
	info, _ := stored[0].Info()
	raw, _ := os.ReadFile(filepath.Join(execution.PreviewBaselineDir, stored[0].Name()))
	if info.Mode().Perm() != 0o600 || strings.Contains(string(raw), "Preview customer") {
		t.Fatalf("the baseline file is private and encrypted: %v", info.Mode())
	}
	noBaselineObjects()
	t.Log("baseline: built from main with migrations and seed, stored encrypted, nothing left running")

	// 2. A pull request's first build restores the baseline and runs only
	// its own migration, on the existing rows; the seed does not run.
	gh.push(dataCommit1, files("MIGRATION=2"))
	shopPR := pullRequest(e2ePRNumber, dataCommit1, "opened")
	build(shopPR)
	shopHost := serving(shopPR, dataCommit1, 1)
	orders(shopHost, seededOnly)
	if columns := answer(shopHost, "/columns"); !strings.Contains(columns, "discount_code") {
		t.Fatalf("the pull request's migration did not run: %s", columns)
	}
	if logs := e2eLog(t, pool, shopPR); !strings.Contains(logs, "shop: migrated; 2 orders were already there") || strings.Contains(logs, "shop: seeded") {
		t.Fatalf("build log:\n%s", logs)
	}
	if d := started(shopPR); d.Source != "baseline" || d.BaselineSHA != mainCommit1 || d.Seeded {
		t.Fatalf("started from %+v", d)
	}
	t.Log("first build: restored the baseline, migrated 2 existing orders, no seed")

	// 3. A tester's order survives a new commit and a Rebuild.
	response, body, err := send(shopHost, http.MethodPost, "/orders", url.Values{"customer": {"Tester order"}})
	if err != nil || response.StatusCode != http.StatusCreated {
		t.Fatalf("add an order: %v %v %s", err, response, body)
	}
	orders(shopHost, withTester)
	gh.push(dataCommit2, files("MIGRATION=2"))
	pullRequest(e2ePRNumber, dataCommit2, "synchronize")
	build(shopPR)
	serving(shopPR, dataCommit2, 2)
	orders(shopHost, withTester)
	if d := started(shopPR); d.Source != "kept" || d.KeptSHA != dataCommit1 {
		t.Fatalf("a new commit keeps data: %+v", d)
	}
	e2eWait(t, ctx, "the comment to name the kept data", 3*time.Minute, func() (string, error) {
		if comment := gh.livingComment(); !strings.Contains(comment, "Test data: kept from previous build.") || strings.Contains(comment, "Tester order") {
			return "", fmt.Errorf("comment %q", comment)
		}
		return "", nil
	})
	request(shopPR, environments.Rebuild, false, "")
	serving(shopPR, dataCommit2, 3)
	orders(shopHost, withTester)
	if d := started(shopPR); d.Source != "kept" {
		t.Fatalf("a Rebuild keeps data: %+v", d)
	}
	t.Log("kept: the tester's order survived a new commit and a Rebuild")

	// 4. A parallel pull request starts from the baseline and never sees it.
	gh.pushBranch(notesPRNumber, otherCommit, files())
	otherPR := pullRequest(notesPRNumber, otherCommit, "opened")
	build(otherPR)
	otherHost := serving(otherPR, otherCommit, 1)
	orders(otherHost, seededOnly)
	if d := started(otherPR); d.Source != "baseline" {
		t.Fatalf("a parallel pull request starts from %+v", d)
	}
	t.Log("isolation: a parallel pull request sees only the baseline")

	// 5. Reset test data to the baseline, then to empty databases and seed.
	request(shopPR, environments.Rebuild, true, environments.StartFromBaseline)
	serving(shopPR, dataCommit2, 4)
	orders(shopHost, seededOnly)
	if d := started(shopPR); d.Source != "baseline" {
		t.Fatalf("Reset to the baseline: %+v", d)
	}
	if n := e2eCount(t, pool, "SELECT count(*) FROM activity_events WHERE pull_request_id=$1 AND kind=$2 AND metadata->>'source'='baseline'", shopPR, activity.ResetRequested); n != 1 {
		t.Fatalf("%d reset events with their source", n)
	}
	request(shopPR, environments.Rebuild, true, environments.StartEmpty)
	serving(shopPR, dataCommit2, 5)
	orders(shopHost, seededOnly)
	if d := started(shopPR); d.Source != "empty" || !d.Seeded {
		t.Fatalf("Reset to empty and seed: %+v", d)
	}
	t.Log("reset: to the baseline, then to empty databases and seed")

	// 6. A branch whose migration history rejects copied data falls back to
	// empty and seed, with every reason recorded.
	gh.push(dataCommit3, files("REJECT_EXISTING=1"))
	pullRequest(e2ePRNumber, dataCommit3, "synchronize")
	build(shopPR)
	serving(shopPR, dataCommit3, 6)
	orders(shopHost, seededOnly)
	d := started(shopPR)
	if d.Source != "empty" || !d.Seeded || len(d.Fallbacks) != 2 || d.Fallbacks[0] != (environments.DataFallback{Source: "kept", Reason: "setup_failed"}) ||
		d.Fallbacks[1] != (environments.DataFallback{Source: "baseline", Reason: "setup_failed"}) {
		t.Fatalf("fallback: %+v", d)
	}
	if logs := e2eLog(t, pool, shopPR); !strings.Contains(logs, "Previa tries the next starting data") || !strings.Contains(logs, "shop: seeded 2 orders") {
		t.Fatalf("build log:\n%s", logs)
	}
	t.Logf("fallback: %s; %s", d.Title(), strings.Join(d.Reasons(), " "))

	// 7. A genuinely broken migration still fails the build as setup_failed;
	// the earlier deployment keeps serving.
	gh.push(dataCommit4, files("MIGRATE_FAIL=1"))
	pullRequest(e2ePRNumber, dataCommit4, "synchronize")
	build(shopPR)
	e2eWait(t, ctx, "the broken migration to fail the update", 8*time.Minute, func() (string, error) {
		var state, code string
		if err := pool.QueryRow(ctx, "SELECT b.state, coalesce(e.last_error_code, '') FROM build_intents b JOIN environments e ON e.pull_request_id = b.pull_request_id WHERE b.pull_request_id=$1", shopPR).Scan(&state, &code); err != nil {
			return "", err
		}
		if state != "failed" || code != "setup_failed" {
			return "", fmt.Errorf("request %s, failure %q", state, code)
		}
		return "", nil
	})
	orders(shopHost, seededOnly)
	t.Log("broken migration: setup_failed after every source; deployment 6 keeps serving")

	// 8. A fork gets the baseline, no secrets, and no other pull request's data.
	gh.pushFork(notesForkNumber, dataForkCommit, files())
	forkPR := pullRequest(notesForkNumber, dataForkCommit, "opened")
	build(forkPR)
	forkHost := serving(forkPR, dataForkCommit, 1)
	orders(forkHost, seededOnly)
	if d := started(forkPR); d.Source != "baseline" {
		t.Fatalf("the fork starts from %+v", d)
	}
	t.Log("fork: started from the baseline")

	// 9. A pull request merged into the baseline branch refreshes the
	// baseline, through the webhook delivery that records the merge.
	gh.pushToBranch("main", mainCommit2, files("MIGRATION=2"))
	now := github.Timestamp{Time: time.Now().UTC()}
	merged := &github.PullRequestEvent{
		Action: new("closed"), Installation: &github.Installation{ID: new(int64(900))},
		Repo: &github.Repository{ID: new(int64(e2eRepoID)), DefaultBranch: new("main")},
		PullRequest: &github.PullRequest{Number: new(notesPRNumber), State: new("closed"), Merged: new(true), MergedAt: &now, UpdatedAt: &now,
			Title: new("Another change"), User: &github.User{Login: new("contributor")},
			Head: &github.PullRequestBranch{SHA: new(otherCommit), Ref: new("feature-9"), Repo: &github.Repository{ID: new(int64(e2eRepoID))}},
			Base: &github.PullRequestBranch{Ref: new("main"), Repo: &github.Repository{ID: new(int64(e2eRepoID))}}},
	}
	if err := deliveries.Process(ctx, "test-data-merge-1", "pull_request", "merge-hash-1", merged); err != nil {
		t.Fatalf("deliver the merge: %v", err)
	}
	log = refreshed(2, mainCommit2)
	if !strings.Contains(log, "from branch main at commit "+mainCommit2[:7]) {
		t.Fatalf("refresh log:\n%s", log)
	}
	if n := e2eCount(t, pool, "SELECT count(*) FROM baseline_refreshes WHERE repo_id=$1 AND sequence=2 AND trigger='merged'", repoID); n != 1 {
		t.Fatal("the merge did not trigger the refresh")
	}
	noBaselineObjects()
	t.Log("merge: the baseline was refreshed from the new head of main")

	// 10. Teardown leaves no container, image, network or volume.
	for number, sha := range map[int32]string{e2ePRNumber: dataCommit4, notesForkNumber: dataForkCommit} {
		pullRequest(number, sha, "closed")
	}
	for _, id := range []int64{shopPR, otherPR, forkPR} {
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
	noBaselineObjects()
	if added := slices.DeleteFunc(slices.Collect(maps.Keys(e2eVolumes(t, ctx, docker))), func(v string) bool { return volumesBefore[v] }); len(added) > 0 {
		t.Fatalf("volumes left behind: %v", added)
	}
	t.Log("teardown: no container, image, network or volume remains")
}
