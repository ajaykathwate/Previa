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
	prpages "github.com/ajaykathwatee/previa/web/pages/pullrequests"
	repositorypages "github.com/ajaykathwatee/previa/web/pages/repositories"
	"github.com/alexedwards/scs/v2/memstore"
	"github.com/go-chi/chi/v5"
	"strconv"
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
		for _, path := range []string{"/builds", "/environments"} {
			r.With(auth.RequireUser).Get(path, func(w http.ResponseWriter, r *http.Request) {
				templ.Handler(prpages.ActivityIndex("design-review", handler.CSRFToken(r.Context()), prpages.Activity{Environments: r.URL.Path == "/environments", Number: 1, Pages: 1})).ServeHTTP(w, r)
			})
		}
		dashboard.MountPing(r, fixturePings{}, auth.RequireUser, logger)
		// Real workspace templates; database ownership is verified in pullrequests.
		panel := func(r *http.Request, repoID int64) prpages.Page {
			p := prpages.Page{RepoID: repoID, Name: "fixture-owner/repository-" + strconv.FormatInt(repoID, 10), Mode: sessions.GetString(r.Context(), "fixture-mode"), Available: repoID%2 == 1, DiscoveryEnabled: true, SyncState: "complete", Fetched: 25, LastRefresh: "2026-10-04T10:00:00Z", Number: 1, Pages: 3, Total: 25, State: r.URL.Query().Get("pr_state"), Search: r.URL.Query().Get("pr_q"), WorkspaceQuery: r.URL.RawQuery}
			if p.Mode == "" {
				p.Mode = "manual"
			}
			if p.State == "" {
				p.State = "open"
			}
			if ticks := sessions.GetInt(r.Context(), "fixture-sync"); ticks > 0 {
				p.SyncState = "queued"
				sessions.Put(r.Context(), "fixture-sync", ticks-1)
			}
			if n, err := strconv.ParseInt(r.URL.Query().Get("pr_page"), 10, 64); err == nil {
				p.Number = min(max(n, 1), 3)
			}
			if p.State == "open" || p.State == "all" {
				for n := int32(1); n <= 25; n++ {
					title := "A careful change to preview " + strconv.Itoa(int(n))
					if p.Search != "" && !strings.Contains(title, p.Search) {
						continue
					}
					p.Rows = append(p.Rows, prpages.PR{Number: n, Title: title, Author: "design-review", Branch: "feature/preview", SHA: "1234567890abcdef", State: "open", Build: "Not requested"})
				}
			}
			p.Total = int64(len(p.Rows))
			p.Pages = max(int64(1), (p.Total+9)/10)
			p.Number = min(p.Number, p.Pages)
			offset := (p.Number - 1) * 10
			p.Rows = p.Rows[offset:min(offset+10, p.Total)]
			if n, err := strconv.ParseInt(r.URL.Query().Get("pr"), 10, 32); err == nil && n > 0 {
				p.SelectedPR = &prpages.PR{Number: int32(n), Available: p.Available, Title: "A careful change to preview " + strconv.Itoa(int(n)), SHA: "latest-commit", Build: "Not requested", State: "open", Environment: "Not deployed", URL: "https://github.com/fixture-owner/repository/pull/1"}
			}
			return p
		}
		for _, action := range []string{"policy", "refresh"} {
			r.With(auth.RequireUser).Post("/repositories/{repoID}/"+action, func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					http.Error(w, "Invalid form", 400)
					return
				}
				id, _ := strconv.ParseInt(chi.URLParam(r, "repoID"), 10, 64)
				if strings.HasSuffix(r.URL.Path, "/policy") {
					sessions.Put(r.Context(), "fixture-mode", r.Form.Get("mode"))
				} else {
					sessions.Put(r.Context(), "fixture-sync", 2)
				}
				query, _ := url.ParseQuery(r.Form.Get("workspace_query"))
				location := repositorypages.WorkspaceURL(id, query)
				if r.Header.Get("HX-Request") != "true" {
					http.Redirect(w, r, location, http.StatusSeeOther)
					return
				}
				r.URL.RawQuery = query.Encode()
				w.Header().Set("HX-Push-Url", location)
				templ.Handler(prpages.Panel(panel(r, id), handler.CSRFToken(r.Context()))).ServeHTTP(w, r)
			})
		}
		for _, path := range []string{"/repositories/{repoID}", "/repositories/{repoID}/pull-requests/{number}"} {
			r.With(auth.RequireUser).Get(path, func(w http.ResponseWriter, r *http.Request) {
				id, _ := strconv.ParseInt(chi.URLParam(r, "repoID"), 10, 64)
				q := r.URL.Query()
				if n := chi.URLParam(r, "number"); n != "" {
					q.Set("pr", n)
				} else {
					q.Del("pr")
				}
				r.URL.RawQuery = q.Encode()
				p := panel(r, id)
				if r.Header.Get("HX-Request") == "true" {
					if r.Header.Get("HX-Target") == "pull-request-results" {
						templ.Handler(prpages.Results(p)).ServeHTTP(w, r)
						return
					}
					if r.Header.Get("HX-Target") == "repository-detail" {
						templ.Handler(prpages.Panel(p, handler.CSRFToken(r.Context()))).ServeHTTP(w, r)
						return
					}
				}
				templ.Handler(prpages.Index("design-review", handler.CSRFToken(r.Context()), p)).ServeHTTP(w, r)
			})
		}
		// Test-only paged repository views. Database pagination is verified separately.
		r.With(auth.RequireUser).Get("/repositories", func(w http.ResponseWriter, r *http.Request) {
			number, _ := strconv.ParseInt(r.URL.Query().Get("page"), 10, 64)
			if number < 1 {
				number = 1
			}
			selected, _ := strconv.ParseInt(r.URL.Query().Get("repo_id"), 10, 64)
			if selected > 0 {
				http.Redirect(w, r, repositorypages.WorkspaceURL(selected, r.URL.Query()), http.StatusSeeOther)
				return
			}
			data := repositorypages.Page{Query: r.URL.RawQuery, Installations: 2, Enabled: true, Search: r.URL.Query().Get("q"), Access: r.URL.Query().Get("access")}
			if data.Access == "" {
				data.Access = "available"
			}
			var rows []repositorypages.Row
			for i := int64(1); i <= 150; i++ {
				row := repositorypages.Row{ID: i, FullName: "fixture-owner/a-long-repository-name-for-responsive-layout-verification-" + strconv.FormatInt(i, 10), Account: "fixture-owner", InstallationID: 99, Status: "active", Accessible: i%2 == 1}
				if !strings.Contains(strings.ToLower(row.FullName), strings.ToLower(data.Search)) {
					continue
				}
				if data.Access == "available" && !row.Accessible || data.Access == "unavailable" && row.Accessible {
					continue
				}
				rows = append(rows, row)
			}
			data.Total = int64(len(rows))
			data.Pages = max(int64(1), (data.Total+9)/10)
			data.Number = min(number, data.Pages)
			offset := (data.Number - 1) * 10
			if data.Total > 0 {
				data.Start = offset + 1
				data.End = min(offset+10, data.Total)
				data.Rows = rows[offset:data.End]
			}

			if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Target") == "repository-results" {
				templ.Handler(repositorypages.Results(data)).ServeHTTP(w, r)
				return
			}
			templ.Handler(repositorypages.Index("design-review", handler.CSRFToken(r.Context()), data)).ServeHTTP(w, r)
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
