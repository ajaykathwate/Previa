package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/activity"
	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/ajaykathwatee/previa/internal/pullrequests"
	"github.com/ajaykathwatee/previa/internal/reviews"
	"github.com/ajaykathwatee/previa/internal/secrets"
	"github.com/ajaykathwatee/previa/internal/source"
	"github.com/ajaykathwatee/previa/internal/webhooks"
	reviewpages "github.com/ajaykathwatee/previa/web/pages/reviews"
	"github.com/moby/moby/client"
)

const (
	shopCommit1      = "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1"
	shopCommit2      = "c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2"
	notesCommit      = "d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1"
	notesForkCommit  = "f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9"
	notesPRNumber    = 9
	notesForkNumber  = 10
	referenceAccount = "reviewer@preview.test"
)

// shopOrder is one order the reference shop lists at /orders.
type shopOrder struct {
	Customer string `json:"customer"`
	Source   string `json:"source"`
}

// TestRealReferenceJourneys runs the reference journeys of
// docs/31-test-data.md through the worker's own pipeline on real Docker,
// BuildKit and Traefik. The shop, an HTTP API with SQL data: its setup
// migrates and seeds two orders and the review shows how long it took; an
// order a tester adds is gone after Reset test data, a Rebuild of the same
// commit that is a new deployment and is on the timeline; a failing seed
// fails the update with setup_failed while the earlier deployment keeps
// serving. The notes app, a web app with a test login: its seed creates a
// preview-only account from a repository secret, the review's test access
// reveals the same password and it signs in while a wrong one does not, and a
// fork's preview, which gets no secrets, says it has no test login. Teardown
// leaves nothing behind.
func TestRealReferenceJourneys(t *testing.T) {
	if os.Getenv("TEST_E2E") != "1" {
		t.Skip("set TEST_E2E=1 (make test-e2e) to run the reference journeys on the local Docker Engine")
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
	// The fixture's images are tiny; the production free-disk floor (15 GiB)
	// would refuse every build on a CI runner.
	execution.BuildMinFreeDisk = 1 << 30
	gateway := newE2EGateway(t, docker, execution.EgressImage)
	router := startE2ERouter(t, docker)
	github := newFakeGitHub(t)
	binary := buildFixture(t, ctx, docker, "reference")
	files := func(app string, env ...string) map[string]string {
		yaml := "port: 8080\nresource_tier: small\nhealth_check_path: /health\nservices:\n  postgres: 17\nsetup: [reference, " + app + ", setup]\n"
		if app == "shop" {
			yaml += "docs_path: /docs\n"
		}
		if len(env) > 0 {
			yaml += "env:\n"
			for _, e := range env {
				yaml += "  - " + e + "\n"
			}
		}
		return map[string]string{".previewrc.yaml": yaml, "reference": binary,
			"Dockerfile": "FROM alpine:3.22\nCOPY --chmod=0755 reference /usr/local/bin/reference\nCMD [\"reference\", \"" + app + "\", \"serve\"]\n"}
	}
	github.push(shopCommit1, files("shop"))

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
	// Three previews with databases and a swap exceed Docker Desktop's
	// default share; capacity has its own tests.
	cfg.PreviewMemoryBudget, cfg.PreviewCPUBudget = 24<<30, 32
	keys, err := secrets.ParseKeyring(cfg.SecretsEncryptionKeys, cfg.SecretsActiveKeyVersion)
	if err != nil {
		t.Fatal(err)
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

	// The server side, wired as the server wires it: Reset test data is
	// recorded in the transaction that requests its Rebuild, and test access
	// is sealed with the secrets' keyring.
	operations, err := environments.NewOperationBroker(producer)
	if err != nil {
		t.Fatal(err)
	}
	serverRepo := environments.NewRepository(pool, operations).WithReports()
	broker := environments.NewRequestBroker(pool, producer).WithReports(serverRepo).WithResets(resetActivity{recorder: activity.NewRecorder()})
	sync := pullrequests.NewSyncService(pool, producer, pullrequests.NewGitHubSource(github), broker)
	events := webhooks.NewService(pool, broker)
	review, err := reviews.NewService(pool, reviewActivity{recorder: activity.NewRecorder()}, reviews.Options{PreviewDomain: e2eDomain, Sealer: keys})
	if err != nil {
		t.Fatal(err)
	}
	vault, err := secrets.NewService(secrets.NewRepository(pool), keys)
	if err != nil {
		t.Fatal(err)
	}
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
	build := func(pr int64) {
		t.Helper()
		if _, err := sync.VerifyUpstream(ctx, pr); err != nil {
			t.Fatalf("verify upstream: %v", err)
		}
		var revision int64
		_ = pool.QueryRow(ctx, "SELECT revision FROM build_intents WHERE pull_request_id=$1", pr).Scan(&revision)
		requests++
		if _, err := broker.RequestBuild(ctx, environments.IntentCommand{UserID: user, PullRequestID: pr, ExpectedRevision: revision,
			IdempotencyKey: fmt.Sprintf("reference-e2e-request-%04d", requests), Action: environments.BuildLatest}); err != nil {
			t.Fatalf("request build: %v", err)
		}
	}
	// serving waits until the PR's preview serves sha as its deployments-th
	// deployment and returns its host.
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
	send := func(host, method, path string, form url.Values, cookies ...*http.Cookie) (*http.Response, string, error) {
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		request, err := http.NewRequestWithContext(ctx, method, "https://"+host+path, body)
		if err != nil {
			return nil, "", err
		}
		if form != nil {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		for _, c := range cookies {
			request.AddCookie(c)
		}
		response, err := router.client(host).Do(request)
		if err != nil {
			return nil, "", err
		}
		defer func() { _ = response.Body.Close() }()
		data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<16))
		return response, string(data), nil
	}
	// answer waits until path answers 200 and returns the body.
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
	// orders waits until the shop lists orders that satisfy ok.
	orders := func(host string, ok func([]shopOrder) bool) []shopOrder {
		t.Helper()
		var listed []shopOrder
		e2eWait(t, ctx, "the shop's orders", time.Minute, func() (string, error) {
			listed = nil
			if err := json.Unmarshal([]byte(answer(host, "/orders")), &listed); err != nil {
				return "", err
			}
			if !ok(listed) {
				return "", fmt.Errorf("orders %+v", listed)
			}
			return "", nil
		})
		return listed
	}
	seededOnly := func(o []shopOrder) bool {
		return len(o) == 2 && o[0] == shopOrder{"Preview customer A", "seed"} && o[1] == shopOrder{"Preview customer B", "seed"}
	}
	reviewPage := func(number int32) reviewpages.Page {
		t.Helper()
		page, err := review.Page(ctx, user, repoID, number, reviews.Query{})
		if err != nil {
			t.Fatal(err)
		}
		return page
	}

	// 1. The shop: setup migrates and seeds two orders; the review says how
	// long setup took and offers Reset test data.
	shopPR := pullRequest(e2ePRNumber, shopCommit1, "opened")
	build(shopPR)
	shopHost := serving(shopPR, shopCommit1, 1)
	orders(shopHost, seededOnly)
	if logs := e2eLog(t, pool, shopPR); !strings.Contains(logs, "shop: migrated and seeded 2 orders") {
		t.Fatalf("build log:\n%s", logs)
	}
	var setupMs *int32
	e2eScan(t, pool, `SELECT c.setup_ms FROM preview_components c JOIN environments e ON e.pull_request_id = c.pull_request_id
		AND c.generation = e.applied_generation AND c.revision = e.applied_revision WHERE c.pull_request_id = $1`, &setupMs, shopPR)
	if setupMs == nil || *setupMs < 0 {
		t.Fatalf("setup duration %v", setupMs)
	}
	page := reviewPage(e2ePRNumber)
	if !page.Preview.SetupDeclared || page.Preview.SetupTook == "" || !page.Preview.Data || page.Preview.DocsPath != "/docs" {
		t.Fatalf("review preview %+v", page.Preview)
	}
	// Without Test data (docs/32) builds stay as before: no starting source is
	// recorded, and the comment names none.
	if n := e2eCount(t, pool, "SELECT count(*) FROM preview_components WHERE pull_request_id=$1 AND data_source IS NOT NULL", shopPR); n != 0 {
		t.Fatalf("a repository without Test data recorded %d starting sources", n)
	}
	e2eWait(t, ctx, "the Ready comment", 3*time.Minute, func() (string, error) {
		comment := github.livingComment()
		if strings.Contains(comment, "Test data:") {
			return "", errStop("a repository without Test data names a test data source:\n" + comment)
		}
		if !strings.Contains(comment, "Previa preview: Ready") {
			return "", fmt.Errorf("comment %q", comment)
		}
		return "", nil
	})
	if page.Reset == nil || page.Reset.Commit != shopCommit1 {
		t.Fatalf("Reset test data is not offered: %+v", page.Reset)
	}
	t.Logf("shop at %s: 2 seeded orders, setup completed in %s", shopHost, page.Preview.SetupTook)

	// 2. A tester adds an order; Reset test data starts again from the seed:
	// a new deployment of the same commit with a fresh database.
	response, body, err := send(shopHost, http.MethodPost, "/orders", url.Values{"customer": {"Tester order"}})
	if err != nil || response.StatusCode != http.StatusCreated {
		t.Fatalf("add an order: %v %v %s", err, response, body)
	}
	orders(shopHost, func(o []shopOrder) bool { return len(o) == 3 && o[2].Source == "tester" })
	if _, err := sync.VerifyUpstream(ctx, shopPR); err != nil {
		t.Fatal(err)
	}
	if _, err := broker.RequestBuild(ctx, environments.IntentCommand{UserID: user, PullRequestID: shopPR, ExpectedRevision: page.Reset.ExpectedRevision,
		IdempotencyKey: page.Reset.RequestKey, Action: environments.Rebuild, Reset: true}); err != nil {
		t.Fatalf("reset test data: %v", err)
	}
	serving(shopPR, shopCommit1, 2)
	orders(shopHost, seededOnly)
	if n := e2eCount(t, pool, "SELECT count(*) FROM preview_publications WHERE pull_request_id=$1 AND head_sha=$2", shopPR, shopCommit1); n != 2 {
		t.Fatalf("%d deployments of the reset commit", n)
	}
	if n := e2eCount(t, pool, "SELECT count(*) FROM activity_events WHERE pull_request_id=$1 AND kind=$2 AND actor_user_id=$3 AND metadata->>'sha'=$4",
		shopPR, activity.ResetRequested, user, shopCommit1); n != 1 {
		t.Fatalf("%d reset events on the timeline", n)
	}
	if page = reviewPage(e2ePRNumber); page.Preview.Current == nil || page.Preview.Current.Sequence != 2 || page.Reset == nil {
		t.Fatalf("after the reset: deployment %+v, reset %+v", page.Preview.Current, page.Reset)
	}
	t.Log("reset: the tester's order is gone; deployment 2 of the same commit serves the seed again")

	// 3. A failing seed fails the update with setup_failed; the earlier
	// deployment and its data keep serving and the review says why.
	github.push(shopCommit2, files("shop", "SEED_FAIL=1"))
	pullRequest(e2ePRNumber, shopCommit2, "synchronize")
	build(shopPR)
	e2eWait(t, ctx, "the failing seed to fail the update", 5*time.Minute, func() (string, error) {
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
	if logs := e2eLog(t, pool, shopPR); !strings.Contains(logs, "seed orders.sql failed") || !strings.Contains(logs, "The setup command exited with code 1") {
		t.Fatalf("build log:\n%s", logs)
	}
	// Only the serving candidate's containers remain; the failed one's are gone.
	app, companions := candidate(t, ctx, docker, pool, instance, shopPR)
	e2eWait(t, ctx, "the failed candidate's containers to be removed", 3*time.Minute, func() (string, error) {
		listed, err := docker.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: client.Filters{}.Add("label",
			dockerx.LabelInstance+"="+instance, dockerx.LabelPR+"="+strconv.FormatInt(shopPR, 10))})
		if err != nil {
			return "", err
		}
		serving := map[string]bool{app: true}
		for _, id := range companions {
			serving[id] = true
		}
		for _, c := range listed.Items {
			if !serving[c.ID] {
				return "", fmt.Errorf("container %s (%s) of another candidate remains", c.Names, c.Labels[dockerx.LabelCandidate])
			}
		}
		return "", nil
	})
	page = reviewPage(e2ePRNumber)
	if !page.Preview.SetupFailed || page.Preview.Deployed != shopCommit1 || page.Preview.SetupTook == "" || page.Reset != nil {
		t.Fatalf("after a failed seed: preview %+v, reset %+v", page.Preview, page.Reset)
	}
	t.Log("failed seed: setup_failed; deployment 2 keeps serving its seeded data")

	// 4. The notes app: a repository secret gives its seed a preview-only
	// account, and the review's test access holds the same password.
	raw := make([]byte, 9)
	_, _ = rand.Read(raw)
	password := "preview-" + hex.EncodeToString(raw)
	if _, err := vault.Put(ctx, user, repoID, "TEST_LOGIN_PASSWORD", []byte(password)); err != nil {
		t.Fatal(err)
	}
	github.pushBranch(notesPRNumber, notesCommit, files("notes"))
	notesPR := pullRequest(notesPRNumber, notesCommit, "opened")
	build(notesPR)
	notesHost := serving(notesPR, notesCommit, 1)
	if logs := e2eLog(t, pool, notesPR); !strings.Contains(logs, "notes: seeded 3 notes and the preview-only account "+referenceAccount) || strings.Contains(logs, password) {
		t.Fatalf("build log:\n%s", logs)
	}
	actor := reviews.Actor{UserID: user, RepoID: repoID, Number: notesPRNumber}
	if err := review.SaveTestAccess(ctx, actor, reviews.TestAccessInput{Account: referenceAccount, Secret: password}); err != nil {
		t.Fatalf("save test access: %v", err)
	}
	if n := e2eCount(t, pool, "SELECT count(*) FROM review_test_access WHERE repo_id=$1 AND position(convert_to($2, 'UTF8') in secret_ciphertext) = 0", repoID, password); n != 1 {
		t.Fatal("test access is not stored sealed")
	}
	if access := reviewPage(e2ePRNumber).TestAccess; access.Account != referenceAccount || !access.HasSecret || access.Revealed != "" {
		t.Fatalf("test access on the review %+v", access)
	}
	revealed, err := review.RevealTestAccess(ctx, actor)
	if err != nil || revealed != password {
		t.Fatalf("reveal: %v (matches: %t)", err, revealed == password)
	}
	if n := e2eCount(t, pool, "SELECT count(*) FROM review_test_access WHERE repo_id=$1 AND last_revealed_by=$2", repoID, user); n != 1 {
		t.Fatal("the reveal was not recorded")
	}

	// 5. The revealed password signs in to the preview; a wrong one does not.
	if login := answer(notesHost, "/login"); !strings.Contains(login, `name="password"`) {
		t.Fatalf("login page %q", login)
	}
	response, _, err = send(notesHost, http.MethodPost, "/login", url.Values{"email": {referenceAccount}, "password": {password + "x"}})
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a wrong password: %v %v", err, response)
	}
	response, _, err = send(notesHost, http.MethodPost, "/login", url.Values{"email": {referenceAccount}, "password": {revealed}})
	if err != nil || response.StatusCode != http.StatusSeeOther || len(response.Cookies()) == 0 {
		t.Fatalf("sign in with the revealed password: %v %v", err, response)
	}
	response, body, err = send(notesHost, http.MethodGet, "/", nil, response.Cookies()...)
	if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(body, "Signed in as "+referenceAccount) || strings.Count(body, "<li>") != 3 {
		t.Fatalf("signed in: %v %v %q", err, response, body)
	}
	t.Logf("notes at %s: the revealed test access signs in to 3 seeded notes", notesHost)

	// 6. A fork's preview gets no secrets: its seed and its login page say
	// there is no test login.
	github.pushFork(notesForkNumber, notesForkCommit, files("notes"))
	forkPR := pullRequest(notesForkNumber, notesForkCommit, "opened")
	build(forkPR)
	forkHost := serving(forkPR, notesForkCommit, 1)
	if login := answer(forkHost, "/login"); !strings.Contains(login, "Test login unavailable in this preview") {
		t.Fatalf("the fork's login page %q", login)
	}
	if logs := e2eLog(t, pool, forkPR); !strings.Contains(logs, "TEST_LOGIN_PASSWORD is not set, so this preview has no test login") {
		t.Fatalf("fork build log:\n%s", logs)
	}
	t.Log("fork: no secrets, no test login, and the preview says so")

	// 7. Teardown leaves no container, image, network or volume.
	for number, sha := range map[int32]string{e2ePRNumber: shopCommit2, notesPRNumber: notesCommit, notesForkNumber: notesForkCommit} {
		pullRequest(number, sha, "closed")
	}
	for _, id := range []int64{shopPR, notesPR, forkPR} {
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
	t.Log("teardown: no container, image, network or volume of any preview remains")
}
