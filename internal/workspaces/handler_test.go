package workspaces

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/ajaykathwatee/previa/internal/auth"
	"github.com/ajaykathwatee/previa/internal/buildlogs"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/scope"
	"github.com/alexedwards/scs/v2/memstore"
	"github.com/go-chi/chi/v5"
)

type testIdentity struct{}

func (testIdentity) AuthCodeURL(state string) string {
	return "/test-github?state=" + url.QueryEscape(state)
}
func (testIdentity) Authenticate(_ context.Context, code string) (auth.Identity, error) {
	for _, v := range []auth.Identity{{ID: 101, Username: "owner"}, {ID: 102, Username: "admin"}, {ID: 103, Username: "dev"}, {ID: 104, Username: "reviewer"}, {ID: 105, Username: "outsider"}, {ID: 106, Username: "newbie"}} {
		if v.Username == code {
			return v, nil
		}
	}
	return auth.Identity{}, fmt.Errorf("unknown fixture identity")
}

type testBrowser struct {
	router  http.Handler
	cookies map[string]*http.Cookie
}

func (b *testBrowser) request(method, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range b.cookies {
		req.AddCookie(c)
	}
	res := httptest.NewRecorder()
	b.router.ServeHTTP(res, req)
	for _, c := range res.Result().Cookies() {
		b.cookies[c.Name] = c
	}
	return res
}
func (b *testBrowser) login(t *testing.T, login string) {
	t.Helper()
	res := b.request(http.MethodGet, "/auth/github/login", nil)
	redirect, err := url.Parse(res.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	res = b.request(http.MethodGet, "/auth/github/callback?"+url.Values{"state": {redirect.Query().Get("state")}, "code": {login}}.Encode(), nil)
	if res.Code != http.StatusSeeOther {
		t.Fatalf("login failed: %d %s", res.Code, res.Body.String())
	}
}
func (b *testBrowser) csrf(t *testing.T) string {
	t.Helper()
	res := b.request(http.MethodGet, "/workspaces", nil)
	match := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`).FindStringSubmatch(res.Body.String())
	if len(match) != 2 {
		t.Fatalf("missing CSRF token: %s", res.Body.String())
	}
	return match[1]
}
func fixtureRouter(t *testing.T, f fixture, origins ...string) http.Handler {
	origin := "http://localhost"
	if len(origins) > 0 {
		origin = origins[0]
	}
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := memstore.NewWithCleanupInterval(0)
	t.Cleanup(store.StopCleanup)
	sessions := auth.NewSessionManager(store, false, logger)
	users := auth.NewHandler(config.ServerConfig{AppBaseURL: origin, SessionSecret: strings.Repeat("s", 32)}, sessions, auth.NewRepository(f.pool), testIdentity{}, logger)
	router := chi.NewRouter()
	router.Use(sessions.LoadAndSave, users.LoadUser, users.CSRF, NewMiddleware(f.service, users, logger).Load)
	users.Mount(router)
	NewHandler(f.service, users, logger, origin, "preview.test").Mount(router, auth.RequireUser)
	buildlogs.NewHandler(buildlogs.NewStore(f.pool), users, logger).Mount(router, auth.RequireUser)
	router.With(auth.RequireUser).Get("/repositories/{repoID}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	router.With(auth.RequireUser).Post("/repositories/{repoID}/policy", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	router.With(auth.RequireUser).Get("/dashboard", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("/workspaces/%d/members", scope.WorkspaceID(r.Context())), http.StatusSeeOther)
	})
	return router
}
func TestWorkspaceRoutesRespectRoleScopeAndCSRF(t *testing.T) {
	f := newFixture(t)
	f.invite(t, "admin", "admin", f.admin, 102, nil)
	f.invite(t, "dev", "developer", f.dev, 103, []int64{f.repo})
	f.invite(t, "reviewer", "reviewer", f.reviewer, 104, []int64{f.repo})
	router := fixtureRouter(t, f)
	repoURL := fmt.Sprintf("/repositories/%d", f.repo)
	hiddenURL := fmt.Sprintf("/repositories/%d", f.otherRepo)
	for _, tt := range []struct {
		login                string
		policy, hidden, logs int
	}{{"owner", 204, 200, 200}, {"admin", 204, 200, 200}, {"dev", 404, 404, 200}, {"reviewer", 404, 404, 303}, {"outsider", 404, 404, 404}} {
		t.Run(tt.login, func(t *testing.T) {
			b := testBrowser{router: router, cookies: map[string]*http.Cookie{}}
			b.login(t, tt.login)
			csrf := b.csrf(t)
			if r := b.request(http.MethodPost, repoURL+"/policy", url.Values{"csrf_token": {csrf}}); r.Code != tt.policy {
				t.Fatalf("policy: %d %s", r.Code, r.Body.String())
			}
			if r := b.request(http.MethodGet, hiddenURL, nil); r.Code != tt.hidden {
				t.Fatalf("hidden repository: %d", r.Code)
			}
			if r := b.request(http.MethodGet, repoURL+"/pull-requests/1/logs", nil); r.Code != tt.logs {
				t.Fatalf("logs: %d %s", r.Code, r.Body.String())
			}
			if r := b.request(http.MethodPost, "/workspaces", url.Values{"name": {"Forged"}}); r.Code != http.StatusForbidden {
				t.Fatal("missing CSRF accepted", r.Code)
			}
		})
	}
}
func TestInvitationRouteShowsExactScopeAndJoinsWithoutJavaScript(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	w, _ := f.service.Read(ctx, f.owner, f.wid)
	token, err := f.service.Invite(ctx, InvitationCommand{ActorID: f.owner, WorkspaceID: f.wid, Revision: w.Revision, Login: "reviewer", Role: "reviewer", Repositories: []int64{f.repo}})
	if err != nil {
		t.Fatal(err)
	}
	b := testBrowser{router: fixtureRouter(t, f), cookies: map[string]*http.Cookie{}}
	path := "/workspace-invitations/" + token
	if r := b.request(http.MethodGet, path, nil); r.Code != 303 || !strings.Contains(r.Header().Get("Location"), "next=") {
		t.Fatal("invitation not preserved through login", r.Code)
	}
	b.login(t, "reviewer")
	r := b.request(http.MethodGet, path, nil)
	if r.Code != 200 || !strings.Contains(r.Body.String(), "acme/api") || strings.Contains(r.Body.String(), "acme/private") || r.Header().Get("Cache-Control") != "no-store" || r.Header().Get("Referrer-Policy") != "same-origin" {
		t.Fatalf("consent scope: %d %s", r.Code, r.Body.String())
	}
	if r := b.request(http.MethodPost, path, url.Values{"csrf_token": {b.csrf(t)}, "action": {"accept"}}); r.Code != 303 {
		t.Fatal("accept failed", r.Code, r.Body.String())
	}
	r = b.request(http.MethodGet, fmt.Sprintf("/workspaces/%d/previews", f.wid), nil)
	if r.Code != 200 || !strings.Contains(r.Body.String(), "acme/api") || strings.Contains(r.Body.String(), "acme/private") {
		t.Fatalf("joined scope: %d %s", r.Code, r.Body.String())
	}
}
