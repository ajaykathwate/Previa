package accounts

import (
	"context"
	"fmt"
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
	"github.com/ajaykathwatee/previa/internal/dashboard"
	"github.com/ajaykathwatee/previa/internal/earlyaccess"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/httpx"
	"github.com/ajaykathwatee/previa/internal/platform/publicid"
	"github.com/ajaykathwatee/previa/internal/support"
	"github.com/ajaykathwatee/previa/internal/workspaces"
	"github.com/ajaykathwatee/previa/web"
	"github.com/alexedwards/scs/v2/memstore"
	"github.com/go-chi/chi/v5"
)

type browserIdentity struct{}

func (browserIdentity) AuthCodeURL(state string) string {
	return "/test-github?state=" + url.QueryEscape(state)
}
func (browserIdentity) Authenticate(_ context.Context, code string) (auth.Identity, error) {
	if code == "member" {
		return auth.Identity{ID: 102, Username: "member"}, nil
	}
	return auth.Identity{ID: 101, Username: "owner"}, nil
}
func TestAccountsBrowser(t *testing.T) {
	if os.Getenv("RUN_ACCOUNTS_BROWSER_TESTS") != "1" {
		t.Skip("run make test-accounts-browser")
	}
	f := newFixture(t)
	for i, name := range []string{"js", "native"} {
		f.exec(t, `INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES($1,$2,$3)`, f.installation, 401+i, "acme/"+name)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	root := httpx.NewRouter(logger)
	root.Handle("/static/*", web.StaticHandler())
	root.Get("/test-github", func(w http.ResponseWriter, r *http.Request) {
		code := "owner"
		if c, err := r.Cookie("fixture_login"); err == nil && c.Value == "member" {
			code = "member"
		}
		http.Redirect(w, r, "/auth/github/callback?"+url.Values{"state": {r.URL.Query().Get("state")}, "code": {code}}.Encode(), http.StatusSeeOther)
	})
	root.Post("/__fixture/cleanup", func(w http.ResponseWriter, r *http.Request) {
		j := f.journal(t)
		if err := f.s.AcknowledgeJournal(r.Context(), j); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		for range 4 {
			if _, err := f.pool.Exec(r.Context(), `UPDATE lifecycle_operations SET available_at=clock_timestamp() WHERE stage<>'complete'`); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			if err := f.s.Sweep(r.Context()); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
		}
		w.WriteHeader(204)
	})
	server := httptest.NewUnstartedServer(root)
	origin := "http://" + server.Listener.Addr().String()
	store := memstore.NewWithCleanupInterval(0)
	defer store.StopCleanup()
	sessions := auth.NewSessionManager(store, false, logger)
	users := auth.NewHandler(config.ServerConfig{AppBaseURL: origin, SessionSecret: strings.Repeat("s", 32)}, sessions, auth.NewRepository(f.pool), browserIdentity{}, logger)
	team := workspaces.NewService(workspaces.NewRepository(f.pool), nil, nil, nil)
	users.WithAvatarAudience(team).WithActivity(team)
	catalog := publicid.New(f.pool)
	r := chi.NewRouter()
	r.Use(sessions.LoadAndSave, users.LoadUser, users.CSRF, catalog.Middleware, workspaces.NewMiddleware(team, users, logger).Load)
	admission := earlyaccess.NewHandler(earlyaccess.NewService(f.pool, true, []int64{101}), users, logger)
	r.Use(admission.Guard)
	admission.Mount(r, auth.RequireUser)
	users.Mount(r)
	dashboard.Mount(r, users, auth.RequireUser)
	workspaces.NewHandler(team, users, logger, origin).Mount(r, auth.RequireUser)
	NewHandler(f.s, users, logger, sessions.Destroy, false).Mount(r, auth.RequireUser)
	support.NewHandler(support.NewService(f.pool, support.Settings{Email: "support@example.test", Operators: []int64{101}}), users, logger).Mount(r, auth.RequireUser)
	root.Mount("/", r)
	server.Start()
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "--test", "accounts.test.cjs")
	cmd.Dir = "../../web/tests/browser"
	cmd.Env = append(os.Environ(), "ACCOUNTS_BROWSER_URL="+server.URL)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(fmt.Errorf("accounts browser tests: %w\n%s", err, out))
	}
	t.Log(string(out))
}
