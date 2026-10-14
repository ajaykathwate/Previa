package workspaces

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
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/httpx"
	"github.com/ajaykathwatee/previa/web"
)

// The team UI uses real PostgreSQL, session/CSRF middleware and feature routes;
// only external GitHub identity/permission responses are fixtures.
func TestWorkspaceBrowser(t *testing.T) {
	if os.Getenv("RUN_WORKSPACE_BROWSER_TESTS") != "1" {
		t.Skip("run make test-workspace-browser with TEST_DATABASE_URL")
	}
	f := newFixture(t)
	f.invite(t, "admin", "admin", f.admin, 102, nil)
	f.invite(t, "dev", "developer", f.dev, 103, []int64{f.repo})
	f.invite(t, "reviewer", "reviewer", f.reviewer, 104, []int64{f.repo})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := httpx.NewRouter(logger)
	router.Handle("/static/*", web.StaticHandler())
	router.Get("/__fixture/identity", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "fixture_identity", Value: r.URL.Query().Get("login"), Path: "/", HttpOnly: true})
		w.WriteHeader(http.StatusNoContent)
	})
	router.Get("/test-github", func(w http.ResponseWriter, r *http.Request) {
		login := "owner"
		if c, err := r.Cookie("fixture_identity"); err == nil {
			login = c.Value
		}
		http.Redirect(w, r, "/auth/github/callback?"+url.Values{"state": {r.URL.Query().Get("state")}, "code": {login}}.Encode(), http.StatusSeeOther)
	})
	server := httptest.NewUnstartedServer(router)
	origin := "http://" + server.Listener.Addr().String()
	router.Mount("/", fixtureRouter(t, f, origin))
	server.Start()
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "--test", "workspaces.test.cjs")
	cmd.Dir = "../../web/tests/browser"
	cmd.Env = append(os.Environ(), "WORKSPACE_BROWSER_URL="+server.URL, fmt.Sprintf("WORKSPACE_TEST_ID=%d", f.wid), fmt.Sprintf("WORKSPACE_DEV_ID=%d", f.dev))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("workspace browser checks: %v\n%s", err, output)
	}
	t.Log(string(output))
}
