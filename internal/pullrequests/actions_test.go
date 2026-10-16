package pullrequests_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/auth"
	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/installations"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/workers"
	pr "github.com/ajaykathwatee/previa/internal/pullrequests"
	"github.com/alexedwards/scs/v2/memstore"
	"github.com/go-chi/chi/v5"
)

type fixedCapabilities struct{ execution, paused, discovery bool }

func (c *fixedCapabilities) Current(context.Context) (workers.Capabilities, error) {
	return workers.Capabilities{Execution: c.execution, Discovery: c.discovery, Paused: c.paused}, nil
}

// actionsHarness is the PR workspace with build and teardown actions, signed
// in as the fixture's owner.
type actionsHarness struct {
	*fixture
	router   http.Handler
	cookies  []*http.Cookie
	caps     *fixedCapabilities
	upstream *upstreamFake
	commits  *commitFake
	prID     int64
}

func newActionsHarness(t *testing.T) *actionsHarness {
	t.Helper()
	f := setup(t)
	h := &actionsHarness{fixture: f, caps: &fixedCapabilities{execution: true, discovery: true}, commits: &commitFake{}}
	h.prID = seedPR(t, f, `"mergeable":true,"mergeable_state":"clean",`)
	h.upstream = &upstreamFake{inspect: func(context.Context, int64, int64, int32) (pr.Upstream, error) {
		return upstreamPR("sha-a", "open", yes(), "clean", verifyBase), nil
	}}
	operations, err := environments.NewOperationBroker(f.producer)
	if err != nil {
		t.Fatal(err)
	}
	repo := environments.NewRepository(f.pool, operations).WithReports()
	broker := environments.NewRequestBroker(f.pool, f.producer).WithReports(repo)
	sync := pr.NewSyncService(f.pool, f.producer, h.upstream, broker)
	sessions := auth.NewSessionManager(memstore.NewWithCleanupInterval(0), false, f.log)
	authHandler := auth.NewHandler(config.ServerConfig{AppBaseURL: "http://localhost:8080", SessionSecret: strings.Repeat("s", 32)}, sessions, testUsers{f.userID}, testOAuth{}, f.log)
	policy, err := installations.NewBuildPolicyService(installations.NewRepository(f.pool).WithReports(broker))
	if err != nil {
		t.Fatal(err)
	}
	handler := pr.NewHandler(pr.NewRepository(f.pool), sync, policy, authHandler, f.log, true).
		WithActions(pr.Actions{Builds: broker, Verifier: sync, Teardowns: repo, Capabilities: h.caps, PreviewBaseDomain: "preview.example.com"}).
		WithCommits(h.commits)
	router := chi.NewRouter()
	router.Use(sessions.LoadAndSave, authHandler.LoadUser, authHandler.CSRF)
	authHandler.Mount(router)
	handler.Mount(router, auth.RequireUser)
	h.router = router
	w := h.request(t, "GET", "/auth/github/login", "")
	h.request(t, "GET", w.Header().Get("Location"), "")
	return h
}

func (h *actionsHarness) request(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range h.cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, r)
	if c := w.Result().Cookies(); len(c) > 0 {
		h.cookies = c
	}
	return w
}

const detailPath = "/repositories/1/pull-requests/7"

func TestAutomaticBuildActionStopsOnceQueued(t *testing.T) {
	h := newActionsHarness(t)
	h.exec(t, `INSERT INTO build_intents(pull_request_id, revision, base_revision, generation, head_sha, source, request_action, idempotency_key, state, available_at)
		SELECT id,1,0,generation,head_sha,'automatic','automatic','automatic-test','pending',now()+interval '5 minutes' FROM pull_requests WHERE id=$1`, h.prID)
	form, body := h.form(t)
	if form == nil || !strings.Contains(body, "Build now") {
		t.Fatal("automatic quiet period must allow Build now")
	}
	for _, state := range []string{"queued", "running"} {
		h.exec(t, "UPDATE build_intents SET state=$1, started_at=CASE WHEN $1='running' THEN now() ELSE NULL END WHERE pull_request_id=$2", state, h.prID)
		form, body := h.form(t)
		if form != nil {
			t.Fatalf("%s still offers a build form", state)
		}
		label := "Build queued"
		if state == "running" {
			label = "Building preview"
		}
		if !strings.Contains(body, label) {
			t.Fatalf("%s missing progress label", state)
		}
	}
}

// form reads the detail page and returns its build form's values (or nil)
// and the page body.
func (h *actionsHarness) form(t *testing.T) (url.Values, string) {
	t.Helper()
	w := h.request(t, "GET", detailPath, "")
	if w.Code != http.StatusOK {
		t.Fatalf("detail %d", w.Code)
	}
	body := w.Body.String()
	values := url.Values{}
	token := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`).FindStringSubmatch(body)
	if len(token) == 2 {
		values.Set("csrf_token", token[1])
	}
	build := regexp.MustCompile(`(?s)action="` + regexp.QuoteMeta(detailPath) + `/build".*?</form>`).FindString(body)
	if build == "" {
		return nil, body
	}
	for _, name := range []string{"action", "expected_revision", "request_key"} {
		if m := regexp.MustCompile(`name="` + name + `" value="([^"]*)"`).FindStringSubmatch(build); len(m) == 2 {
			values.Set(name, m[1])
		}
	}
	return values, body
}

func (h *actionsHarness) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := h.pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func (h *actionsHarness) n(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (h *actionsHarness) intent(t *testing.T) (revision int64, state, sha, source string) {
	t.Helper()
	if err := h.pool.QueryRow(t.Context(), "SELECT revision, state, head_sha, source FROM build_intents WHERE pull_request_id=$1", h.prID).Scan(&revision, &state, &sha, &source); err != nil {
		t.Fatal(err)
	}
	return
}

func TestBuildIsUnavailableWithoutAWorkerThatCanBuild(t *testing.T) {
	h := newActionsHarness(t)
	h.caps.execution = false
	values, body := h.form(t)
	if values != nil {
		t.Fatal("a build control was offered with no worker able to build")
	}
	// The action stays visible but disabled, with the reason and the fix.
	disabled := regexp.MustCompile(`(?s)<button[^>]*disabled[^>]*aria-describedby="build-unavailable"[^>]*>.*?Build latest commit`)
	if !disabled.MatchString(body) || !strings.Contains(body, "it has no preview domain") || !strings.Contains(body, "PREVIEW_BASE_DOMAIN") {
		t.Fatal("the blocked build was not shown disabled with its reason and fix")
	}
	h.caps.discovery = false
	if _, body := h.form(t); !strings.Contains(body, "No worker that can build previews is running") {
		t.Fatal("with no worker at all, the page did not say so")
	}
	h.caps.discovery = true
	token := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`).FindStringSubmatch(body)[1]
	w := h.request(t, "POST", detailPath+"/build", "csrf_token="+url.QueryEscape(token)+"&action=build&expected_revision=0&request_key=abcdefghijklmnop1234")
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "No build was requested") {
		t.Fatalf("build without a worker: %d %s", w.Code, w.Body.String())
	}
	if h.n(t, "SELECT count(*) FROM build_intents") != 0 || h.upstream.calls != 0 {
		t.Fatal("a refused build recorded intent or read GitHub")
	}
	// Paused by the operator: the same refusal, with the reason.
	h.caps.paused = true
	if values, body := h.form(t); values != nil || !strings.Contains(body, "Builds are paused by the operator") {
		t.Fatal("a paused server did not say builds are paused")
	}
	w = h.request(t, "POST", detailPath+"/build", "csrf_token="+url.QueryEscape(token)+"&action=build&expected_revision=0&request_key=abcdefghijklmnop5678")
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "paused") || h.n(t, "SELECT count(*) FROM build_intents") != 0 {
		t.Fatalf("build while paused: %d %s", w.Code, w.Body.String())
	}
}

func TestBuildRetryRebuildAreIdempotentAndRevisionChecked(t *testing.T) {
	h := newActionsHarness(t)
	values, body := h.form(t)
	if values == nil || values.Get("action") != "build" || !strings.Contains(body, "Build latest commit") {
		t.Fatalf("no Build latest commit control: %v", values)
	}
	if w := h.request(t, "POST", detailPath+"/build", values.Encode()); w.Code != http.StatusSeeOther {
		t.Fatalf("build %d %s", w.Code, w.Body.String())
	}
	revision, state, sha, source := h.intent(t)
	if revision != 1 || state != "pending" && state != "queued" || sha != "sha-a" || source != "manual" || h.upstream.calls != 1 {
		t.Fatalf("request %d %s %s %s (GitHub read %d times)", revision, state, sha, source, h.upstream.calls)
	}
	if h.n(t, "SELECT count(*) FROM river_job WHERE kind='environment_wake'") != 1 {
		t.Fatal("the request has no durable wake-up")
	}
	// A double click submits the same form again: nothing new is recorded.
	if w := h.request(t, "POST", detailPath+"/build", values.Encode()); w.Code != http.StatusSeeOther {
		t.Fatalf("duplicate submit %d %s", w.Code, w.Body.String())
	}
	if again, _, _, _ := h.intent(t); again != revision || h.n(t, "SELECT count(*) FROM river_job WHERE kind='environment_wake'") != 1 {
		t.Fatal("a duplicate submit recorded a second request")
	}
	if next, _ := h.form(t); next != nil {
		t.Fatal("a build control is offered while a manual request is queued")
	}

	// Failed: Retry, with the revision the page showed.
	h.exec(t, "UPDATE build_intents SET state='failed', started_at=now(), completed_at=now() WHERE pull_request_id=$1", h.prID)
	values, _ = h.form(t)
	if values.Get("action") != "retry" || values.Get("expected_revision") != "1" {
		t.Fatalf("retry control %v", values)
	}
	stale := url.Values{}
	for k, v := range values {
		stale[k] = v
	}
	if w := h.request(t, "POST", detailPath+"/build", values.Encode()); w.Code != http.StatusSeeOther {
		t.Fatalf("retry %d %s", w.Code, w.Body.String())
	}
	if revision, state, _, _ := h.intent(t); revision != 2 || state == "failed" {
		t.Fatalf("retry recorded revision %d %s", revision, state)
	}
	// The same Retry from a page that is now out of date is refused.
	stale.Set("request_key", "another-key-from-an-old-tab")
	if w := h.request(t, "POST", detailPath+"/build", stale.Encode()); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "changed meanwhile") {
		t.Fatalf("stale retry %d %s", w.Code, w.Body.String())
	}

	// Succeeded: Rebuild the same commit.
	h.exec(t, "UPDATE build_intents SET state='succeeded', started_at=now(), completed_at=now() WHERE pull_request_id=$1", h.prID)
	values, _ = h.form(t)
	if values.Get("action") != "rebuild" {
		t.Fatalf("rebuild control %v", values)
	}
	if w := h.request(t, "POST", detailPath+"/build", values.Encode()); w.Code != http.StatusSeeOther {
		t.Fatalf("rebuild %d %s", w.Code, w.Body.String())
	}
	if revision, _, sha, _ := h.intent(t); revision != 3 || sha != "sha-a" {
		t.Fatalf("rebuild recorded revision %d of %s", revision, sha)
	}
}

func TestBuildUsesTheCommitGitHubReportsNow(t *testing.T) {
	h := newActionsHarness(t)
	values, _ := h.form(t)
	h.upstream.inspect = func(context.Context, int64, int64, int32) (pr.Upstream, error) {
		return upstreamPR("sha-b", "open", yes(), "clean", verifyBase.Add(time.Minute)), nil
	}
	if w := h.request(t, "POST", detailPath+"/build", values.Encode()); w.Code != http.StatusSeeOther {
		t.Fatalf("build %d %s", w.Code, w.Body.String())
	}
	if _, _, sha, _ := h.intent(t); sha != "sha-b" {
		t.Fatalf("built %s instead of the commit GitHub reports now", sha)
	}
	// GitHub unreachable: nothing is requested.
	h.exec(t, "DELETE FROM build_intents")
	h.upstream.inspect = func(context.Context, int64, int64, int32) (pr.Upstream, error) {
		return pr.Upstream{}, environments.ErrUpstreamUnavailable
	}
	values, _ = h.form(t)
	if w := h.request(t, "POST", detailPath+"/build", values.Encode()); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "No build was requested") {
		t.Fatalf("GitHub outage %d %s", w.Code, w.Body.String())
	}
	if h.n(t, "SELECT count(*) FROM build_intents") != 0 {
		t.Fatal("a build was requested without a fresh read")
	}
	// GitHub reports the PR closed: refused, and the close is recorded.
	h.upstream.inspect = func(context.Context, int64, int64, int32) (pr.Upstream, error) {
		return upstreamPR("sha-b", "closed", yes(), "clean", verifyBase.Add(2*time.Minute)), nil
	}
	if w := h.request(t, "POST", detailPath+"/build", values.Encode()); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "closed") {
		t.Fatalf("closed upstream %d %s", w.Code, w.Body.String())
	}
}

func TestTeardownIsConfirmedOwnerOnlyAndIdempotent(t *testing.T) {
	h := newActionsHarness(t)
	h.exec(t, `INSERT INTO environments(pull_request_id, subdomain, status, container_id, image_tag, head_sha, applied_generation, applied_revision, expires_at)
		SELECT id, 'pr-7-2s-abcdefgh', 'running', 'container-1', 'image-1', head_sha, generation, 0, now() + interval '2 days' FROM pull_requests WHERE id=$1`, h.prID)
	_, body := h.form(t)
	for _, want := range []string{`data-open-dialog="teardown-preview"`, `id="teardown-preview"`, "https://pr-7-2s-abcdefgh.preview.example.com", "Expires"} {
		if !strings.Contains(body, want) {
			t.Fatalf("detail lacks %q", want)
		}
	}
	token := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`).FindStringSubmatch(body)[1]
	if w := h.request(t, "POST", detailPath+"/teardown", ""); w.Code != http.StatusForbidden {
		t.Fatalf("teardown without CSRF %d", w.Code)
	}
	if w := h.request(t, "POST", "/repositories/999/pull-requests/7/teardown", "csrf_token="+url.QueryEscape(token)); w.Code != http.StatusNotFound {
		t.Fatalf("teardown of another owner's PR %d", w.Code)
	}
	if w := h.request(t, "POST", detailPath+"/teardown", "csrf_token="+url.QueryEscape(token)); w.Code != http.StatusSeeOther {
		t.Fatalf("teardown %d %s", w.Code, w.Body.String())
	}
	var status, reason string
	if err := h.pool.QueryRow(t.Context(), `SELECT e.status, pr.teardown_reason FROM environments e JOIN pull_requests pr ON pr.id=e.pull_request_id WHERE pr.id=$1`, h.prID).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "tearing_down" || reason != "requested" {
		t.Fatalf("teardown recorded %s/%s", status, reason)
	}
	if h.n(t, "SELECT count(*) FROM environment_operations WHERE kind='remove_route' AND pull_request_id=$1", h.prID) != 1 {
		t.Fatal("no durable route removal")
	}
	if w := h.request(t, "POST", detailPath+"/teardown", "csrf_token="+url.QueryEscape(token)); w.Code != http.StatusSeeOther {
		t.Fatalf("repeated teardown %d", w.Code)
	}
	if h.n(t, "SELECT count(*) FROM environment_operations WHERE kind='remove_route' AND pull_request_id=$1", h.prID) != 1 {
		t.Fatal("a repeated teardown removed twice")
	}
	if _, body := h.form(t); strings.Contains(body, `data-open-dialog="teardown-preview"`) {
		t.Fatal("teardown still offered for a preview being removed")
	}
	// From the Previews page the owner returns there; a form from before the
	// page was renamed ("environments") does too.
	for _, from := range []string{"previews", "environments"} {
		h.exec(t, "UPDATE pull_requests SET teardown_requested_generation=-1 WHERE id=$1", h.prID)
		h.exec(t, "UPDATE environments SET status='running' WHERE pull_request_id=$1", h.prID)
		w := h.request(t, "POST", detailPath+"/teardown", "csrf_token="+url.QueryEscape(token)+"&return="+from+"&page=2")
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/previews?page=2" {
			t.Fatalf("return to Previews from %q: %d %s", from, w.Code, w.Header().Get("Location"))
		}
	}
	if w := h.request(t, "POST", detailPath+"/teardown", "csrf_token="+url.QueryEscape(token)+"&return=https://evil.example"); w.Code != http.StatusSeeOther || strings.Contains(w.Header().Get("Location"), "evil") {
		t.Fatalf("open redirect: %s", w.Header().Get("Location"))
	}
}

// TestActionsRefuseAnotherOwnersPullRequest signs in as one owner and aims
// every action at a real PR in a repository another owner linked.
func TestActionsRefuseAnotherOwnersPullRequest(t *testing.T) {
	h := newActionsHarness(t)
	var other, otherRepo int64
	if err := h.pool.QueryRow(t.Context(), `INSERT INTO users(github_id,username)VALUES(43,'stranger')RETURNING id`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	h.exec(t, `INSERT INTO installations(github_installation_id,owner_user_id,account_login)VALUES(98,$1,'stranger')`, other)
	if err := h.pool.QueryRow(t.Context(), `INSERT INTO repos(installation_id,github_repo_id,full_name)SELECT id,200,'stranger/repo' FROM installations WHERE github_installation_id=98 RETURNING id`).Scan(&otherRepo); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"action":"opened","installation":{"id":98},"repository":{"id":200},"pull_request":{"number":7,"title":"Theirs","state":"open","mergeable":true,"mergeable_state":"clean","updated_at":%q,"user":{"login":"contributor"},"html_url":"https://github.com/stranger/repo/pull/7","head":{"sha":"sha-x","ref":"feature","repo":{"id":200}}}}`, verifyBase.Format(time.RFC3339))
	if code := send(h.hook, "stranger-opened", "pull_request", body); code != 204 {
		t.Fatalf("seed webhook: %d", code)
	}
	var theirs int64
	if err := h.pool.QueryRow(t.Context(), "SELECT pr.id FROM pull_requests pr JOIN repos r ON r.id=pr.repo_id WHERE r.id=$1 AND pr.number=7", otherRepo).Scan(&theirs); err != nil {
		t.Fatal(err)
	}
	h.exec(t, `INSERT INTO environments(pull_request_id, subdomain, status, container_id, image_tag, head_sha, applied_generation, applied_revision)
		SELECT id, 'pr-7-5k-stranger', 'running', 'container-x', 'image-x', head_sha, generation, 0 FROM pull_requests WHERE id=$1`, theirs)

	values, _ := h.form(t) // a valid CSRF token for the signed-in owner
	theirPath := "/repositories/" + strconv.FormatInt(otherRepo, 10) + "/pull-requests/7"
	if w := h.request(t, "GET", theirPath, ""); w.Code != http.StatusNotFound {
		t.Fatalf("another owner's PR page: %d", w.Code)
	}
	for _, action := range []string{"build", "retry", "rebuild"} {
		form := url.Values{"csrf_token": {values.Get("csrf_token")}, "action": {action}, "expected_revision": {"0"}, "request_key": {"k-" + action}}
		if w := h.request(t, "POST", theirPath+"/build", form.Encode()); w.Code != http.StatusNotFound {
			t.Fatalf("%s of another owner's PR: %d", action, w.Code)
		}
	}
	if w := h.request(t, "POST", theirPath+"/teardown", "csrf_token="+url.QueryEscape(values.Get("csrf_token"))); w.Code != http.StatusNotFound {
		t.Fatalf("teardown of another owner's PR: %d", w.Code)
	}
	if n := h.n(t, "SELECT count(*) FROM build_intents WHERE pull_request_id=$1", theirs); n != 0 {
		t.Fatalf("%d build requests recorded for another owner's PR", n)
	}
	if n := h.n(t, "SELECT count(*) FROM pull_requests WHERE id=$1 AND teardown_reason IS NOT NULL", theirs); n != 0 {
		t.Fatal("a teardown was recorded for another owner's PR")
	}
	if n := h.n(t, "SELECT count(*) FROM environments WHERE pull_request_id=$1 AND status='running'", theirs); n != 1 {
		t.Fatal("another owner's preview changed")
	}
	// Neither list shows it.
	for _, page := range []string{"/builds", "/previews"} {
		if w := h.request(t, "GET", page, ""); w.Code != http.StatusOK || strings.Contains(w.Body.String(), "stranger/repo") {
			t.Fatalf("%s shows another owner's PR (%d)", page, w.Code)
		}
	}
}

func TestPreviewsAndBuildsShowRealDeliveryState(t *testing.T) {
	h := newActionsHarness(t)
	h.exec(t, `INSERT INTO environments(pull_request_id, subdomain, status, container_id, image_tag, head_sha, applied_generation, applied_revision, expires_at)
		SELECT id, 'pr-7-2s-abcdefgh', 'running', 'container-1', 'image-1', 'sha-old', generation, 0, now() + interval '2 days' FROM pull_requests WHERE id=$1`, h.prID)
	w := h.request(t, "GET", "/previews", "")
	body := w.Body.String()
	for _, want := range []string{"https://pr-7-2s-abcdefgh.preview.example.com", "Earlier than the latest commit", "Tear down", `name="return" value="previews"`, ">Live<", "Open preview"} {
		if !strings.Contains(body, want) {
			t.Fatalf("Previews lacks %q", want)
		}
	}
	h.exec(t, `INSERT INTO build_intents(pull_request_id, revision, base_revision, generation, head_sha, source, request_action, requested_by, idempotency_key, state, available_at, started_at, waiting_revision, waiting_reason)
		SELECT id, 1, 0, generation, head_sha, 'manual', 'build', $2, 'key-waiting-0000001', 'running', now(), now(), 1, 'capacity' FROM pull_requests WHERE id=$1`, h.prID, h.userID)
	if body := h.request(t, "GET", "/builds", "").Body.String(); !strings.Contains(body, "Waiting for capacity") {
		t.Fatal("Builds does not show the wait")
	}
	if _, body := h.form(t); !strings.Contains(body, "Waiting for capacity") || !strings.Contains(body, "starts automatically") {
		t.Fatal("the PR does not explain its wait")
	}
	h.exec(t, "UPDATE build_intents SET state='failed', completed_at=now(), waiting_revision=NULL, waiting_reason=NULL WHERE pull_request_id=$1", h.prID)
	h.exec(t, "UPDATE environments SET last_error_code='unhealthy', applied_revision=1 WHERE pull_request_id=$1", h.prID)
	if _, body := h.form(t); !strings.Contains(body, "The app did not become ready.") {
		t.Fatal("the PR does not explain its failure")
	}
}

func TestRepositoryPageSaysWhenSyncsWaitStallOrNeverResolve(t *testing.T) {
	h := newActionsHarness(t)
	list := func() string {
		t.Helper()
		w := h.request(t, "GET", "/repositories/1", "")
		if w.Code != http.StatusOK {
			t.Fatalf("list %d", w.Code)
		}
		return w.Body.String()
	}
	syncButton := regexp.MustCompile(`(?s)<button[^>]*class="[^"]*sync-button[^"]*"[^>]*>`)
	disabled := func(button string) bool { return regexp.MustCompile(`\sdisabled(\s|>|=)`).MatchString(button) }
	h.exec(t, "INSERT INTO pr_syncs(repo_id,state) VALUES(1,'queued') ON CONFLICT (repo_id) DO UPDATE SET state='queued', requested_at=clock_timestamp()")
	if body := list(); !strings.Contains(body, "is-syncing") || !strings.Contains(body, "data-refresh-at=") || !disabled(syncButton.FindString(body)) {
		t.Fatal("a fresh sync spins, refreshes when it would stall, and is not requested twice")
	}
	// No worker: no spinner, an explanation and how to start one.
	h.caps.discovery = false
	if body := list(); strings.Contains(body, "is-syncing") || !strings.Contains(body, "Waiting for a worker") || !strings.Contains(body, "data-worker-missing") || !strings.Contains(body, "make run-worker") {
		t.Fatal("a sync with no worker must say it waits for one")
	}
	h.caps.discovery = true
	// Older than SyncStaleAfter: stalled, and Sync works again.
	h.exec(t, "UPDATE pr_syncs SET requested_at = now() - interval '6 minutes'")
	if body := list(); !strings.Contains(body, "Sync stalled") || !strings.Contains(body, "data-sync-stalled") || disabled(syncButton.FindString(body)) {
		t.Fatal("a stalled sync must say so and let the owner sync again")
	}
	// A mergeability check that gave up reads Unknown, with Sync as the way on.
	h.exec(t, "UPDATE pull_requests SET build_status='checking', mergeable=NULL, mergeability_unresolved_at=now() WHERE id=$1", h.prID)
	if body := list(); !strings.Contains(body, ">Unknown<") || !strings.Contains(body, "GitHub did not answer; sync to ask again") {
		t.Fatal("an unresolved check must read Unknown in the list")
	}
	values, body := h.form(t)
	if values != nil || !strings.Contains(body, "GitHub did not report mergeability") || !strings.Contains(body, "Choose Sync Pull Requests on the repository page to ask again") {
		t.Fatal("the PR page must say GitHub did not answer and how to ask again")
	}
	// Still checking, but no worker to ask: the page says so and how to fix it.
	h.exec(t, "UPDATE pull_requests SET mergeability_unresolved_at=NULL WHERE id=$1", h.prID)
	h.caps.discovery = false
	if values, body := h.form(t); values != nil || !strings.Contains(body, "Waiting for a worker to check mergeability") || !strings.Contains(body, "No worker is running to ask GitHub") {
		t.Fatal("the PR page must explain the missing worker")
	}
}

// Previews lists only previews that serve a commit. A first build that failed
// before going live reserves an address but is not a preview: it is on Builds.
// Once a version is live, a failed update keeps it listed, as live.
func TestPreviewsListOnlyWhatServes(t *testing.T) {
	h := newActionsHarness(t)
	h.exec(t, `INSERT INTO build_intents(pull_request_id, revision, base_revision, generation, head_sha, source, request_action, requested_by, idempotency_key, state, available_at, started_at, completed_at)
		SELECT id, 1, 0, generation, head_sha, 'manual', 'build', $2, 'key-failed-00000001', 'failed', now(), now(), now() FROM pull_requests WHERE id=$1`, h.prID, h.userID)
	h.exec(t, `INSERT INTO environments(pull_request_id, subdomain, status, applied_generation, applied_revision, last_error_code, expires_at)
		SELECT id, 'pr-7-2s-abcdefgh', 'failed', generation, 1, 'source_failed', now() + interval '2 days' FROM pull_requests WHERE id=$1`, h.prID)
	listed := func(path string) bool {
		t.Helper()
		return strings.Contains(h.request(t, "GET", path, "").Body.String(), "/repositories/1/pull-requests/7")
	}
	if listed("/previews") || !listed("/builds") {
		t.Fatal("a build that never went live is on Builds, not Previews")
	}
	if body := h.request(t, "GET", "/previews", "").Body.String(); !strings.Contains(body, "No live previews.") {
		t.Fatal("an empty Previews says nothing is live and where failed builds are")
	}
	h.exec(t, "UPDATE environments SET head_sha='sha-old', container_id='container-1', image_tag='image-1', last_error_code='unhealthy' WHERE pull_request_id=$1", h.prID)
	body := h.request(t, "GET", "/previews", "").Body.String()
	for _, want := range []string{"Live · update failed", "Newer build failed:", "https://pr-7-2s-abcdefgh.preview.example.com"} {
		if !strings.Contains(body, want) {
			t.Fatalf("a live preview whose update failed lacks %q", want)
		}
	}
	if w := h.request(t, "GET", "/environments?page=2", ""); w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "/previews?page=2" {
		t.Fatalf("old links to Environments move to Previews: %d %q", w.Code, w.Header().Get("Location"))
	}
}

// The review page's Reset test data posts a Rebuild with reset=true and
// return=review: it comes back to the review page with what happened.
func TestResetFromTheReviewPageReturnsThere(t *testing.T) {
	h := newActionsHarness(t)
	values, _ := h.form(t)
	if w := h.request(t, "POST", detailPath+"/build", values.Encode()); w.Code != http.StatusSeeOther {
		t.Fatalf("first build: %d", w.Code)
	}
	h.exec(t, "UPDATE build_intents SET state='succeeded', started_at=now(), completed_at=now() WHERE pull_request_id=$1", h.prID)
	reset := url.Values{"csrf_token": {values.Get("csrf_token")}, "action": {"rebuild"}, "reset": {"true"}, "return": {"review"},
		"workspace_query": {"pr=7"}, "expected_revision": {"1"}, "request_key": {"review-reset-key-0000001"}}
	w := h.request(t, "POST", detailPath+"/build", reset.Encode())
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/repositories/1/pull-requests/7/review?saved=reset" {
		t.Fatalf("reset: %d %s", w.Code, w.Header().Get("Location"))
	}
	if revision, _, _, _ := h.intent(t); revision != 2 {
		t.Fatalf("the Rebuild was not requested: revision %d", revision)
	}
	// A reset from a page that is out of date explains itself there.
	reset.Set("request_key", "review-reset-key-0000002")
	w = h.request(t, "POST", detailPath+"/build", reset.Encode())
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/repositories/1/pull-requests/7/review?notice=build-changed" {
		t.Fatalf("a stale reset: %d %s", w.Code, w.Header().Get("Location"))
	}
	h.exec(t, "UPDATE pull_requests SET state='closed' WHERE id=$1", h.prID)
	w = h.request(t, "POST", detailPath+"/build", reset.Encode())
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/repositories/1/pull-requests/7/review?notice=build-closed" {
		t.Fatalf("a reset of a closed pull request: %d %s", w.Code, w.Header().Get("Location"))
	}
}

// The Setup tab posts to the same Build and Sync actions with return=setup:
// each comes back to the checklist, on the pull request it showed, with what
// happened, never to a plain-text error page.
func TestSetupTabActionsReturnToTheChecklist(t *testing.T) {
	h := newActionsHarness(t)
	values, _ := h.form(t)
	values.Set("return", "setup")
	values.Set("workspace_query", "pr=7")
	w := h.request(t, "POST", detailPath+"/build", values.Encode())
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/repositories/1/setup?pr=7&saved=build" {
		t.Fatalf("build from setup: %d %s", w.Code, w.Header().Get("Location"))
	}
	if revision, _, _, _ := h.intent(t); revision != 1 {
		t.Fatalf("the build was not requested: revision %d", revision)
	}
	h.exec(t, "UPDATE build_intents SET state='failed', started_at=now(), completed_at=now() WHERE pull_request_id=$1", h.prID)
	values, _ = h.form(t)
	values.Set("return", "setup")
	values.Set("workspace_query", "pr=7")
	values.Set("expected_revision", "0")
	values.Set("request_key", "a-key-from-an-old-setup-tab")
	w = h.request(t, "POST", detailPath+"/build", values.Encode())
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/repositories/1/setup?pr=7&notice=build-changed" {
		t.Fatalf("a stale build from setup: %d %s", w.Code, w.Header().Get("Location"))
	}
	w = h.request(t, "POST", "/repositories/1/refresh", url.Values{"csrf_token": {values.Get("csrf_token")}, "return": {"setup"}}.Encode())
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/repositories/1/setup?saved=sync" {
		t.Fatalf("sync from setup: %d %s", w.Code, w.Header().Get("Location"))
	}
}
