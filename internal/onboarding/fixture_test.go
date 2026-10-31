package onboarding

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/auth"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/publicid"
	"github.com/ajaykathwatee/previa/internal/platform/realtime"
	"github.com/ajaykathwatee/previa/internal/platform/workers"
	"github.com/ajaykathwatee/previa/internal/pullrequests"
	"github.com/ajaykathwatee/previa/internal/repoconfig"
	"github.com/ajaykathwatee/previa/internal/workspaces"
	"github.com/alexedwards/scs/v2/memstore"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// isolatedPool migrates a fresh schema of TEST_DATABASE_URL.
func isolatedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for repository setup database tests")
	}
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("onboarding_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		defer admin.Close()
		_, _ = admin.Exec(c, "DROP SCHEMA "+quoted+" CASCADE")
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	files, _ := filepath.Glob("../../db/migrations/*.sql")
	sort.Strings(files)
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(b), "-- +goose Down")
		if _, err := pool.Exec(ctx, up); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
	}
	return pool
}

type recordedJobs struct {
	mu   sync.Mutex
	args []CheckArgs
}

func (j *recordedJobs) InsertTx(_ context.Context, _ pgx.Tx, args river.JobArgs, _ *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if a, ok := args.(CheckArgs); ok {
		j.args = append(j.args, a)
	}
	return &rivertype.JobInsertResult{}, nil
}

func (j *recordedJobs) last() CheckArgs {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.args) == 0 {
		return CheckArgs{}
	}
	return j.args[len(j.args)-1]
}

type fixedPlatform struct{ fingerprint string }

func (p *fixedPlatform) Platform(context.Context) (repoconfig.Platform, string, error) {
	return testPlatform, p.fingerprint, nil
}

type fakeCaps struct{ caps workers.Capabilities }

func (c *fakeCaps) Current(context.Context) (workers.Capabilities, error) { return c.caps, nil }

type fakeSecrets struct {
	names      []string
	configured bool
}

func (s *fakeSecrets) Names(context.Context, int64, int64) ([]string, error) { return s.names, nil }
func (s *fakeSecrets) Configured() bool                                      { return s.configured }

// fixture is a workspace with every role, two repositories (the Developer
// and Reviewer are granted acme/api) and an open pull request #42, plus an
// outsider's own workspace.
type fixture struct {
	pool                                                  *pgxpool.Pool
	jobs                                                  *recordedJobs
	platform                                              *fixedPlatform
	caps                                                  *fakeCaps
	secrets                                               *fakeSecrets
	service                                               *Service
	owner, admin, dev, reviewer, outsider, newdev         int64
	workspace, installation, repo, otherRepo, outsideRepo int64
	pr                                                    int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := isolatedPool(t)
	ctx := t.Context()
	f := &fixture{pool: pool, jobs: &recordedJobs{}, platform: &fixedPlatform{fingerprint: "fp"},
		caps: &fakeCaps{caps: workers.Capabilities{Execution: true, Discovery: true}}, secrets: &fakeSecrets{configured: true}}
	scan := func(dst *int64, query string, args ...any) {
		t.Helper()
		if err := pool.QueryRow(ctx, query, args...).Scan(dst); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	for login, dst := range map[string]*int64{"owner": &f.owner, "admin": &f.admin, "dev": &f.dev, "reviewer": &f.reviewer, "outsider": &f.outsider, "newdev": &f.newdev} {
		scan(dst, "INSERT INTO users(github_id,username) VALUES($1,$2) RETURNING id", githubIDs[login], login)
	}
	exec("UPDATE users SET first_name='Olive',last_name='Owner' WHERE id=$1", f.owner)
	scan(&f.installation, "INSERT INTO installations(github_installation_id,owner_user_id,account_login) VALUES(7,$1,'acme') RETURNING id", f.owner)
	scan(&f.workspace, "SELECT workspace_id FROM installations WHERE id=$1", f.installation)
	exec("INSERT INTO workspace_memberships(workspace_id,user_id,role) VALUES($1,$2,'admin'),($1,$3,'developer'),($1,$4,'reviewer'),($1,$5,'developer')", f.workspace, f.admin, f.dev, f.reviewer, f.newdev)
	scan(&f.repo, "INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES($1,100,'acme/api') RETURNING id", f.installation)
	scan(&f.otherRepo, "INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES($1,101,'acme/private') RETURNING id", f.installation)
	exec("INSERT INTO repository_memberships(repo_id,workspace_id,user_id) VALUES($1,$2,$3),($1,$2,$4)", f.repo, f.workspace, f.dev, f.reviewer)
	var outside int64
	scan(&outside, "INSERT INTO installations(github_installation_id,owner_user_id,account_login) VALUES(8,$1,'other') RETURNING id", f.outsider)
	scan(&f.outsideRepo, "INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES($1,200,'other/app') RETURNING id", outside)
	f.pr = f.addPR(t, 42, testSHA, -time.Hour)
	service, err := NewService(pool, f.jobs, f.platform, f.caps, f.secrets, Options{PreviewDomain: "preview.test", Discover: true})
	if err != nil {
		t.Fatal(err)
	}
	f.service = service
	return f
}

var githubIDs = map[string]int64{"owner": 101, "admin": 102, "dev": 103, "reviewer": 104, "outsider": 105, "newdev": 106}

// addPR opens a pull request on acme/api, last updated updated ago.
func (f *fixture) addPR(t *testing.T, number int32, sha string, updated time.Duration) int64 {
	t.Helper()
	var id int64
	if err := f.pool.QueryRow(t.Context(), `INSERT INTO pull_requests(repo_id,number,head_sha,head_repo_id,generation,title,author,head_branch,state,build_status,source_updated_at)
		VALUES($1,$2,$3,100,1,$4,'contributor',$5,'open','open',now()+$6::interval) RETURNING id`,
		f.repo, number, sha, fmt.Sprintf("Change %d", number), fmt.Sprintf("feature/change-%d", number), updated.String()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// deploy publishes the pull request's head as a running preview.
func (f *fixture) deploy(t *testing.T, pr int64) {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO environments(pull_request_id,subdomain,status,head_sha,applied_generation,applied_revision)
		SELECT id,'pr-'||number||'-fixture','running',head_sha,generation,1 FROM pull_requests WHERE id=$1`, pr); err != nil {
		t.Fatal(err)
	}
}

// checkRow is a setup check as stored.
type checkRow struct {
	Revision           int64
	State, Failure     string
	ResultSHA, Head    string
	Result             []byte
	Platform           string
	Checked, Requested time.Time
}

func (f *fixture) check(t *testing.T, pr int64) checkRow {
	t.Helper()
	var c checkRow
	var checked *time.Time
	err := f.pool.QueryRow(t.Context(), "SELECT revision,state,failure_code,result_sha,head_sha,result,platform,checked_at,requested_at FROM repository_setup_checks WHERE pull_request_id=$1", pr).
		Scan(&c.Revision, &c.State, &c.Failure, &c.ResultSHA, &c.Head, &c.Result, &c.Platform, &checked, &c.Requested)
	if err != nil {
		t.Fatal(err)
	}
	if checked != nil {
		c.Checked = *checked
	}
	return c
}

type testIdentity struct{}

func (testIdentity) AuthCodeURL(state string) string {
	return "/test-github?state=" + url.QueryEscape(state)
}
func (testIdentity) Authenticate(_ context.Context, code string) (auth.Identity, error) {
	if id, ok := githubIDs[code]; ok {
		return auth.Identity{ID: id, Username: code}, nil
	}
	return auth.Identity{}, errors.New("unknown fixture identity")
}

// fixtureGitHub answers the workspace feature's GitHub questions.
type fixtureGitHub struct{}

func (fixtureGitHub) LookupGitHubUser(_ context.Context, login string) (int64, string, error) {
	return githubIDs[login], login, nil
}
func (fixtureGitHub) RepositoryUserPermission(context.Context, int64, string, string, int64) (string, error) {
	return "write", nil
}

// router serves the real session, CSRF, public address and workspace
// middleware with the setup, workspace and pull request routes.
func (f *fixture) router(t *testing.T, origin string) http.Handler {
	t.Helper()
	return f.routerWith(t, origin, nil, nil)
}

// routerWith adds live updates from hub and extra routes inside the session
// group (the browser suite's Overview).
func (f *fixture) routerWith(t *testing.T, origin string, hub *realtime.Hub, extra func(chi.Router, *auth.Handler)) http.Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := memstore.NewWithCleanupInterval(0)
	t.Cleanup(store.StopCleanup)
	sessions := auth.NewSessionManager(store, false, logger)
	users := auth.NewHandler(config.ServerConfig{AppBaseURL: origin, SessionSecret: strings.Repeat("s", 32)}, sessions, auth.NewRepository(f.pool), testIdentity{}, logger)
	team := workspaces.NewService(workspaces.NewRepository(f.pool), fixtureGitHub{}, fixtureGitHub{}, nil)
	users.WithAvatarAudience(team).WithActivity(team)
	r := chi.NewRouter()
	r.Use(sessions.LoadAndSave, users.LoadUser, users.CSRF, publicid.New(f.pool).Middleware, workspaces.NewMiddleware(team, users, logger).Load)
	users.Mount(r)
	workspaces.NewHandler(team, users, logger, origin).Mount(r, auth.RequireUser)
	prs := pullrequests.NewHandler(pullrequests.NewRepository(f.pool), nil, nil, users, logger, false).WithPreviewDomain("preview.test")
	var live Cursor
	if hub != nil {
		prs.WithLiveUpdates(hub)
		live = hub
	}
	prs.Mount(r, auth.RequireUser)
	NewHandler(f.service, users, live, logger).Mount(r, auth.RequireUser)
	if extra != nil {
		extra(r, users)
	}
	return r
}

// browser is a cookie-keeping client for handler tests.
type browser struct {
	router  http.Handler
	cookies map[string]*http.Cookie
	headers http.Header
}

func newBrowser(router http.Handler) *browser {
	return &browser{router: router, cookies: map[string]*http.Cookie{}, headers: http.Header{}}
}

func (b *browser) do(method, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range b.headers {
		req.Header[k] = v
	}
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

func (b *browser) login(t *testing.T, login string) {
	t.Helper()
	res := b.do(http.MethodGet, "/auth/github/login", nil)
	redirect, err := url.Parse(res.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	res = b.do(http.MethodGet, "/auth/github/callback?"+url.Values{"state": {redirect.Query().Get("state")}, "code": {login}}.Encode(), nil)
	if res.Code != http.StatusSeeOther {
		t.Fatalf("login %s: %d %s", login, res.Code, res.Body.String())
	}
}

var csrfField = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

func (b *browser) csrf(t *testing.T) string {
	t.Helper()
	match := csrfField.FindStringSubmatch(b.do(http.MethodGet, "/workspaces", nil).Body.String())
	if len(match) != 2 {
		t.Fatal("missing CSRF token")
	}
	return match[1]
}

func (f *fixture) public(t *testing.T, kind string, id, parent int64) string {
	t.Helper()
	value, err := publicid.New(f.pool).Public(t.Context(), kind, id, parent)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func slogDiscard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
