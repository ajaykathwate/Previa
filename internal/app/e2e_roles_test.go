package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/ajaykathwatee/previa/internal/platform/workers"
	"github.com/ajaykathwatee/previa/internal/source"
)

// TestRealRoleJourney walks the core flow through the server's own routes,
// step by step and as each person would, while the worker builds on real
// Docker, BuildKit and Traefik: signed out, then signed in; a signed GitHub
// webhook opens the PR (Manual builds nothing); the Reviewer, a Developer
// GitHub no longer lets read the code, a removed member and an outsider try to
// build and are refused; the granted Developer builds; the Reviewer opens
// the preview and records a finding on Deployment 1; a pushed commit is
// rebuilt at the same address as Deployment 2; the Reviewer resolves the
// finding there; the Owner tears the preview down; closing the PR keeps the
// review readable and refuses builds. Sign-in and the App installation are
// seeded: they redirect to github.com and are proven by the live pass.
func TestRealRoleJourney(t *testing.T) {
	if os.Getenv("TEST_E2E") != "1" {
		t.Skip("set TEST_E2E=1 (make test-e2e) to run the role journey on the local Docker Engine")
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
	execution.BuildMinFreeDisk = 1 << 30
	gateway := newE2EGateway(t, docker, execution.EgressImage)
	router := startE2ERouter(t, docker)
	github := newFakeGitHub(t)
	github.push(e2eCommit1, e2eApp("journey-v1"))
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
	// As RunWorker does: the server builds only while a worker advertises
	// execution, and the crash, visit and health loops run beside River.
	heartbeat, err := workers.Start(ctx, pool, p.capabilities, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = heartbeat.Stop(context.Background()) })
	loops, stopLoops := context.WithCancel(ctx)
	t.Cleanup(stopLoops)
	for _, loop := range p.background {
		go loop(loops)
	}
	instance, err := database.Instance(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e2eCleanup(t, docker, instance) })

	f := newTeamFixture(t, pool, github, e2eDomain)
	repoURL := "/repositories/" + f.public["repoA"]
	expect := func(persona, method, path string, form url.Values, status int, location string) *httptest.ResponseRecorder {
		t.Helper()
		w := f.send(t, persona, method, path, form)
		if w.Code != status || !strings.HasPrefix(publicIDPattern.ReplaceAllString(w.Header().Get("Location"), "*"), location) {
			t.Fatalf("%s %s as %s: %d %q, want %d %q\n%.400s", method, path, persona, w.Code, w.Header().Get("Location"), status, location, w.Body)
		}
		return w
	}
	requested := func() int {
		return e2eCount(t, pool, "SELECT count(*) FROM build_intents WHERE requested_by IS NOT NULL")
	}
	served := func(what string, marker string) string {
		t.Helper()
		var label string
		e2eWait(t, ctx, what, 10*time.Minute, func() (string, error) {
			var status string
			var expires *time.Time
			if err := pool.QueryRow(ctx, "SELECT e.status, e.subdomain, e.expires_at FROM environments e JOIN pull_requests p ON p.id=e.pull_request_id WHERE p.repo_id=$1 AND p.number=$2",
				f.repoA, e2ePRNumber).Scan(&status, &label, &expires); err != nil {
				return "", err
			}
			if status == "failed" {
				return "", errStop(e2eLog(t, pool, f.prA))
			}
			if status != "running" || expires == nil {
				return "", fmt.Errorf("environment %s", status)
			}
			body, err := router.get(label + "." + e2eDomain)
			if err == nil && body != marker {
				err = fmt.Errorf("serves %q", body)
			}
			return body, err
		})
		return label + "." + e2eDomain
	}

	t.Log("1. A signed-out visitor is sent to GitHub sign-in; signed in, the Owner sees the connected repository")
	expect(personaAnonymous, http.MethodGet, "/dashboard", nil, http.StatusSeeOther, "/auth/github/login")
	if w := expect(personaOwner, http.MethodGet, "/repositories", nil, http.StatusOK, ""); !strings.Contains(w.Body.String(), "octo/app") {
		t.Fatal("the connected repository is not listed")
	}

	t.Log("2. GitHub opens PR #7 with a signed webhook; Manual mode builds nothing")
	f.webhook(t, "opened", e2eCommit1, http.StatusNoContent)
	f.scan(t, "SELECT id FROM pull_requests WHERE repo_id=$1 AND number=$2", []any{f.repoA, e2ePRNumber}, &f.prA)
	f.public["prA"] = f.publicID(t, "pull_requests", f.prA)
	prURL := repoURL + "/pull-requests/" + f.public["prA"]
	if w := expect(personaOwner, http.MethodGet, repoURL, nil, http.StatusOK, ""); !strings.Contains(w.Body.String(), "Journey change") {
		t.Fatal("the opened PR is not on the repository page")
	}
	time.Sleep(2 * time.Second)
	if n := requested(); n != 0 {
		t.Fatalf("a Manual PR has %d build requests", n)
	}

	t.Log("3. The Reviewer, a Developer without GitHub access, a removed member and an outsider cannot build")
	expect(personaReviewer, http.MethodGet, prURL, nil, http.StatusSeeOther, "/repositories/*/pull-requests/*/review")
	for _, persona := range []string{personaReviewer, personaDevNoSource, personaRemoved, personaOutsider} {
		expect(persona, http.MethodPost, prURL+"/build", nil, http.StatusNotFound, "")
	}
	if n := requested(); n != 0 {
		t.Fatalf("refused people recorded %d build requests", n)
	}

	t.Log("4. The granted Developer builds the latest commit; the worker publishes Deployment 1")
	if w := expect(personaDeveloper, http.MethodGet, prURL, nil, http.StatusOK, ""); !strings.Contains(w.Body.String(), "Build latest commit") {
		t.Fatal("the Developer is not offered Build latest commit")
	}
	expect(personaDeveloper, http.MethodPost, prURL+"/build", url.Values{"request_key": {"journey-build-request-0001"}}, http.StatusSeeOther, "/repositories/*")
	host := served("Deployment 1 to be served", "journey-v1")
	e2eWait(t, ctx, "the PR report to say Ready", 2*time.Minute, func() (string, error) {
		if c := github.livingComment(); !strings.Contains(c, "Previa preview: Ready") || !strings.Contains(c, host) {
			return "", errors.New("not reported yet")
		}
		return "", nil
	})

	t.Log("5. The Developer reads the build log; the Reviewer is kept to previews")
	expect(personaDeveloper, http.MethodGet, prURL+"/logs", nil, http.StatusOK, "")
	expect(personaDeveloper, http.MethodGet, prURL+"/logs/recent", nil, http.StatusOK, "")
	expect(personaReviewer, http.MethodGet, prURL+"/logs", nil, http.StatusSeeOther, "/previews")

	t.Log("6. The Reviewer opens the preview and records a finding on Deployment 1")
	expect(personaReviewer, http.MethodGet, prURL+"/open", nil, http.StatusSeeOther, "https://"+host)
	if w := expect(personaReviewer, http.MethodGet, prURL+"/review", nil, http.StatusOK, ""); !strings.Contains(w.Body.String(), `data-review-deployment="1"`) {
		t.Fatal("the review page does not show Deployment 1")
	}
	expect(personaReviewer, http.MethodPost, prURL+"/review/findings",
		url.Values{"body": {"The heading is wrong."}, "deployment": {"1"}, "request_key": {"journey-finding-key-0001"}}, http.StatusSeeOther, "/repositories/*/pull-requests/*/review")

	t.Log("7. A new commit is pushed; Manual keeps serving Deployment 1 until the Developer rebuilds it as Deployment 2 at the same address")
	github.push(e2eCommit2, e2eApp("journey-v2"))
	f.webhook(t, "synchronize", e2eCommit2, http.StatusNoContent)
	time.Sleep(2 * time.Second)
	if body, err := router.get(host); err != nil || body != "journey-v1" {
		t.Fatalf("Manual mode replaced the preview without a request: %q %v", body, err)
	}
	expect(personaDeveloper, http.MethodPost, prURL+"/build", url.Values{"request_key": {"journey-build-request-0002"}}, http.StatusSeeOther, "/repositories/*")
	if again := served("Deployment 2 to be served", "journey-v2"); again != host {
		t.Fatalf("the preview moved from %s to %s", host, again)
	}
	if n := e2eCount(t, pool, "SELECT count(*) FROM preview_publications WHERE pull_request_id=$1", f.prA); n != 2 {
		t.Fatalf("%d deployments recorded, want 2", n)
	}

	t.Log("8. The Reviewer resolves the finding on Deployment 2")
	expect(personaReviewer, http.MethodPost, prURL+"/review/findings/1/resolve", url.Values{"revision": {"1"}, "deployment": {"2"}},
		http.StatusSeeOther, "/repositories/*/pull-requests/*/review")
	if n := e2eCount(t, pool, `SELECT count(*) FROM preview_feedback_threads t JOIN preview_publications p ON p.id=t.resolved_publication_id
		WHERE t.pull_request_id=$1 AND t.number=1 AND t.state='resolved' AND p.sequence=2`, f.prA); n != 1 {
		t.Fatal("the finding is not resolved on Deployment 2")
	}

	t.Log("9. The Owner tears the preview down: route, containers, image and network go")
	expect(personaOwner, http.MethodPost, prURL+"/teardown", nil, http.StatusSeeOther, "/repositories/*")
	e2eWait(t, ctx, "the teardown to finish", 3*time.Minute, func() (string, error) {
		var status string
		if err := pool.QueryRow(ctx, "SELECT status FROM environments WHERE pull_request_id=$1", f.prA).Scan(&status); err != nil {
			return "", err
		}
		if status != "torn_down" {
			return "", fmt.Errorf("environment %s", status)
		}
		return "", e2eLeftovers(ctx, docker, instance, f.prA)
	})
	if body, err := router.get(host); err == nil && strings.HasPrefix(body, "journey-") {
		t.Fatal("the route still serves the removed preview")
	}

	t.Log("10. GitHub closes the PR: its review stays readable and nobody can build it")
	f.webhook(t, "closed", e2eCommit2, http.StatusNoContent)
	expect(personaReviewer, http.MethodGet, prURL+"/review", nil, http.StatusOK, "")
	expect(personaDeveloper, http.MethodPost, prURL+"/build", nil, http.StatusConflict, "")
	if n := requested(); n != 1 {
		t.Fatalf("%d PRs have requests, want only PR #7", n)
	}
}

// webhook delivers a signed pull_request event for PR #7 of octo/app, as
// GitHub sends it to the App's webhook URL.
func (f *accessFixture) webhook(t *testing.T, action, sha string, status int) {
	t.Helper()
	state := "open"
	if action == "closed" {
		state = "closed"
	}
	body := fmt.Appendf(nil, `{"action":%q,"number":%d,"installation":{"id":900},"repository":{"id":%d,"full_name":"octo/app","default_branch":"main"},
"pull_request":{"number":%d,"state":%q,"title":"Journey change","updated_at":%q,"html_url":"https://github.com/octo/app/pull/%d",
"mergeable":true,"mergeable_state":"clean","user":{"login":"dev"},"head":{"sha":%q,"ref":"feature","repo":{"id":%d}},"base":{"ref":"main","repo":{"id":%d}}}}`,
		action, e2ePRNumber, e2eRepoID, e2ePRNumber, state, time.Now().UTC().Format(time.RFC3339Nano), e2ePRNumber, sha, e2eRepoID, e2eRepoID)
	r := httptest.NewRequest(http.MethodPost, "https://"+testAppHost+webhookPath, strings.NewReader(string(body)))
	r.RemoteAddr = "192.0.2.30:40000"
	r.Header.Set("X-GitHub-Event", "pull_request")
	r.Header.Set("X-GitHub-Delivery", fmt.Sprintf("journey-%s-%d", action, time.Now().UnixNano()))
	r.Header.Set("X-Hub-Signature-256", sign([]byte(testWebhookSecret), body))
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("%s webhook: %d %s", action, w.Code, w.Body)
	}
}
