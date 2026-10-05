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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a-h/templ"
	"github.com/ajaykathwatee/previa/internal/auth"
	"github.com/ajaykathwatee/previa/internal/buildlogs"
	"github.com/ajaykathwatee/previa/internal/dashboard"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/httpx"
	"github.com/ajaykathwatee/previa/internal/platform/realtime"
	pr "github.com/ajaykathwatee/previa/internal/pullrequests"
	"github.com/ajaykathwatee/previa/internal/routing"
	"github.com/ajaykathwatee/previa/internal/secrets"
	"github.com/ajaykathwatee/previa/web"
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
	router.Post("/__fixture/logs/seed", func(w http.ResponseWriter, r *http.Request) {
		logs.seed()
		publishLogs()
		w.WriteHeader(http.StatusNoContent)
	})
	router.Post("/__fixture/logs/append", func(w http.ResponseWriter, r *http.Request) {
		logs.add(r.FormValue("text"))
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
		dashboard.Mount(r, handler, auth.RequireUser)
		for _, path := range []string{"/builds", "/environments"} {
			r.With(auth.RequireUser).Get(path, func(w http.ResponseWriter, r *http.Request) {
				activity := prpages.Activity{Environments: r.URL.Path == "/environments", Number: 1, Pages: 1}
				if r.URL.Query().Get("fixture") == "rows" {
					activity.Total = 2
					activity.Rows = []prpages.ActivityRow{
						{RepoID: 1, Number: 3, Name: "fixture-owner/repository-1", Latest: "latest-commit", Requested: "latest-commit", Deployed: "older-commit", State: "running", Available: true,
							PreviewURL: "https://pr-3-1-fixture.preview.example.com", ExpiresAt: "2026-10-07T10:00:00Z", OlderPreview: true, Teardown: true, Waiting: "Waiting for capacity"},
						{RepoID: 1, Number: 4, Name: "fixture-owner/repository-1", Latest: "latest-commit", Requested: "latest-commit", State: "failed", Available: true, Failure: "The app did not become ready."},
					}
					if activity.Environments {
						activity.Rows[0].Waiting = ""
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
					p.Rows = append(p.Rows, prpages.PR{Number: n, Title: title, Author: "design-review", Branch: "feature/preview", SHA: "1234567890abcdef", State: "open", Build: "Not requested"})
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
				if r.Form.Get("return") == "environments" {
					http.Redirect(w, r, "/environments?page=1", http.StatusSeeOther)
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

// fixtureSecrets keeps names only; sealed values are discarded after encryption.
type fixtureSecrets struct {
	mu    sync.Mutex
	names map[int64]map[string]time.Time
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
		out = append(out, secrets.Summary{Name: name, UpdatedAt: at})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fixtureSecrets) Put(ctx context.Context, user, repoID int64, name string, sealed []byte, _ int32) (bool, error) {
	repo, err := f.Repository(ctx, user, repoID)
	if err != nil || !repo.Available {
		return false, secrets.ErrUnavailable
	}
	if len(sealed) == 0 {
		return false, errors.New("fixture expected ciphertext")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.names[repoID] == nil {
		f.names[repoID] = map[string]time.Time{}
	}
	_, exists := f.names[repoID][name]
	f.names[repoID][name] = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	return !exists, nil
}

func (f *fixtureSecrets) Delete(ctx context.Context, user, repoID int64, name string) (bool, error) {
	if _, err := f.Repository(ctx, user, repoID); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, exists := f.names[repoID][name]
	delete(f.names[repoID], name)
	return exists, nil
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

func (f *fixtureLogs) restart(text string) {
	f.mu.Lock()
	f.epoch++
	f.lines = nil
	f.mu.Unlock()
	f.add(text)
}

func (f *fixtureLogs) Target(_ context.Context, userID, repoID int64, number int32) (buildlogs.Target, error) {
	if userID <= 0 || repoID != 1 || number != 3 {
		return buildlogs.Target{}, buildlogs.ErrUnavailable
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return buildlogs.Target{PullRequestID: 3, RepoID: 1, Number: 3, Title: "A careful change to preview 3", Repository: "fixture-owner/repository-1", Available: true,
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
		Build: "Not requested", State: "open", Environment: "Not deployed", URL: "https://github.com/fixture-owner/repository/pull/1"}
	if !available {
		pr.Actions.Unavailable = "GitHub access to this repository is unavailable."
		return pr
	}
	if number == 3 {
		pr.Build, pr.RequestedSHA, pr.Environment, pr.DeployedSHA = "succeeded", "older-commit", "running", "older-commit"
		pr.PreviewURL, pr.ExpiresAt, pr.OlderPreview = "https://pr-3-1-fixture.preview.example.com", "2026-10-07T10:00:00Z", true
		pr.Actions = prpages.Actions{Build: true, Teardown: true, ExpectedRevision: 1, RequestKey: "fixture-request-key-0003"}
	} else {
		pr.Actions = prpages.Actions{Build: true, RequestKey: "fixture-request-key-" + strconv.Itoa(int(number))}
	}
	switch sessions.GetString(r.Context(), "fixture-pr-"+strconv.Itoa(int(number))) {
	case "queued":
		pr.Build, pr.RequestedSHA = "queued", "latest-commit"
		pr.Actions = prpages.Actions{Teardown: true}
	case "removing":
		pr.Environment, pr.PreviewURL, pr.ExpiresAt, pr.OlderPreview = "tearing_down", "", "", false
		pr.RemovedNote = "The owner tore this preview down. Build again to bring it back at the same address."
		pr.Actions = prpages.Actions{}
	}
	return pr
}
