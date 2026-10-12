package browser_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a-h/templ"
	"github.com/ajaykathwatee/previa/internal/auth"
	"github.com/ajaykathwatee/previa/internal/buildlogs"
	"github.com/ajaykathwatee/previa/internal/dashboard"
	"github.com/ajaykathwatee/previa/internal/outbound"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/egress"
	"github.com/ajaykathwatee/previa/internal/platform/httpx"
	"github.com/ajaykathwatee/previa/internal/platform/realtime"
	pr "github.com/ajaykathwatee/previa/internal/pullrequests"
	"github.com/ajaykathwatee/previa/internal/repoconfig"
	"github.com/ajaykathwatee/previa/internal/routing"
	"github.com/ajaykathwatee/previa/internal/secrets"
	"github.com/ajaykathwatee/previa/internal/starterkits"
	"github.com/ajaykathwatee/previa/web"
	dashboardpages "github.com/ajaykathwatee/previa/web/pages/dashboard"
	prpages "github.com/ajaykathwatee/previa/web/pages/pullrequests"
	repositorypages "github.com/ajaykathwatee/previa/web/pages/repositories"
	"github.com/alexedwards/scs/v2"
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// The production router's middleware, so every scenario runs under the
	// real Content-Security-Policy (violations fail the test).
	router := httpx.NewRouter(logger)
	server := httptest.NewUnstartedServer(router)
	origin := "http://" + server.Listener.Addr().String()
	store := memstore.New()
	t.Cleanup(store.StopCleanup)
	sessions := auth.NewSessionManager(store, false, logger)
	handler := auth.NewHandler(config.ServerConfig{AppBaseURL: origin, SessionSecret: strings.Repeat("fixture-only-", 4)}, sessions, fixtureUsers{}, fixtureProvider{origin: origin}, logger)
	router.Handle("/static/*", web.StaticHandler())
	// The router's destination for unknown preview hosts: public, no session.
	router.Get(routing.UnavailablePath, routing.UnavailablePage)
	// Live updates use the real hub and SSE stream. Fixture sync progress advances
	// on a server-side timeline and publishes each step, like the database triggers.
	hub := realtime.NewHub()
	var syncMu sync.Mutex
	syncing := map[int64]int{}
	startSync := func(id int64) {
		syncMu.Lock()
		syncing[id] = 2
		syncMu.Unlock()
		hub.Publish(pr.RepositoryTopic(id), "pr_syncs")
		go func() {
			for left := 2; left > 0; {
				time.Sleep(700 * time.Millisecond)
				syncMu.Lock()
				syncing[id]--
				left = syncing[id]
				syncMu.Unlock()
				hub.Publish(pr.RepositoryTopic(id), "pr_syncs")
			}
		}()
	}
	// Real build log handler and live stream over an in-memory store. Test-only
	// routes change the logs and publish like the database trigger does.
	logs := &fixtureLogs{}
	logs.seed()
	publishLogs := func() { hub.Publish(buildlogs.Topic(fixtureLogEnvironment), "build_logs") }
	router.Post("/__fixture/logs/seed", func(w http.ResponseWriter, _ *http.Request) {
		logs.seed()
		publishLogs()
		w.WriteHeader(http.StatusNoContent)
	})
	router.Post("/__fixture/logs/append", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("system") == "1" {
			logs.addSystem(r.FormValue("text"))
		} else {
			logs.add(r.FormValue("text"))
		}
		publishLogs()
		w.WriteHeader(http.StatusNoContent)
	})
	router.Post("/__fixture/logs/restart", func(w http.ResponseWriter, r *http.Request) {
		logs.restart(r.FormValue("text"))
		publishLogs()
		w.WriteHeader(http.StatusNoContent)
	})
	router.Group(func(r chi.Router) {
		r.Use(sessions.LoadAndSave, handler.LoadUser, handler.CSRF)
		handler.Mount(r)
		buildlogs.NewHandler(logs, handler, logger).
			WithLiveUpdates(hub, realtime.StreamOptions{Retry: 500 * time.Millisecond, Heartbeat: 10 * time.Second, MaxDuration: time.Minute}).
			Mount(r, auth.RequireUser)
		// The Overview shows an empty workspace until a test asks for a busy one
		// (GET /__fixture/overview?state=busy), kept in the session.
		// Starter kits with Previa's default service versions and no worker.
		starterkits.NewHandler(handler, nil, starterkits.Offer{Postgres: []string{"17", "16"}, Redis: []string{"8", "7"}, MySQL: []string{"8.4"}}, fixtureKitCheck, logger).
			Mount(r, auth.RequireUser)
		dashboard.MountWithSummary(r, handler, auth.RequireUser, dashboard.Options{
			DeveloperTools: true, Identity: handler.UserIDs,
			Summary: func(ctx context.Context, _ int64) (dashboardpages.Summary, error) {
				if sessions.GetString(ctx, "fixture-overview") != "busy" {
					return dashboardpages.Summary{}, nil
				}
				return fixtureOverview(time.Now()), nil
			},
		})
		r.With(auth.RequireUser).Get("/__fixture/overview", func(w http.ResponseWriter, r *http.Request) {
			sessions.Put(r.Context(), "fixture-overview", r.URL.Query().Get("state"))
			http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		})
		r.With(auth.RequireUser).Get("/environments", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/previews?"+r.URL.RawQuery, http.StatusMovedPermanently)
		})
		for _, path := range []string{"/builds", "/previews"} {
			r.With(auth.RequireUser).Get(path, func(w http.ResponseWriter, r *http.Request) {
				activity := prpages.Activity{Previews: r.URL.Path == "/previews", Number: 1, Pages: 1, Failures: r.URL.Query().Get("failures"), AttentionDays: 7}
				if d := r.URL.Query().Get("dismissed"); d == "1-4" {
					activity.DismissedRepo, activity.DismissedNumber = 1, 4
				}
				if r.URL.Query().Get("fixture") == "rows" {
					activity.Total = 3
					activity.Rows = []prpages.ActivityRow{
						{RepoID: 1, Number: 3, Name: "fixture-owner/repository-1", Latest: "latest-commit", Requested: "latest-commit", Deployed: "older-commit", State: "running", Available: true,
							PreviewURL: "https://pr-3-1-fixture.preview.example.com", ExpiresAt: "2026-10-07T10:00:00Z", OlderPreview: true, Teardown: true, Waiting: "Waiting for capacity"},
						{RepoID: 1, Number: 4, Name: "fixture-owner/repository-1", Latest: "latest-commit", Requested: "latest-commit", State: "failed", Available: true, Failure: "The app did not become ready.", FailureCode: "unhealthy",
							Revision: 2, Attention: "attention"},
						{RepoID: 1, Number: 5, Name: "fixture-owner/repository-1", Latest: "old-commit", Requested: "old-commit", State: "failed", Available: true, Failure: "The image build failed.", FailureCode: "build_failed",
							Revision: 1, Attention: "older", AttentionNote: "Over 7 days old"},
					}
					if activity.Previews {
						// #3 is live on an earlier commit; #4's newer build failed while
						// its earlier version keeps serving.
						activity.Rows[0].Waiting = ""
						activity.Rows[1].Deployed, activity.Rows[1].Attention, activity.Rows[1].PreviewURL = "old-commit", "", "https://pr-4-1-fixture.preview.example.com"
						activity.Rows = activity.Rows[:2]
						activity.Total = 2
					}
				}
				templ.Handler(prpages.ActivityIndex("design-review", handler.CSRFToken(r.Context()), activity)).ServeHTTP(w, r)
			})
		}
		dashboard.MountPing(r, fixturePings{}, auth.RequireUser, logger)
		// Real secrets handler, keyring and CSRF over an in-memory store.
		fixtureKeys, err := secrets.ParseKeyring("1:"+base64.StdEncoding.EncodeToString(make([]byte, 32)), "1")
		if err != nil {
			t.Fatal(err)
		}
		secretService, err := secrets.NewService(&fixtureSecrets{names: map[int64]map[string]time.Time{}}, fixtureKeys)
		if err != nil {
			t.Fatal(err)
		}
		secrets.NewHandler(secretService, handler, logger).Mount(r, auth.RequireUser)
		// Real outbound handler and rules over an in-memory store.
		outboundService, err := outbound.NewService(&fixtureOutbound{approved: map[int64][]outbound.Approved{}}, []int{5432, 6379})
		if err != nil {
			t.Fatal(err)
		}
		outbound.NewHandler(outboundService, handler, logger).Mount(r, auth.RequireUser)
		// Real workspace templates; database ownership is verified in pullrequests.
		panel := func(r *http.Request, repoID int64) prpages.Page {
			p := prpages.Page{RepoID: repoID, Name: "fixture-owner/repository-" + strconv.FormatInt(repoID, 10), Mode: sessions.GetString(r.Context(), "fixture-mode"), Available: repoID%2 == 1, DiscoveryEnabled: true, SyncState: "complete", Fetched: 25, LastRefresh: "2026-10-04T10:00:00Z", Number: 1, Pages: 3, Total: 25, State: r.URL.Query().Get("pr_state"), Search: r.URL.Query().Get("pr_q"), WorkspaceQuery: r.URL.RawQuery}
			if p.Mode == "" {
				p.Mode = "manual"
			}
			if p.State == "" {
				p.State = "open"
			}
			syncMu.Lock()
			if syncing[repoID] > 0 {
				p.SyncState = "queued"
			}
			syncMu.Unlock()
			if n, err := strconv.ParseInt(r.URL.Query().Get("pr_page"), 10, 64); err == nil {
				p.Number = min(max(n, 1), 3)
			}
			if p.State == "open" || p.State == "all" {
				for n := int32(1); n <= 25; n++ {
					title := "A careful change to preview " + strconv.Itoa(int(n))
					if p.Search != "" && !strings.Contains(title, p.Search) {
						continue
					}
					row := prpages.PR{Number: n, Title: title, Author: "design-review", Branch: "feature/preview", SHA: "1234567890abcdef", State: "open", Build: "Not requested",
						URL: "https://github.com/fixture-owner/repository/pull/" + strconv.Itoa(int(n)), UpdatedAt: "2026-10-04T09:00:00Z", RequestCurrent: true}
					switch n { // the states the detail fixture shows for these PRs
					case 3:
						row.Build, row.PreviewURL, row.OlderPreview = "succeeded", "https://pr-3-1-fixture.preview.example.com", true
					case 4:
						row.Build = "failed"
					case 6:
						row.Build = "running"
					}
					p.Rows = append(p.Rows, row)
				}
			}
			p.Total = int64(len(p.Rows))
			p.Pages = max(int64(1), (p.Total+9)/10)
			p.Number = min(p.Number, p.Pages)
			offset := (p.Number - 1) * 10
			p.Rows = p.Rows[offset:min(offset+10, p.Total)]
			if n, err := strconv.ParseInt(r.URL.Query().Get("pr"), 10, 32); err == nil && n > 0 {
				p.SelectedPR = fixturePR(r, sessions, int32(n), p.Available)
			}
			return p
		}
		// Build and teardown act like the real handler: an owner's request
		// changes the PR's state and the panel re-renders in place.
		for _, action := range []string{"build", "teardown"} {
			r.With(auth.RequireUser).Post("/repositories/{repoID}/pull-requests/{number}/"+action, func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					http.Error(w, "Invalid form", 400)
					return
				}
				id, _ := strconv.ParseInt(chi.URLParam(r, "repoID"), 10, 64)
				if id%2 == 0 {
					http.NotFound(w, r)
					return
				}
				key := "fixture-pr-" + chi.URLParam(r, "number")
				if strings.HasSuffix(r.URL.Path, "/build") {
					if r.Form.Get("request_key") == "" {
						http.Error(w, "The pull request or its build changed meanwhile. Reload and try again.", http.StatusConflict)
						return
					}
					sessions.Put(r.Context(), key, "queued")
				} else {
					sessions.Put(r.Context(), key, "removing")
				}
				if r.Form.Get("return") == "previews" {
					http.Redirect(w, r, "/previews?page=1", http.StatusSeeOther)
					return
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
		for _, action := range []string{"policy", "refresh"} {
			r.With(auth.RequireUser).Post("/repositories/{repoID}/"+action, func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					http.Error(w, "Invalid form", 400)
					return
				}
				id, _ := strconv.ParseInt(chi.URLParam(r, "repoID"), 10, 64)
				if strings.HasSuffix(r.URL.Path, "/policy") {
					sessions.Put(r.Context(), "fixture-mode", r.Form.Get("mode"))
					hub.Publish(pr.RepositoryTopic(id), "repos")
				} else {
					startSync(id)
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
				cursor := hub.Cursor()
				p := panel(r, id)
				p.LiveURL = prpages.RepoURL(id) + "/events?since=" + url.QueryEscape(cursor.String())
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
		r.With(auth.RequireUser).Get("/repositories/{repoID}/events", func(w http.ResponseWriter, r *http.Request) {
			id, _ := strconv.ParseInt(chi.URLParam(r, "repoID"), 10, 64)
			sub := hub.Subscribe(pr.RepositoryTopic(id), realtime.RequestCursor(r))
			defer sub.Close()
			_ = realtime.Stream(w, r, sub, realtime.StreamOptions{Retry: 500 * time.Millisecond, Heartbeat: 10 * time.Second, MaxDuration: time.Minute})
		})
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
			limit := int64(150)
			if value, err := strconv.ParseInt(r.URL.Query().Get("fixture_count"), 10, 64); err == nil && value > 0 && value <= 5 {
				limit = value
			}
			for i := int64(1); i <= limit; i++ {
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
	t.Cleanup(hub.Close) // Runs first, ending open event streams so Close returns.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	args := []string{"--test"}
	if pattern := os.Getenv("UI_TEST_PATTERN"); pattern != "" {
		args = append(args, "--test-name-pattern="+pattern) // rerun matching scenarios only
	}
	command := exec.CommandContext(ctx, "node", append(args, "ui.test.cjs")...)
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

// fixtureSecrets keeps names only; sealed values are discarded after encryption.
type fixtureSecrets struct {
	mu    sync.Mutex
	names map[int64]map[string]time.Time
	build map[int64]map[string]bool
}

func (f *fixtureSecrets) Repository(_ context.Context, _ int64, repoID int64) (secrets.RepositoryView, error) {
	if repoID <= 0 || repoID > 150 {
		return secrets.RepositoryView{}, secrets.ErrUnavailable
	}
	return secrets.RepositoryView{ID: repoID, FullName: "fixture-owner/repository-" + strconv.FormatInt(repoID, 10), Available: repoID%2 == 1}, nil
}

func (f *fixtureSecrets) List(ctx context.Context, user, repoID int64) ([]secrets.Summary, error) {
	if _, err := f.Repository(ctx, user, repoID); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []secrets.Summary
	for name, at := range f.names[repoID] {
		out = append(out, secrets.Summary{Name: name, UpdatedAt: at, Build: f.build[repoID][name]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fixtureSecrets) PutMany(ctx context.Context, user, repoID int64, items []secrets.SealedSecret) (int, error) {
	repo, err := f.Repository(ctx, user, repoID)
	if err != nil || !repo.Available {
		return 0, secrets.ErrUnavailable
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.names[repoID] == nil {
		f.names[repoID] = map[string]time.Time{}
	}
	created := 0
	for _, item := range items {
		if len(item.Value) == 0 {
			return 0, errors.New("fixture expected ciphertext")
		}
		if _, exists := f.names[repoID][item.Name]; !exists {
			created++
		}
	}
	if len(f.names[repoID])+created > secrets.MaxSecretsPerRepository {
		return 0, secrets.ErrLimit
	}
	for _, item := range items {
		f.names[repoID][item.Name] = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	}
	return created, nil
}

func (f *fixtureSecrets) DeleteMany(ctx context.Context, user, repoID int64, names []string) (int, error) {
	if _, err := f.Repository(ctx, user, repoID); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	deleted := 0
	for _, name := range names {
		if _, exists := f.names[repoID][name]; exists {
			deleted++
			delete(f.names[repoID], name)
			delete(f.build[repoID], name)
		}
	}
	return deleted, nil
}

func (f *fixtureSecrets) SetBuild(ctx context.Context, user, repoID int64, name string, build bool) (bool, error) {
	repo, err := f.Repository(ctx, user, repoID)
	if err != nil || (build && !repo.Available) {
		return false, secrets.ErrUnavailable
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.names[repoID][name]; !ok {
		return false, nil
	}
	if f.build == nil {
		f.build = map[int64]map[string]bool{}
	}
	if f.build[repoID] == nil {
		f.build[repoID] = map[string]bool{}
	}
	changed := f.build[repoID][name] != build
	f.build[repoID][name] = build
	return changed, nil
}

type fixturePings struct{}

func (fixturePings) EnqueuePing(context.Context, string) (int64, bool, error) { return 17, false, nil }

// fixtureLogs serves the current-build logs of PR #3 in repository 1.
type fixtureLogs struct {
	mu    sync.Mutex
	epoch int64
	lines []buildlogs.Line
}

const fixtureLogEnvironment = 7001

func (f *fixtureLogs) seed() {
	f.mu.Lock()
	f.epoch++
	f.lines = nil
	f.mu.Unlock()
	for i := 1; i <= 517; i++ {
		f.add(fmt.Sprintf("#%d step output line %d", i/40+1, i))
	}
	f.add(`<script>window.__pwned = true</script>`, `<img src=x onerror="window.__pwned = true">`, "\tindented output")
}

func (f *fixtureLogs) add(texts ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, text := range texts {
		f.lines = append(f.lines, buildlogs.Line{Sequence: int64(len(f.lines)), Text: text})
	}
}

// addSystem appends one of the runtime's own progress lines.
func (f *fixtureLogs) addSystem(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines = append(f.lines, buildlogs.Line{Sequence: int64(len(f.lines)), System: true, Text: text})
}

func (f *fixtureLogs) restart(text string) {
	f.mu.Lock()
	f.epoch++
	f.lines = nil
	f.mu.Unlock()
	f.add(text)
}

func (f *fixtureLogs) Target(_ context.Context, userID, repoID int64, number int32) (buildlogs.Target, error) {
	// PR 3's logs, also streamed as running PR 6's so its tracker can follow them.
	if userID <= 0 || repoID != 1 || (number != 3 && number != 6) {
		return buildlogs.Target{}, buildlogs.ErrUnavailable
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return buildlogs.Target{PullRequestID: int64(number), RepoID: 1, Number: number, Title: "A careful change to preview " + strconv.Itoa(int(number)), Repository: "fixture-owner/repository-1", Available: true,
		EnvironmentID: fixtureLogEnvironment, Intent: &buildlogs.Intent{Generation: 1, Revision: f.epoch, HeadSHA: "1234567890abcdef", State: "running"}}, nil
}

func (f *fixtureLogs) Stream(_ context.Context, environmentID int64) (buildlogs.Stream, error) {
	if environmentID != fixtureLogEnvironment {
		return buildlogs.Stream{}, buildlogs.ErrNoStream
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return buildlogs.Stream{EnvironmentID: environmentID, Epoch: f.epoch, Generation: 1, Revision: f.epoch, HeadSHA: "1234567890abcdef", Lines: int64(len(f.lines))}, nil
}

func (f *fixtureLogs) LinesFrom(_ context.Context, _, epoch, from int64, limit int) ([]buildlogs.Line, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if epoch != f.epoch || from >= int64(len(f.lines)) {
		return nil, nil
	}
	return append([]buildlogs.Line(nil), f.lines[from:min(int(from)+limit, len(f.lines))]...), nil
}

// Delete refuses: the fixture's attempt is always running.
func (f *fixtureLogs) Delete(context.Context, int64, int64) error { return buildlogs.ErrNotDeletable }

func (f *fixtureLogs) LinesBefore(_ context.Context, _, epoch, before int64, limit int) ([]buildlogs.Line, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if epoch != f.epoch {
		return nil, nil
	}
	before = min(before, int64(len(f.lines)))
	return append([]buildlogs.Line(nil), f.lines[max(int(before)-limit, 0):before]...), nil
}

// fixturePR is a PR detail in one of the states the real handler derives:
// PR 3 serves an older preview, any other PR has nothing requested; an owner's
// build or teardown (kept in the session) moves it on.
func fixturePR(r *http.Request, sessions *scs.SessionManager, number int32, available bool) *prpages.PR {
	pr := &prpages.PR{Number: number, Available: available, Title: "A careful change to preview " + strconv.Itoa(int(number)), SHA: "latest-commit",
		Build: "Not requested", State: "open", Environment: "Not deployed", URL: "https://github.com/fixture-owner/repository/pull/1",
		Headline: prpages.Headline{Tone: "tone-neutral", Title: "No preview yet", Detail: "Build the latest commit to get a preview address for this pull request."}}
	if !available {
		pr.Actions = prpages.Actions{Unavailable: "GitHub access to this repository is unavailable.",
			Hint: "Restore access on the Repositories page: choose repositories on GitHub and select this one again.", Blocked: "Build latest commit"}
		pr.Headline = prpages.Headline{Tone: "tone-neutral", Title: "GitHub access to this repository is unavailable"}
		return pr
	}
	switch number {
	case 3:
		pr.Build, pr.RequestedSHA, pr.Environment, pr.DeployedSHA = "succeeded", "older-commit", "running", "older-commit"
		pr.PreviewURL, pr.ExpiresAt, pr.OlderPreview = "https://pr-3-1-fixture.preview.example.com", "2026-10-07T10:00:00Z", true
		pr.Actions = prpages.Actions{Build: true, Teardown: true, ExpectedRevision: 1, RequestKey: "fixture-request-key-0003"}
		pr.Headline = prpages.Headline{Tone: "tone-warn", Title: "Preview live on an earlier commit"}
	case 4:
		// The commit could not be downloaded: the page explains it and offers a retry.
		pr.Build, pr.RequestedSHA, pr.RequestCurrent = "failed", "latest-commit", true
		pr.Failure, pr.FailureCode = "The commit could not be prepared.", "source_failed"
		pr.FailureLog = []string{"Preparing commit 1234567 from fixture-owner/repository-1.",
			"Source preparation failed: GitHub denied access to this commit's files. Give the GitHub App Contents: read, then retry."}
		pr.StartedAt, pr.CompletedAt = "2026-10-04T09:58:00Z", "2026-10-04T09:58:04Z"
		pr.Actions = prpages.Actions{Retry: true, RequestKey: "fixture-request-key-0004"}
		pr.Headline = prpages.Headline{Tone: "tone-danger", Title: "Build failed", Detail: pr.Failure}
	case 6:
		// A build is running; the tracker follows its live log.
		pr.Build, pr.RequestedSHA, pr.RequestCurrent = "running", "latest-commit", true
		pr.StartedAt = time.Now().Add(-75 * time.Second).UTC().Format(time.RFC3339)
		pr.Actions = prpages.Actions{Teardown: true, CancelOnly: true}
		pr.Headline = prpages.Headline{Tone: "tone-info", Title: "Building latest-commi", Detail: "The preview address appears here when it is live."}
	case 9:
		// A worker runs but has no preview domain: the action is shown disabled.
		pr.Actions = prpages.Actions{Unavailable: "The running worker cannot build previews: it has no preview domain.",
			Hint: "Set PREVIEW_BASE_DOMAIN on the worker, then restart it.", Blocked: "Build latest commit"}
	default:
		pr.Actions = prpages.Actions{Build: true, RequestKey: "fixture-request-key-" + strconv.Itoa(int(number))}
	}
	switch sessions.GetString(r.Context(), "fixture-pr-"+strconv.Itoa(int(number))) {
	case "queued":
		pr.Build, pr.RequestedSHA, pr.RequestCurrent = "queued", "latest-commit", true
		pr.Failure, pr.FailureCode, pr.FailureLog = "", "", nil // the handler explains only a failed current request
		pr.Actions = prpages.Actions{Teardown: true, CancelOnly: pr.DeployedSHA == ""}
		pr.Headline = prpages.Headline{Tone: "tone-info", Title: "Queued to build latest-commi"}
	case "removing":
		pr.Environment, pr.PreviewURL, pr.ExpiresAt, pr.OlderPreview = "tearing_down", "", "", false
		pr.RemovedNote = "The owner tore this preview down. Build again to bring it back at the same address."
		pr.Actions = prpages.Actions{}
		pr.Headline = prpages.Headline{Tone: "tone-warn", Title: "Removing the preview"}
	}
	return pr
}

// fixtureOverview is a busy workspace for the Overview's browser checks:
// requests waiting, building and failed, previews part-way through their
// lifetime and every worker capability connected.
func fixtureOverview(now time.Time) dashboardpages.Summary {
	at := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339) }
	return dashboardpages.Summary{
		Installations: 2, Repositories: 6, Available: 5, OpenPRs: 12, Waiting: 2, Building: 1, Failed: 1, FailedOlder: 2, Live: 4, AttentionDays: 7,
		Workers: dashboardpages.Workers{Discovery: true, Execution: true, Reporting: true, Housekeeping: true},
		Builds: []prpages.ActivityRow{
			{RepoID: 1, Number: 3, Name: "fixture-owner/repository-1", Title: "Fixture pull request 3", Latest: "latest-commit", Requested: "latest-commit",
				State: "running", RequestCurrent: true, RequestSource: "manual", Available: true, Deadline: at(-3 * time.Minute), StartedAt: at(-2 * time.Minute)},
			{RepoID: 1, Number: 4, Name: "fixture-owner/repository-1", Title: "Fixture pull request 4", Latest: "latest-commit", Requested: "latest-commit",
				State: "failed", Failure: "The app did not become ready.", FailureCode: "unhealthy", RequestCurrent: true, RequestSource: "automatic", Available: true,
				StartedAt: at(-50 * time.Minute), CompletedAt: at(-47 * time.Minute), Revision: 3, Attention: "attention"},
			{RepoID: 1, Number: 5, Name: "fixture-owner/repository-1", Title: "Fixture pull request 5", Latest: "old-commit", Requested: "old-commit",
				State: "failed", Failure: "The image build failed.", FailureCode: "build_failed", RequestCurrent: true, RequestSource: "manual", Available: true,
				StartedAt: at(-9 * 24 * time.Hour), CompletedAt: at(-9*24*time.Hour + 2*time.Minute), Revision: 1, Attention: "older", AttentionNote: "Over 7 days old"},
		},
		Previews: []prpages.ActivityRow{
			{RepoID: 1, Number: 3, Name: "fixture-owner/repository-1", Title: "Fixture pull request 3", Latest: "latest-commit", Deployed: "older-commit",
				OlderPreview: true, State: "running", Available: true, PreviewURL: "https://pr-3-1-fixture.preview.example.com",
				PublishedAt: at(-12 * time.Hour), ExpiresAt: at(36 * time.Hour)},
		},
	}
}

// fixtureOutbound keeps approvals in memory; odd repositories are available.
type fixtureOutbound struct {
	mu       sync.Mutex
	approved map[int64][]outbound.Approved
	next     int64
}

func (f *fixtureOutbound) Repository(_ context.Context, _ int64, repoID int64) (outbound.RepositoryView, error) {
	if repoID <= 0 || repoID > 25 {
		return outbound.RepositoryView{}, outbound.ErrUnavailable
	}
	return outbound.RepositoryView{ID: repoID, FullName: "fixture-owner/repository-" + strconv.FormatInt(repoID, 10), Available: repoID%2 == 1}, nil
}

func (f *fixtureOutbound) List(_ context.Context, _ int64, repoID int64) ([]outbound.Approved, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]outbound.Approved(nil), f.approved[repoID]...), nil
}

func (f *fixtureOutbound) Add(_ context.Context, _ int64, repoID int64, d egress.Destination, limit int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if repoID%2 == 0 {
		return outbound.ErrAccessRemoved
	}
	for _, a := range f.approved[repoID] {
		if a.Port == d.Port {
			return outbound.ErrPortTaken
		}
	}
	if len(f.approved[repoID]) >= limit {
		return outbound.ErrLimit
	}
	f.next++
	f.approved[repoID] = append(f.approved[repoID], outbound.Approved{ID: f.next, Host: d.Host, Port: d.Port, CreatedAt: time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)})
	return nil
}

func (f *fixtureOutbound) Remove(_ context.Context, _ int64, repoID, id int64) (egress.Destination, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, a := range f.approved[repoID] {
		if a.ID == id {
			f.approved[repoID] = append(f.approved[repoID][:i], f.approved[repoID][i+1:]...)
			return egress.Destination{Host: a.Host, Port: a.Port}, nil
		}
	}
	return egress.Destination{}, outbound.ErrUnavailable
}

// fixtureKitCheck checks a starter kit's .previewrc.yaml with the parser a
// build uses, as internal/app wires it in production.
func fixtureKitCheck(config []byte, offer starterkits.Offer) []starterkits.Problem {
	catalog := fixtureCatalog{"postgres": offer.Postgres, "redis": offer.Redis, "mysql": offer.MySQL}
	_, err := repoconfig.ParseFor(config, repoconfig.Platform{Limits: repoconfig.DefaultLimits(), Services: catalog, BuildTimeout: offer.BuildTimeout})
	var invalid *repoconfig.ValidationError
	if !errors.As(err, &invalid) {
		return nil
	}
	problems := make([]starterkits.Problem, 0, len(invalid.Issues))
	for _, issue := range invalid.Issues {
		problems = append(problems, starterkits.Problem{Field: issue.Field, Message: issue.Message, Line: issue.Line})
	}
	return problems
}

type fixtureCatalog map[string][]string

func (c fixtureCatalog) Versions(kind string) []string { return c[kind] }
func (c fixtureCatalog) Image(kind, version string) (string, bool) {
	return kind + ":" + version, slices.Contains(c[kind], version)
}
