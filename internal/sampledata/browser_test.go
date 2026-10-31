package sampledata

import (
	"context"
	"errors"
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
	"github.com/ajaykathwatee/previa/internal/platform/realtime"
	"github.com/ajaykathwatee/previa/internal/pullrequests"
	"github.com/ajaykathwatee/previa/internal/workspaces"
	"github.com/ajaykathwatee/previa/web"
	"github.com/alexedwards/scs/v2/memstore"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// fixtureIdentity signs in the fixture's users by login.
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

// fixtureGitHub answers the workspace feature's GitHub questions.
type fixtureGitHub struct{ ids map[string]int64 }

func (g fixtureGitHub) LookupGitHubUser(_ context.Context, login string) (int64, string, error) {
	return g.ids[login], login, nil
}

func (fixtureGitHub) RepositoryUserPermission(context.Context, int64, string, string, int64) (string, error) {
	return "write", nil
}

// TestTestDataBrowser drives the Test data tab in Chromium against
// PostgreSQL with the real session, CSRF, public address, workspace and
// live-update code. A test-only route stands in for the worker (a refresh
// that starts, then succeeds). Run it with make test-test-data-browser.
func TestTestDataBrowser(t *testing.T) {
	if os.Getenv("RUN_TEST_DATA_BROWSER_TESTS") != "1" {
		t.Skip("run make test-test-data-browser with TEST_DATABASE_URL")
	}
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	logger := f.runner.logger
	hub := realtime.NewHub()
	defer hub.Close()
	go realtime.Listen(ctx, func(ctx context.Context) (*pgx.Conn, error) {
		return pgx.ConnectConfig(ctx, f.pool.Config().ConnConfig.Copy())
	}, hub, logger)
	ids := map[string]int64{}
	rows, err := f.pool.Query(ctx, "SELECT username, github_id FROM users")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var login string
		var id int64
		if err := rows.Scan(&login, &id); err != nil {
			t.Fatal(err)
		}
		ids[login] = id
	}
	rows.Close()
	f.exec(t, "UPDATE users SET first_name='Olive', last_name='Owner' WHERE id=$1", f.owner)

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
	// The worker: the waiting refresh starts, then succeeds with a PostgreSQL
	// 17 baseline and a short log.
	root.Post("/__fixture/refresh", func(w http.ResponseWriter, r *http.Request) {
		apply := func(query string, args ...any) bool {
			if _, err := f.pool.Exec(r.Context(), query, args...); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return false
			}
			return true
		}
		switch r.URL.Query().Get("to") {
		case "running":
			if !apply("UPDATE baseline_refreshes SET state='running', started_at=now(), attempts=attempts+1, log_open=true, branch='main', commit_sha=$2 WHERE repo_id=$1 AND state='pending'", f.repo, shaOne) {
				return
			}
		case "ready":
			if !apply(`WITH done AS (UPDATE baseline_refreshes SET state='succeeded', finished_at=now(), build_seconds=42, log_open=false, log_lines=3,
					branch='main', commit_sha=$2 WHERE repo_id=$1 AND state IN ('pending','running') RETURNING id)
				INSERT INTO baseline_logs (refresh_id, sequence, kind, line)
				SELECT id, s, CASE WHEN s = 1 THEN 'output' ELSE 'system' END,
				       (ARRAY['Building the test data baseline from branch main at commit 1111111.', 'shop: seeded 2 orders', 'Captured 4.0 MiB of PostgreSQL 17, encrypted, in 1.2s.'])[s + 1]
				FROM done, generate_series(0, 2) s`, f.repo, shaOne) {
				return
			}
			if !apply(`INSERT INTO repository_baselines (repo_id, refresh_id, service_kind, service_version, branch, commit_sha, refresh_sequence, storage_key, size_bytes, checksum, key_version, status)
				SELECT repo_id, id, 'postgres', '17', 'main', $2, sequence, md5(random()::text), 4194304, $3, 1, 'current'
				FROM baseline_refreshes WHERE repo_id=$1 ORDER BY sequence DESC LIMIT 1`, f.repo, shaOne, strings.Repeat("c", 64)) {
				return
			}
		default:
			http.Error(w, "unknown step", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewUnstartedServer(root)
	origin := "http://" + server.Listener.Addr().String()
	store := memstore.NewWithCleanupInterval(0)
	t.Cleanup(store.StopCleanup)
	sessions := auth.NewSessionManager(store, false, logger)
	users := auth.NewHandler(config.ServerConfig{AppBaseURL: origin, SessionSecret: strings.Repeat("s", 32)}, sessions, auth.NewRepository(f.pool), fixtureIdentity{ids: ids}, logger)
	team := workspaces.NewService(workspaces.NewRepository(f.pool), fixtureGitHub{ids: ids}, fixtureGitHub{ids: ids}, nil)
	users.WithAvatarAudience(team).WithActivity(team)
	r := chi.NewRouter()
	r.Use(sessions.LoadAndSave, users.LoadUser, users.CSRF, publicid.New(f.pool).Middleware, workspaces.NewMiddleware(team, users, logger).Load)
	users.Mount(r)
	workspaces.NewHandler(team, users, logger, origin).Mount(r, auth.RequireUser)
	pullrequests.NewHandler(pullrequests.NewRepository(f.pool), nil, nil, users, logger, false).WithLiveUpdates(hub).Mount(r, auth.RequireUser)
	logs := fixedLogs{lines: []LogLine{
		{Sequence: 0, System: true, Text: "Building the test data baseline from branch main at commit 1111111.", At: time.Now()},
		{Sequence: 1, Text: "shop: seeded 2 orders", At: time.Now()},
		{Sequence: 2, System: true, Text: "Captured 4.0 MiB of PostgreSQL 17, encrypted, in 1.2s.", At: time.Now()},
	}}
	NewHandler(f.service, logs, users, hub, logger).Mount(r, auth.RequireUser)
	root.Mount("/", r)
	server.Start()
	defer server.Close()
	repo, err := publicid.New(f.pool).Public(ctx, "repository", f.repo, 0)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, "node", "--test", "testdata.test.cjs")
	cmd.Dir = "../../web/tests/browser"
	cmd.Env = append(os.Environ(), "TEST_DATA_BROWSER_URL="+server.URL, "TEST_DATA_PATH=/repositories/"+repo+"/test-data")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("test data browser checks: %v\n%s", err, output)
	}
	t.Log(string(output))
}
