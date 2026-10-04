package pullrequests_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/ajaykathwatee/previa/internal/auth"
	"github.com/ajaykathwatee/previa/internal/installations"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	pr "github.com/ajaykathwatee/previa/internal/pullrequests"
	"github.com/alexedwards/scs/v2/memstore"
	"github.com/go-chi/chi/v5"
)

type testUsers struct{ userID int64 }

func (u testUsers) UpsertUser(context.Context, auth.Identity) (auth.User, error) {
	return auth.User{ID: u.userID, GitHubID: 42, Username: "owner"}, nil
}
func (u testUsers) GetUser(context.Context, int64) (auth.User, error) {
	return auth.User{ID: u.userID, GitHubID: 42, Username: "owner"}, nil
}

type testOAuth struct{}

func (testOAuth) AuthCodeURL(s string) string {
	return "/auth/github/callback?code=test&state=" + url.QueryEscape(s)
}
func (testOAuth) Authenticate(context.Context, string) (auth.Identity, error) {
	return auth.Identity{ID: 42, Username: "owner"}, nil
}
func TestWorkspaceHTTPAuthorizationCSRFActions(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	if send(f.hook, "seed", "pull_request", payload("opened", "sha-a", "open", "2026-10-03T10:00:00Z")) != 204 {
		t.Fatal("seed")
	}
	store := memstore.NewWithCleanupInterval(0)
	sessions := auth.NewSessionManager(store, false, f.log)
	authHandler := auth.NewHandler(config.ServerConfig{AppBaseURL: "http://localhost:8080", SessionSecret: strings.Repeat("s", 32)}, sessions, testUsers{f.userID}, testOAuth{}, f.log)
	policy, err := installations.NewBuildPolicyService(installations.NewRepository(f.pool))
	if err != nil {
		t.Fatal(err)
	}
	syncService := pr.NewSyncService(f.pool, f.producer, nil)
	h := pr.NewHandler(pr.NewRepository(f.pool), syncService, policy, authHandler, f.log, true)
	router := chi.NewRouter()
	router.Use(sessions.LoadAndSave, authHandler.LoadUser, authHandler.CSRF)
	authHandler.Mount(router)
	installations.NewHandler(installations.NewService(installations.NewRepository(f.pool), nil, nil, nil), authHandler, sessions, "fixture-app", f.log).WithNavigator(h).Mount(router, auth.RequireUser)
	h.Mount(router, auth.RequireUser)
	var cookies []*http.Cookie
	request := func(method, path, body string, targets ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if len(targets) > 0 {
			r.Header.Set("HX-Request", "true")
			r.Header.Set("HX-Target", targets[0])
		}
		for _, c := range cookies {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if c := w.Result().Cookies(); len(c) > 0 {
			cookies = c
		}
		return w
	}
	base := "/repositories/1"
	workspace := base + "?pr_state=open"
	if w := request("GET", workspace, ""); w.Code != 303 {
		t.Fatal("unauthenticated access", w.Code)
	}
	w := request("GET", "/auth/github/login", "")
	if w.Code != 303 {
		t.Fatal("login", w.Code)
	}
	w = request("GET", w.Header().Get("Location"), "")
	if w.Code != 303 {
		t.Fatal("callback", w.Code)
	}
	w = request("GET", workspace, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "repository-filters") || !strings.Contains(w.Body.String(), "<!doctype") {
		t.Fatal("repository must render its own full page")
	}
	if legacy := request("GET", "/repositories?repo_id=1&pr=7&pr_q=Improve", ""); legacy.Code != 303 || !strings.HasPrefix(legacy.Header().Get("Location"), base+"/pull-requests/7?") {
		t.Fatal("query bookmark redirect", legacy.Code)
	}
	if directory := request("GET", "/repositories", ""); directory.Code != 200 || strings.Contains(directory.Body.String(), "repository-detail") {
		t.Fatal("directory must list only repositories", directory.Code)
	}
	match := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
	if len(match) != 2 {
		t.Fatal("missing CSRF")
	}
	token := url.QueryEscape(match[1])
	for _, path := range []string{base + "/policy", base + "/refresh", base + "/pull-requests/7/build"} {
		if w := request("POST", path, ""); w.Code != 403 {
			t.Fatal("missing CSRF accepted", path, w.Code)
		}
	}
	for _, path := range []string{"/repositories?repo_id=999", "/repositories?repo_id=999&pr=7", "/repositories/999", "/repositories/999/pull-requests/7", "/builds?repo_id=999", "/environments?repo_id=999"} {
		if w := request("GET", path, ""); w.Code != 404 {
			t.Fatal("unowned query", path, w.Code)
		}
	}
	for _, path := range []string{"/repositories/999/policy", "/repositories/999/refresh", "/repositories/999/pull-requests/7/build"} {
		if w := request("POST", path, "csrf_token="+token+"&mode=automatic"); w.Code != 404 {
			t.Fatal("unowned action", path, w.Code)
		}
	}
	if w := request("POST", base+"/policy", "csrf_token="+token+"&mode=automatic"); w.Code != 303 {
		t.Fatal("policy change", w.Code)
	}
	if mode, err := policy.Get(ctx, f.userID, f.repoID); err != nil || mode.Mode != installations.AutomaticBuilds {
		t.Fatal("policy not saved", err)
	}
	for range 2 {
		if w := request("POST", base+"/refresh", "csrf_token="+token); w.Code != 303 {
			t.Fatal("refresh", w.Code)
		}
	}
	if f.count(t, "SELECT count(*) FROM pr_syncs") != 1 || f.count(t, "SELECT count(*) FROM river_job WHERE kind='pr_discovery'") != 1 {
		t.Fatal("duplicate refresh")
	}
	if w := request("POST", base+"/pull-requests/7/build", "csrf_token="+token); w.Code != 503 {
		t.Fatal("runtime capability gate", w.Code)
	}
	if w := request("GET", base+"?page=999&q=Improve&state=open", ""); w.Code != 303 || !strings.Contains(w.Header().Get("Location"), "pr_q=Improve") {
		t.Fatal("legacy filter redirect", w.Code)
	}
	if w := request("GET", base+"/pull-requests/7", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "Latest observed commit") {
		t.Fatal("dedicated detail page", w.Code)
	}
	for _, target := range []string{"repository-detail", "pull-request-results"} {
		w := request("GET", workspace+"&pr_q=Improve&pr_state=open", "", target)
		if w.Code != 200 || strings.Contains(w.Body.String(), "<!doctype") || !strings.Contains(w.Body.String(), "Improve preview") {
			t.Fatal("partial response", target, w.Code, w.Body.String())
		}
		if target == "pull-request-results" && strings.Contains(w.Body.String(), "build-policy") {
			t.Fatal("results included whole panel")
		}
		if w := request("GET", "/repositories/999", "", target); w.Code != 404 {
			t.Fatal("partial unowned selection", target, w.Code)
		}
	}
	for _, query := range []string{"repo_id=0", "repo_id=no", "repo_id=1&pr_page=0", "repo_id=1&pr_state=no", "repo_id=1&pr=-7", "repo_id=1&pr_q=" + strings.Repeat("x", 101)} {
		if w := request("GET", "/repositories?"+query, "", "repository-detail"); w.Code != 400 {
			t.Fatal("invalid selection accepted", query, w.Code)
		}
	}
	w = request("POST", base+"/policy", "csrf_token="+token+"&mode=automatic&workspace_query="+url.QueryEscape("q=owner&access=all&page=1&pr=7&pr_q=Improve"), "repository-detail")
	if w.Code != 200 || strings.Contains(w.Body.String(), "<!doctype") || strings.Contains(w.Body.String(), `role="switch"`) || !strings.Contains(w.Header().Get("HX-Push-Url"), "/pull-requests/7") {
		t.Fatal("inline policy response", w.Code, w.Body.String())
	}
	if list := request("GET", workspace, ""); list.Code != 200 || !strings.Contains(list.Body.String(), `role="switch" aria-checked="true"`) || !strings.Contains(list.Body.String(), "Enable Automatic Builds") {
		t.Fatal("PR list policy controls", list.Code)
	}
	if detail := request("GET", base+"/pull-requests/7", ""); detail.Code != 200 || strings.Contains(detail.Body.String(), `id="build-policy"`) || strings.Contains(detail.Body.String(), "Sync Pull Requests") {
		t.Fatal("PR detail included list-only controls", detail.Code)
	}
	w = request("POST", base+"/refresh", "csrf_token="+token, "repository-detail")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "every 3s") || strings.Contains(w.Body.String(), "<!doctype") {
		t.Fatal("inline refresh response", w.Code)
	}
	if w := request("GET", base+"/pull-requests/7", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Not requested") {
		t.Fatal("PR detail", w.Code)
	}
	if f.count(t, "SELECT count(*) FROM build_intents") != 0 {
		t.Fatal("disabled build submitted intent")
	}
	for _, status := range []string{"conflicts", "checking"} {
		if _, err := f.pool.Exec(ctx, "UPDATE pull_requests SET build_status=$1", status); err != nil {
			t.Fatal(err)
		}
		w := request("POST", base+"/pull-requests/7/build", "csrf_token="+token)
		reason := "resolve merge conflicts"
		if status == "checking" {
			reason = "checking mergeability"
		}
		if w.Code != 409 || !strings.Contains(w.Body.String(), reason) {
			t.Fatal("build refusal not surfaced", status, w.Code, w.Body.String())
		}
		detail := request("GET", base+"/pull-requests/7", "")
		if detail.Code != 200 || !strings.Contains(detail.Body.String(), reason) || !strings.Contains(detail.Body.String(), pr.DisplayStatus("open", status)) && !strings.Contains(detail.Body.String(), strings.ToUpper(status[:1])+status[1:]) {
			t.Fatal("dashboard mergeability feedback", status, detail.Body.String())
		}
	}
	if _, err := f.pool.Exec(ctx, "UPDATE installations SET status='suspended'"); err != nil {
		t.Fatal(err)
	}
	if w := request("GET", workspace, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "Repository unavailable") {
		t.Fatal("unavailable page")
	}
	if w := request("GET", base+"/pull-requests/7", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Repository unavailable") {
		t.Fatal("unavailable detail", w.Code)
	}
	if w := request("POST", base+"/policy", "csrf_token="+token+"&mode=manual"); w.Code != 404 {
		t.Fatal("unavailable policy mutation")
	}
	if w := request("POST", base+"/refresh", "csrf_token="+token); w.Code != 404 {
		t.Fatal("unavailable refresh")
	}
	if w := request("POST", base+"/pull-requests/7/build", "csrf_token="+token); w.Code != 404 {
		t.Fatal("unavailable build boundary", w.Code)
	}

}
