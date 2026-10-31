package usage

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/auth"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/httpx"
	"github.com/ajaykathwatee/previa/internal/platform/publicid"
	"github.com/ajaykathwatee/previa/internal/workspaces"
	"github.com/ajaykathwatee/previa/web"
	"github.com/alexedwards/scs/v2/memstore"
	"github.com/go-chi/chi/v5"
)

type fixtureIdentity struct{ ids map[string]int64 }

func (fixtureIdentity) AuthCodeURL(state string) string {
	return "/test-github?state=" + url.QueryEscape(state)
}
func (f fixtureIdentity) Authenticate(_ context.Context, code string) (auth.Identity, error) {
	if id, ok := f.ids[code]; ok {
		return auth.Identity{ID: id, Username: code}, nil
	}
	return auth.Identity{}, errors.New("unknown fixture identity")
}

func TestUsageBrowser(t *testing.T) {
	if os.Getenv("RUN_USAGE_BROWSER_TESTS") != "1" {
		t.Skip("run make test-usage-browser with TEST_DATABASE_URL")
	}
	f := newFixture(t)
	f.now = time.Now().UTC()
	f.exec(t, `UPDATE workspace_entitlements SET included_seconds=18000,concurrent_builds=1,preview_slots=2`)
	f.store.WithPreviewPolicy(48*time.Hour, 30*time.Minute, 14*24*time.Hour)
	if err := f.store.Configure(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.member(t, "developer", true)
	f.member(t, "reviewer", true)
	pr := f.pr(t, 1)
	key := f.reserve(t, pr)
	f.start(t, key)
	f.now = f.now.Add(42 * time.Second)
	f.finish(t, key, "failed")
	f.exec(t, `UPDATE build_intents SET waiting_revision=revision,waiting_reason='workspace_minutes',usage_wait_until=now()+interval '1 day',waiting_since=now(),waiting_checked_at=now() WHERE pull_request_id=$1`, pr)
	f.exec(t, `INSERT INTO repos(installation_id,github_repo_id,full_name) SELECT installation_id,200,'acme/private' FROM repos WHERE id=$1`, f.repo)
	ids := map[string]int64{}
	rows, err := f.pool.Query(t.Context(), `SELECT username,github_id FROM users`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		var id int64
		if err = rows.Scan(&name, &id); err != nil {
			t.Fatal(err)
		}
		ids[name] = id
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	root := httpx.NewRouter(logger)
	root.Handle("/static/*", web.StaticHandler())
	root.Get("/__fixture/identity", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "fixture_identity", Value: r.URL.Query().Get("login"), Path: "/", HttpOnly: true})
		w.WriteHeader(http.StatusNoContent)
	})
	root.Get("/test-github", func(w http.ResponseWriter, r *http.Request) {
		login := "owner"
		if c, err := r.Cookie("fixture_identity"); err == nil {
			login = c.Value
		}
		http.Redirect(w, r, "/auth/github/callback?"+url.Values{"state": {r.URL.Query().Get("state")}, "code": {login}}.Encode(), http.StatusSeeOther)
	})
	server := httptest.NewUnstartedServer(root)
	origin := "http://" + server.Listener.Addr().String()
	sessionStore := memstore.NewWithCleanupInterval(0)
	t.Cleanup(sessionStore.StopCleanup)
	sessions := auth.NewSessionManager(sessionStore, false, logger)
	users := auth.NewHandler(config.ServerConfig{AppBaseURL: origin, SessionSecret: strings.Repeat("s", 32)}, sessions, auth.NewRepository(f.pool), fixtureIdentity{ids: ids}, logger)
	team := workspaces.NewService(workspaces.NewRepository(f.pool), nil, nil, nil)
	users.WithAvatarAudience(team).WithActivity(team)
	catalog := publicid.New(f.pool)
	r := chi.NewRouter()
	r.Use(sessions.LoadAndSave, users.LoadUser, users.CSRF, catalog.Middleware, workspaces.NewMiddleware(team, users, logger).Load)
	users.Mount(r)
	workspaces.NewHandler(team, users, logger, origin).Mount(r, auth.RequireUser)
	NewHandler(f.store, users, logger).Mount(r, auth.RequireUser)
	root.Mount("/", r)
	server.Start()
	defer server.Close()
	id, err := catalog.Public(t.Context(), "workspace", f.workspace, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "--test", "usage.test.cjs")
	cmd.Dir = "../../web/tests/browser"
	cmd.Env = append(os.Environ(), "USAGE_BROWSER_URL="+server.URL, "USAGE_PATH=/workspaces/"+id+"/usage")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("usage browser checks: %v\n%s", err, output)
	}
	t.Log(string(output))
}
