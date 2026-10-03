package browser_test

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

	"github.com/a-h/templ"
	"github.com/ajaykathwatee/previa/internal/auth"
	"github.com/ajaykathwatee/previa/internal/dashboard"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/web"
	dashboardpages "github.com/ajaykathwatee/previa/web/pages/dashboard"
	"github.com/alexedwards/scs/v2/memstore"
	"github.com/go-chi/chi/v5"
)

// This opt-in suite exercises the real UI, session, and CSRF middleware without
// a database or external GitHub calls. It exposes no fixture routes in the app.
func TestBrowserRegression(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TESTS") != "1" {
		t.Skip("run make test-browser to enable browser regressions")
	}
	router := chi.NewRouter()
	server := httptest.NewUnstartedServer(router)
	origin := "http://" + server.Listener.Addr().String()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := memstore.New()
	t.Cleanup(store.StopCleanup)
	sessions := auth.NewSessionManager(store, false, logger)
	handler := auth.NewHandler(config.ServerConfig{AppBaseURL: origin, SessionSecret: strings.Repeat("fixture-only-", 4)}, sessions, fixtureUsers{}, fixtureProvider{origin: origin}, logger)
	router.Handle("/static/*", web.StaticHandler())
	router.Group(func(r chi.Router) {
		r.Use(sessions.LoadAndSave, handler.LoadUser, handler.CSRF)
		handler.Mount(r)
		dashboard.Mount(r, handler, auth.RequireUser)
		dashboard.MountPing(r, fixturePings{}, auth.RequireUser, logger)
		// Test-only view of the configured branch; never mounted by internal/app.
		r.With(auth.RequireUser).Get("/fixture-installations", func(w http.ResponseWriter, r *http.Request) {
			templ.Handler(dashboardpages.Index("design-review", handler.CSRFToken(r.Context()), true, []dashboardpages.Installation{
				{ID: 99, Account: "fixture-owner", Status: "active", Repositories: []dashboardpages.Repository{
					{FullName: "fixture-owner/a-long-repository-name-for-responsive-layout-verification", Accessible: true},
					{FullName: "fixture-owner/removed"},
				}},
			})).ServeHTTP(w, r)
		})
	})
	server.Start()
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "node", "--test", "ui.test.cjs")
	command.Env = append(os.Environ(), "BROWSER_BASE_URL="+origin)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("browser regression checks: %v\n%s", err, output)
	}
	t.Log(string(output))
}

type fixtureUsers struct{}

func (fixtureUsers) UpsertUser(context.Context, auth.Identity) (auth.User, error) {
	return auth.User{ID: 1, GitHubID: 42, Username: "design-review"}, nil
}
func (fixtureUsers) GetUser(context.Context, int64) (auth.User, error) {
	return auth.User{ID: 1, GitHubID: 42, Username: "design-review"}, nil
}

type fixtureProvider struct{ origin string }

func (p fixtureProvider) AuthCodeURL(state string) string {
	return p.origin + "/auth/github/callback?code=fixture&state=" + url.QueryEscape(state)
}
func (fixtureProvider) Authenticate(_ context.Context, code string) (auth.Identity, error) {
	if code != "fixture" {
		return auth.Identity{}, errors.New("unexpected fixture authorization code")
	}
	return auth.Identity{ID: 42, Username: "design-review"}, nil
}

type fixturePings struct{}

func (fixturePings) EnqueuePing(context.Context, string) (int64, bool, error) { return 17, false, nil }
