package reviews

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
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/activity"
	"github.com/ajaykathwatee/previa/internal/auth"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/publicid"
	"github.com/ajaykathwatee/previa/internal/platform/realtime"
	"github.com/ajaykathwatee/previa/internal/pullrequests"
	"github.com/ajaykathwatee/previa/internal/workspaces"
	"github.com/alexedwards/scs/v2/memstore"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// isolatedPool migrates a fresh schema of TEST_DATABASE_URL.
func isolatedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for review database tests")
	}
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("reviews_test_%d", time.Now().UnixNano())
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
	cfg.MaxConns = 6
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

// timeline records review events through the real activity feature.
type timeline struct{ recorder *activity.Recorder }

func (a timeline) RecordTx(ctx context.Context, tx pgx.Tx, e Event) error {
	return a.recorder.RecordTx(ctx, tx, activity.Event{PullRequestID: e.PullRequestID, PublicationID: e.PublicationID, ActorID: e.ActorID,
		Kind: e.Kind, Metadata: e.Metadata, DedupKey: e.DedupKey})
}

// fixture is a workspace with every role and two repositories (the Developer
// and Reviewer are granted acme/api), an open pull request #42 there, and an
// outsider with a workspace of their own.
type fixture struct {
	pool                                                  *pgxpool.Pool
	service                                               *Service
	owner, admin, dev, reviewer, outsider, newdev         int64
	workspace, installation, repo, otherRepo, outsideRepo int64
	pr, otherPR, outsidePR                                int64
}

var githubIDs = map[string]int64{"owner": 101, "admin": 102, "dev": 103, "reviewer": 104, "outsider": 105, "newdev": 106}

const (
	shaA = "1111111111111111111111111111111111111111"
	shaB = "2222222222222222222222222222222222222222"
	shaC = "3333333333333333333333333333333333333333"
)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := isolatedPool(t)
	ctx := t.Context()
	f := &fixture{pool: pool}
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
	exec("UPDATE users SET first_name='Ravi',last_name='Reviewer' WHERE id=$1", f.reviewer)
	exec("UPDATE users SET first_name='Maya',last_name='Developer' WHERE id=$1", f.dev)
	scan(&f.installation, "INSERT INTO installations(github_installation_id,owner_user_id,account_login) VALUES(7,$1,'acme') RETURNING id", f.owner)
	scan(&f.workspace, "SELECT workspace_id FROM installations WHERE id=$1", f.installation)
	exec("INSERT INTO workspace_memberships(workspace_id,user_id,role) VALUES($1,$2,'admin'),($1,$3,'developer'),($1,$4,'reviewer'),($1,$5,'developer')", f.workspace, f.admin, f.dev, f.reviewer, f.newdev)
	scan(&f.repo, "INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES($1,100,'acme/api') RETURNING id", f.installation)
	scan(&f.otherRepo, "INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES($1,101,'acme/private') RETURNING id", f.installation)
	exec("INSERT INTO repository_memberships(repo_id,workspace_id,user_id) VALUES($1,$2,$3),($1,$2,$4)", f.repo, f.workspace, f.dev, f.reviewer)
	var outside int64
	scan(&outside, "INSERT INTO installations(github_installation_id,owner_user_id,account_login) VALUES(8,$1,'other') RETURNING id", f.outsider)
	scan(&f.outsideRepo, "INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES($1,200,'other/app') RETURNING id", outside)
	f.pr = f.addPR(t, f.repo, 42, shaA)
	f.otherPR = f.addPR(t, f.otherRepo, 7, shaA)
	f.outsidePR = f.addPR(t, f.outsideRepo, 9, shaA)
	service, err := NewService(pool, timeline{recorder: activity.NewRecorder()}, Options{PreviewDomain: "preview.test"})
	if err != nil {
		t.Fatal(err)
	}
	f.service = service
	return f
}

// addPR opens a pull request.
func (f *fixture) addPR(t *testing.T, repo int64, number int32, sha string) int64 {
	t.Helper()
	var id int64
	if err := f.pool.QueryRow(t.Context(), `INSERT INTO pull_requests(repo_id,number,head_sha,head_repo_id,generation,title,author,head_branch,state,build_status,source_updated_at,description,html_url)
		VALUES($1,$2,$3,100,1,$4,'contributor',$5,'open','open',now(),'Why: <b>faster</b> checkout',$6) RETURNING id`,
		repo, number, sha, fmt.Sprintf("Change %d", number), fmt.Sprintf("feature/change-%d", number), fmt.Sprintf("https://github.com/acme/api/pull/%d", number)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// publish makes sha the pull request's newest deployment, as an activation
// does: a publication and the environment serving it. It returns the
// deployment's number.
func (f *fixture) publish(t *testing.T, pr int64, sha string) int32 {
	t.Helper()
	seq, err := f.deploy(t.Context(), pr, sha)
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

func (f *fixture) deploy(ctx context.Context, pr int64, sha string) (int32, error) {
	var seq int32
	if err := f.pool.QueryRow(ctx, `INSERT INTO preview_publications(pull_request_id,repo_id,workspace_id,sequence,head_sha,generation,revision,container_id,image_tag)
		SELECT p.id,p.repo_id,i.workspace_id,coalesce((SELECT max(x.sequence) FROM preview_publications x WHERE x.pull_request_id=p.id),0)+1,$2,p.generation,
		       coalesce((SELECT max(x.sequence) FROM preview_publications x WHERE x.pull_request_id=p.id),0)+1,'container','image'
		FROM pull_requests p JOIN repos r ON r.id=p.repo_id JOIN installations i ON i.id=r.installation_id WHERE p.id=$1 RETURNING sequence`, pr, sha).Scan(&seq); err != nil {
		return 0, err
	}
	_, err := f.pool.Exec(ctx, `INSERT INTO environments(pull_request_id,subdomain,status,head_sha,applied_generation,applied_revision,container_id,image_tag)
		SELECT p.id,'pr-'||p.number||'-fixture','running',$2,p.generation,$3,'container','image' FROM pull_requests p WHERE p.id=$1
		ON CONFLICT (pull_request_id) DO UPDATE SET status='running',head_sha=EXCLUDED.head_sha,applied_generation=EXCLUDED.applied_generation,
		applied_revision=EXCLUDED.applied_revision,container_id=EXCLUDED.container_id,image_tag=EXCLUDED.image_tag`, pr, sha, seq)
	return seq, err
}

// as is a review change by user on #42 of acme/api.
func (f *fixture) as(user int64) Actor { return Actor{UserID: user, RepoID: f.repo, Number: 42} }

// events lists the pull request's timeline kinds, oldest first.
func (f *fixture) events(t *testing.T, pr int64) []string {
	t.Helper()
	rows, err := f.pool.Query(t.Context(), "SELECT kind FROM activity_events WHERE pull_request_id=$1 ORDER BY id", pr)
	if err != nil {
		t.Fatal(err)
	}
	kinds, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return kinds
}

func key(n int) string { return fmt.Sprintf("request-key-%010d", n) }

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
// middleware with the review and pull request routes; hub may be nil.
func (f *fixture) router(t *testing.T, origin string, hub *realtime.Hub) http.Handler {
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
	NewHandler(f.service, users, live, logger, origin).Mount(r, auth.RequireUser)
	// Open preview belongs to the onboarding feature; this stand-in shows the
	// workspace guard let the request through.
	r.With(auth.RequireUser).Get("/repositories/{repoID}/pull-requests/{number}/open", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return r
}

// browser is a cookie-keeping client for handler tests.
type browser struct {
	router  http.Handler
	cookies map[string]*http.Cookie
}

func newBrowser(router http.Handler) *browser {
	return &browser{router: router, cookies: map[string]*http.Cookie{}}
}

func (b *browser) do(method, path string, form url.Values) *httptest.ResponseRecorder {
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

// reviewPath is #42's review page with public IDs.
func (f *fixture) reviewPath(t *testing.T) string {
	t.Helper()
	return "/repositories/" + f.public(t, "repository", f.repo, 0) + "/pull-requests/" + f.public(t, "pull-request", 42, f.repo) + "/review"
}
