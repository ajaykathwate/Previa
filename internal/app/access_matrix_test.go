package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/ajaykathwatee/previa/internal/platform/realtime"
	"github.com/ajaykathwatee/previa/internal/webhooks"
	"github.com/ajaykathwatee/previa/internal/workspaces"
	"github.com/alexedwards/scs/pgxstore"
	"github.com/go-chi/chi/v5"
	"github.com/google/go-github/v91/github"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/net/html"
)

// matrixApp is the fake GitHub as the server's App: installations are always
// reachable and installation signals are not needed by these requests.
type matrixApp struct{ *fakeGitHub }

func (matrixApp) Installation(_ context.Context, id int64) (*github.Installation, error) {
	return &github.Installation{ID: &id}, nil
}
func (matrixApp) Invalidate(int64)                                           {}
func (matrixApp) WithInstallationSignals(func(context.Context, int64) error) {}

// matrixDirectory answers the workspace service's GitHub lookups while the
// fixture seeds memberships.
type matrixDirectory map[string]int64

func (d matrixDirectory) LookupGitHubUser(_ context.Context, login string) (int64, string, error) {
	return d[login], login, nil
}
func (matrixDirectory) RepositoryUserPermission(context.Context, int64, string, string, int64) (string, error) {
	return "write", nil
}

// Personas: everyone who can reach the dashboard, and how they relate to the
// shared workspace. devNoSource is a granted Developer whom GitHub no longer
// lets read the repository; removed was a Developer; outsider uses Previa in
// another workspace but has GitHub access to the shared repository.
const (
	personaAnonymous   = "anonymous"
	personaOwner       = "owner"
	personaAdmin       = "admin"
	personaDeveloper   = "dev"
	personaDevNoSource = "devnosource"
	personaReviewer    = "reviewer"
	personaRemoved     = "removed"
	personaOutsider    = "outsider"
)

var matrixPersonas = []string{personaAnonymous, personaOwner, personaAdmin, personaDeveloper, personaDevNoSource, personaReviewer, personaRemoved, personaOutsider}

type accessFixture struct {
	pool     *pgxpool.Pool
	features *serverFeatures
	router   http.Handler
	github   *fakeGitHub
	users    map[string]int64
	// public IDs by fixture name; token is a pending Reviewer invitation's.
	public                              map[string]string
	token                               string
	workspace, repoA, repoB, prA, prB   int64
	invitation, destination, otherSpace int64
	installation                        int64
	requests                            int
}

const (
	matrixSecretKey = "1:" + "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	matrixDomain    = "preview.example.test"
)

// newTeamFixture assembles the server exactly as RunServer does, on pool with
// the fake GitHub as its App, and seeds one shared workspace: repository A
// (granted to the Developers and the Reviewer), repository B (granted to
// nobody below Admin), a removed Developer, a pending Reviewer invitation and
// an outsider's own workspace. GitHub lets admin, dev, removed and outsider
// write to the repositories and devnosource and reviewer not even read them.
func newTeamFixture(t *testing.T, pool *pgxpool.Pool, gh *fakeGitHub, domain string) *accessFixture {
	t.Helper()
	ctx := t.Context()
	store := pgxstore.NewWithCleanupInterval(pool, 0)
	t.Cleanup(store.StopCleanup)
	gh.collaborators = map[string]fakeCollaborator{
		"admin": {ID: 102, Permission: "maintain"}, "dev": {ID: 103, Permission: "write"},
		"removed": {ID: 106, Permission: "write"}, "outsider": {ID: 107, Permission: "admin"},
	}
	cfg := config.ServerConfig{
		AppBaseURL: "https://" + testAppHost, GitHubOAuthClientID: "matrix-client", GitHubOAuthClientSecret: "matrix-secret",
		SessionSecret: strings.Repeat("matrix-session-", 3), GitHubWebhookSecret: testWebhookSecret,
		GitHubAppID: e2eAppID, GitHubAppSlug: e2eSlug, PreviewBaseDomain: domain,
	}
	cfg.AppEnv = config.Production
	cfg.SecretsEncryptionKeys, cfg.SecretsActiveKeyVersion = matrixSecretKey, "1"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if os.Getenv("ACCESS_MATRIX_LOG") == "1" {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	hub := realtime.NewHub()
	t.Cleanup(hub.Close)
	features, err := newServerFeaturesWithApp(cfg, pool, store, hub, logger, matrixApp{gh})
	if err != nil {
		t.Fatal(err)
	}
	router, err := features.routes(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	f := &accessFixture{pool: pool, features: features, router: router, github: gh, users: map[string]int64{}, public: map[string]string{}}
	directory := matrixDirectory{}
	for login, githubID := range map[string]int64{"owner": 1, "admin": 102, "dev": 103, "reviewer": 104, "devnosource": 105, "removed": 106, "outsider": 107, "newbie": 108} {
		var id int64
		f.scan(t, "INSERT INTO users(github_id,username) VALUES($1,$2) RETURNING id", []any{githubID, login}, &id)
		f.users[login], directory[login] = id, githubID
	}
	f.scan(t, "INSERT INTO installations(github_installation_id,owner_user_id,account_login) VALUES(900,$1,'octo') RETURNING id,workspace_id", []any{f.users["owner"]}, &f.installation, &f.workspace)
	f.scan(t, "INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES($1,$2,'octo/app') RETURNING id", []any{f.installation, e2eRepoID}, &f.repoA)
	f.scan(t, "INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES($1,300,'octo/other') RETURNING id", []any{f.installation}, &f.repoB)
	f.scan(t, "INSERT INTO installations(github_installation_id,owner_user_id,account_login) VALUES(901,$1,'outsider') RETURNING workspace_id", []any{f.users["outsider"]}, &f.otherSpace)

	members := workspaces.NewService(workspaces.NewRepository(pool), directory, directory, nil)
	invite := func(login, role string, repos []int64, accept bool) string {
		t.Helper()
		w, err := members.Read(ctx, f.users["owner"], f.workspace)
		if err != nil {
			t.Fatal(err)
		}
		token, err := members.Invite(ctx, workspaces.InvitationCommand{ActorID: f.users["owner"], WorkspaceID: f.workspace, Revision: w.Revision, Login: login, Role: role, Repositories: repos})
		if err != nil {
			t.Fatalf("invite %s: %v", login, err)
		}
		if accept {
			if _, err := members.RespondInvitation(ctx, f.users[login], directory[login], token, true); err != nil {
				t.Fatalf("accept %s: %v", login, err)
			}
		}
		return token
	}
	invite("admin", "admin", nil, true)
	invite("dev", "developer", []int64{f.repoA}, true)
	invite("devnosource", "developer", []int64{f.repoA}, true)
	invite("reviewer", "reviewer", []int64{f.repoA}, true)
	invite("removed", "developer", []int64{f.repoA}, true)
	w, err := members.Read(ctx, f.users["owner"], f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := members.ChangeMember(ctx, workspaces.MemberChange{ActorID: f.users["owner"], WorkspaceID: f.workspace, WorkspaceRevision: w.Revision,
		UserID: f.users["removed"], Revision: f.revisionOf(t, "SELECT revision FROM workspace_memberships WHERE workspace_id=$1 AND user_id=$2", f.workspace, f.users["removed"]),
		Remove: true}); err != nil {
		t.Fatal(err)
	}
	f.token = invite("newbie", "reviewer", []int64{f.repoA}, false)
	f.scan(t, "SELECT id FROM workspace_invitations WHERE workspace_id=$1 AND state='pending' ORDER BY id DESC LIMIT 1", []any{f.workspace}, &f.invitation)
	for name, ref := range map[string]struct {
		table string
		id    int64
	}{
		"workspace": {"workspaces", f.workspace}, "otherWorkspace": {"workspaces", f.otherSpace}, "repoA": {"repos", f.repoA}, "repoB": {"repos", f.repoB},
		"member": {"users", f.users["dev"]}, "admin": {"users", f.users["admin"]}, "invitation": {"workspace_invitations", f.invitation},
		"installation": {"installations", f.installation},
	} {
		f.public[name] = f.publicID(t, ref.table, ref.id)
	}
	return f
}

// newAccessFixture is the team on a fresh schema, plus what the matrix's
// routes act on: an open PR in each repository, deployment 1 of PR A live, a
// live worker, an approved outbound destination, the Owner's finding 1 and
// repository A's sealed test login.
func newAccessFixture(t *testing.T) *accessFixture {
	t.Helper()
	ctx := t.Context()
	pool := e2eSchema(t)
	gh := newFakeGitHub(t)
	gh.push(e2eCommit1, e2eApp("matrix"))
	f := newTeamFixture(t, pool, gh, matrixDomain)
	producer, err := queue.NewProducer(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	events := webhooks.NewService(pool, environments.NewRequestBroker(pool, producer))
	for _, pr := range []struct {
		repo   int64
		number int32
		dest   *int64
	}{{f.repoA, e2ePRNumber, &f.prA}, {f.repoB, 3, &f.prB}} {
		opened, err := events.ProcessPR(ctx, webhooks.PREvent{RepoID: pr.repo, Number: pr.number, HeadSHA: e2eCommit1, Action: "opened", MergeableState: "clean"})
		if err != nil {
			t.Fatal(err)
		}
		*pr.dest = opened.PullRequestID
	}
	// Deployment 1 of PR A is live, as an activation records it.
	if _, err := pool.Exec(ctx, `INSERT INTO preview_publications(pull_request_id,repo_id,workspace_id,sequence,head_sha,generation,revision,container_id,image_tag)
		SELECT p.id,p.repo_id,i.workspace_id,1,$2,p.generation,1,'matrix-container','matrix-image'
		FROM pull_requests p JOIN repos r ON r.id=p.repo_id JOIN installations i ON i.id=r.installation_id WHERE p.id=$1`, f.prA, e2eCommit1); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO environments(pull_request_id,subdomain,status,head_sha,applied_generation,applied_revision,container_id,image_tag)
		SELECT p.id,'matrix-preview','running',$2,p.generation,1,'matrix-container','matrix-image' FROM pull_requests p WHERE p.id=$1`, f.prA, e2eCommit1); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO worker_capabilities(worker_id,execution,routing,reporting,housekeeping,discovery,private_previews,baselines)
		VALUES('aaaaaaaaaaaaaaaa',true,true,true,true,true,true,true)`); err != nil {
		t.Fatal(err)
	}
	f.scan(t, "INSERT INTO repository_destinations(repo_id,host,port,created_by) VALUES($1,'cache.example.com',6379,$2) RETURNING id", []any{f.repoA, f.users["owner"]}, &f.destination)
	f.public["prA"], f.public["prB"] = f.publicID(t, "pull_requests", f.prA), f.publicID(t, "pull_requests", f.prB)
	f.public["destination"] = f.publicID(t, "repository_destinations", f.destination)
	// The Owner reports finding 1 on deployment 1; edits stay the author's.
	if w := f.send(t, personaOwner, http.MethodPost, "/repositories/"+f.public["repoA"]+"/pull-requests/"+f.public["prA"]+"/review/findings",
		url.Values{"body": {"The checkout button does nothing."}, "deployment": {"1"}, "request_key": {"matrix-request-key-seed00001"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("seed finding: %d %s", w.Code, w.Body)
	}
	// The Owner stores repository A's sealed test login for members to reveal.
	if w := f.send(t, personaOwner, http.MethodPost, "/repositories/"+f.public["repoA"]+"/pull-requests/"+f.public["prA"]+"/review/test-access",
		url.Values{"account": {"tester@example.test"}, "secret": {"matrix-password"}, "revision": {"0"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("seed test access: %d %s", w.Code, w.Body)
	}
	return f
}

func (f *accessFixture) scan(t *testing.T, query string, args []any, dest ...any) {
	t.Helper()
	if err := f.pool.QueryRow(t.Context(), query, args...).Scan(dest...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func (f *accessFixture) publicID(t *testing.T, table string, id int64) string {
	t.Helper()
	var public string
	f.scan(t, "SELECT public_id::text FROM "+table+" WHERE id=$1", []any{id}, &public)
	return public
}

func (f *accessFixture) revisionOf(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var revision int64
	f.scan(t, query, args, &revision)
	return revision
}

// revision reads a row's current revision for forms that must be current.
func (f *accessFixture) revision(t *testing.T, query string, args ...any) string {
	t.Helper()
	return strconv.FormatInt(f.revisionOf(t, query, args...), 10)
}

// staleRevision is a well-formed revision no row has: refusals that come
// before the revision check prove authorization, later ones a conflict.
const staleRevision = "999999"

// form is the valid request each mutation needs, so a refusal is about who
// sends it rather than what was sent. Destructive actions use a stale
// revision: they must be authorized, then conflict, and change nothing.
func (f *accessFixture) form(t *testing.T, persona, pattern, repo string) url.Values {
	t.Helper()
	workspaceRevision := func() string {
		return f.revision(t, "SELECT revision FROM workspaces WHERE id=$1", f.workspace)
	}
	profileRevision := func() string {
		return f.revision(t, "SELECT profile_revision FROM workspaces WHERE id=$1", f.workspace)
	}
	f.requests++
	key := fmt.Sprintf("matrix-request-key-%s-%06d", persona, f.requests)
	const review = "/repositories/{repoID}/pull-requests/{number}/review"
	switch pattern {
	case "/repositories/{repoID}/policy":
		return url.Values{"mode": {"automatic"}}
	case "/repositories/{repoID}/preview-access":
		return url.Values{"private": {"true"}}
	case "/repositories/{repoID}/secrets":
		return url.Values{"name": {"MATRIX_SECRET"}, "value": {"matrix-value"}}
	case "/repositories/{repoID}/secrets/build":
		return url.Values{"name": {"MATRIX_SECRET"}, "build": {"true"}}
	case "/repositories/{repoID}/secrets/delete":
		return url.Values{"name": {"MATRIX_UNUSED"}} // the saved secret stays as evidence
	case "/repositories/{repoID}/outbound":
		return url.Values{"host": {"db.example.com"}, "port": {"5432"}}
	case "/repositories/{repoID}/test-data/settings":
		return url.Values{"revision": {"0"}, "enabled": {"on"}, "baseline_branch": {"main"}}
	case "/repositories/{repoID}/test-data/off":
		return url.Values{"revision": {"0"}}
	case "/repositories/{repoID}/setup/check":
		return url.Values{"expected_sha": {e2eCommit1}}
	case review + "/instructions":
		return url.Values{"summary": {"Try the checkout."}, "revision": {"0"}}
	case review + "/findings":
		return url.Values{"body": {"The total is wrong."}, "deployment": {"1"}, "request_key": {key}}
	case review + "/findings/{finding}/replies":
		return url.Values{"body": {"Seen it too."}, "deployment": {"1"}, "request_key": {key}}
	case review + "/findings/{finding}/messages/{position}/edit":
		return url.Values{"body": {"The checkout button does nothing at all."}, "revision": {"1"}}
	case review + "/findings/{finding}/messages/{position}/remove", review + "/findings/{finding}/remove":
		return url.Values{"reason": {"spam"}}
	case review + "/findings/{finding}/resolve":
		return url.Values{"revision": {f.revision(t, "SELECT revision FROM preview_feedback_threads WHERE pull_request_id=$1 AND number=1", f.prA)}, "deployment": {"1"}}
	case review + "/findings/{finding}/reopen":
		return url.Values{"revision": {f.revision(t, "SELECT revision FROM preview_feedback_threads WHERE pull_request_id=$1 AND number=1", f.prA)}}
	case review + "/test-access":
		return url.Values{"account": {"tester@example.test"}, "secret": {"matrix-password-2"}, "revision": {
			f.revision(t, "SELECT coalesce((SELECT revision FROM review_test_access WHERE repo_id=$1),0)", map[string]int64{"A": f.repoA, "B": f.repoB}[repo])}}
	case "/workspaces/{workspaceID}/settings":
		return url.Values{"name": {"Matrix team"}, "profile_revision": {profileRevision()}}
	case "/workspaces/{workspaceID}/logo/remove":
		return url.Values{"profile_revision": {profileRevision()}}
	case "/workspaces/{workspaceID}/invitations":
		return url.Values{"login": {"stakeholder@example.test"}, "role": {"reviewer"}, "repository": {f.public["repoA"]}, "workspace_revision": {workspaceRevision()}}
	case "/workspaces/{workspaceID}/invitations/{invitationID}":
		return url.Values{"action": {"revoke"}, "revision": {f.revision(t, "SELECT revision FROM workspace_invitations WHERE id=$1", f.invitation)}, "workspace_revision": {workspaceRevision()}}
	case "/workspaces/{workspaceID}/members/{userID}":
		return url.Values{"role": {"developer"}, "repository": {f.public["repoA"], f.public["repoB"]}, "workspace_revision": {workspaceRevision()},
			"revision": {f.revision(t, "SELECT revision FROM workspace_memberships WHERE workspace_id=$1 AND user_id=$2", f.workspace, f.users["dev"])}}
	case "/workspaces/{workspaceID}/ownership":
		return url.Values{"action": {"nominate"}, "target": {f.public["admin"]}, "workspace_revision": {workspaceRevision()}}
	case "/workspaces/{workspaceID}/delete":
		return url.Values{"confirm": {"DELETE"}, "workspace_revision": {staleRevision}}
	case "/account/workspaces/{workspaceID}/delete":
		return url.Values{"confirm": {"DELETE"}, "revision": {staleRevision}}
	case "/account/repositories/{repoID}/disconnect", "/account/installations/{installationID}/disconnect":
		return url.Values{"confirm": {"DISCONNECT"}, "revision": {staleRevision}}
	case "/workspace-invitations/{token}", "/workspace-invitations/received/{invitationID}":
		return url.Values{"action": {"accept"}}
	}
	return nil
}

// destructive routes run after everything else a persona tries.
func destructive(route string) bool {
	for _, word := range []string{"/remove", "/delete", "/teardown", "/disconnect", "/ownership", "/logout", "/revoke", "/invitations/{invitationID}"} {
		if strings.Contains(route, word) {
			return true
		}
	}
	return false
}

// session signs a persona in with a fresh session at the account's current
// epoch, as the GitHub callback would, and returns its cookie.
func (f *accessFixture) session(t *testing.T, persona string) *http.Cookie {
	t.Helper()
	if persona == personaAnonymous {
		return nil
	}
	user := f.users[persona]
	var epoch int64
	if err := f.pool.QueryRow(t.Context(), "SELECT session_epoch FROM users WHERE id=$1", user).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	ctx, err := f.features.sessions.Load(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	f.features.sessions.Put(ctx, "user_id", user)
	f.features.sessions.Put(ctx, "session_epoch", epoch)
	f.features.sessions.Put(ctx, "csrf_token", "matrix-csrf")
	token, _, err := f.features.sessions.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: f.features.sessions.Cookie.Name, Value: token}
}

// send makes one same-origin request as a persona; POST forms carry the
// session's CSRF token unless the form already sets one.
func (f *accessFixture) send(t *testing.T, persona, method, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if method == http.MethodPost {
		if form == nil {
			form = url.Values{}
		}
		if persona != personaAnonymous && form.Get("csrf_token") == "" {
			form.Set("csrf_token", "matrix-csrf")
		}
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, "https://"+testAppHost+path, body)
	r.RemoteAddr = "192.0.2.20:40000"
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "https://"+testAppHost)
	}
	if c := f.session(t, persona); c != nil {
		r.AddCookie(c)
	}
	if strings.HasSuffix(r.URL.Path, "/events") {
		// A granted live stream stays open; its status is written first.
		ctx, cancel := context.WithTimeout(r.Context(), 300*time.Millisecond)
		defer cancel()
		r = r.WithContext(ctx)
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, r)
	return w
}

// matrixRoutes lists every method and pattern the assembled router serves.
func matrixRoutes(t *testing.T, router http.Handler) []string {
	t.Helper()
	routes, ok := router.(chi.Routes)
	if !ok {
		t.Fatalf("router %T does not expose its routes", router)
	}
	var out []string
	if err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		out = append(out, method+" "+strings.ReplaceAll(route, "/*/", "/"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := destructive(out[i]), destructive(out[j]); a != b {
			return b
		}
		return out[i] < out[j]
	})
	return out
}

// run sends every route to the server as persona, each repository route
// for repositories A and B, and returns what each answered (keyed
// "METHOD pattern target") and the database fingerprint afterwards.
func (f *accessFixture) run(t *testing.T, persona string) (map[string]string, string, string) {
	t.Helper()
	navigation := f.navigation(t, persona)
	got := map[string]string{}
	for _, route := range matrixRoutes(t, f.router) {
		method, pattern, _ := strings.Cut(route, " ")
		for _, target := range f.targets(pattern) {
			var form url.Values
			if method == http.MethodPost {
				form = f.form(t, persona, pattern, target.name)
			}
			got[route+" "+target.name] = outcome(f.send(t, persona, method, target.path, form))
		}
	}
	return got, f.fingerprint(t), navigation
}

var (
	sidebarPattern = regexp.MustCompile(`(?s)<aside class="app-sidebar".*?</aside>`)
	hrefPattern    = regexp.MustCompile(`href="([^"]+)"`)
)

// navigation lists the sidebar destinations persona is offered, in order,
// with public IDs as *: roles must not be shown what they cannot use.
func (f *accessFixture) navigation(t *testing.T, persona string) string {
	t.Helper()
	aside := sidebarPattern.FindString(f.send(t, persona, http.MethodGet, "/workspaces", nil).Body.String())
	var links []string
	for _, m := range hrefPattern.FindAllStringSubmatch(aside, -1) {
		links = append(links, publicIDPattern.ReplaceAllString(m[1], "*"))
	}
	return strings.Join(links, " ")
}

var publicIDPattern = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// outcome spells a response as the matrix does: its status, or for a
// redirect where it leads with public IDs as * and no query. L and W are
// See Other to sign-in and to the workspace chooser; ">path" is See Other.
func outcome(w *httptest.ResponseRecorder) string {
	location := w.Header().Get("Location")
	if location == "" {
		return strconv.Itoa(w.Code)
	}
	location, _, _ = strings.Cut(location, "?")
	location = publicIDPattern.ReplaceAllString(location, "*")
	switch {
	case w.Code == http.StatusSeeOther && location == "/auth/github/login":
		return "L"
	case w.Code == http.StatusSeeOther && location == "/workspaces":
		return "W"
	case w.Code == http.StatusSeeOther:
		return ">" + location
	}
	return strconv.Itoa(w.Code) + ">" + location
}

// fingerprint summarizes everything a matrix request could change: build
// policy and privacy, build requests and teardowns, destinations, grants,
// the workspace's name, invitations, ownership transfers, findings,
// instructions and test logins. Refused requests must leave it as seeded.
func (f *accessFixture) fingerprint(t *testing.T) string {
	t.Helper()
	var out string
	err := f.pool.QueryRow(t.Context(), `SELECT concat_ws(' ',
		'policy='||(SELECT string_agg(build_mode||'/'||private_previews, ',' ORDER BY id) FROM repos WHERE id IN ($1,$2)),
		'builds='||(SELECT count(*) FROM build_intents WHERE pull_request_id IN ($3,$4) AND requested_by IS NOT NULL),
		'teardown='||(SELECT coalesce(teardown_generation,0) FROM environments WHERE pull_request_id=$3),
		'destinations='||(SELECT coalesce(string_agg(r.full_name||':'||d.host, ',' ORDER BY r.full_name, d.host), '-') FROM repository_destinations d JOIN repos r ON r.id=d.repo_id),
		'secrets='||(SELECT count(*) FROM env_secrets),
		'devGrants='||(SELECT count(*) FROM repository_memberships WHERE user_id=$5),
		'workspace='||(SELECT quote_literal(name) FROM workspaces WHERE id=$6),
		'invitations='||(SELECT string_agg(role||'/'||state, ',' ORDER BY id) FROM workspace_invitations WHERE workspace_id=$6),
		'transfers='||(SELECT count(*) FROM workspace_transfers),
		'findings='||(SELECT string_agg(number||'/'||state, ',' ORDER BY number) FROM preview_feedback_threads WHERE pull_request_id=$3),
		'instructions='||(SELECT count(*) FROM pr_review_instructions),
		'testLogins='||(SELECT coalesce(string_agg(repo_id::text||'@'||revision, ',' ORDER BY repo_id), '-') FROM review_test_access))`,
		f.repoA, f.repoB, f.prA, f.prB, f.users["dev"], f.workspace).Scan(&out)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(strings.ReplaceAll(out, strconv.FormatInt(f.repoA, 10)+"@", "A@"), strconv.FormatInt(f.repoB, 10)+"@", "B@")
}

// TestEveryRouteRespectsEveryRole drives every route of the assembled server
// as every kind of person (signed out; Owner; Admin; granted Developer, with
// and without GitHub access to the repository; granted Reviewer; removed
// member; another workspace's owner) against a granted and an ungranted
// repository, with valid forms, then checks what changed in the database.
// A route added without a matrix entry fails here until it is classified.
func TestEveryRouteRespectsEveryRole(t *testing.T) {
	for i, persona := range matrixPersonas {
		t.Run(persona, func(t *testing.T) {
			t.Parallel()
			f := newAccessFixture(t)
			got, fingerprint, navigation := f.run(t, persona)
			if navigation != accessNavigation[persona] {
				t.Errorf("sidebar for %s:\n got %s\nwant %s", persona, navigation, accessNavigation[persona])
			}
			for key, value := range got {
				row, ok := accessMatrix[key]
				if !ok {
					t.Errorf("%s is not in accessMatrix: classify who may use it", key)
					continue
				}
				want := strings.Fields(row)
				if len(want) == 1 {
					want = slices.Repeat(want, len(matrixPersonas))
				}
				if len(want) != len(matrixPersonas) {
					t.Fatalf("accessMatrix[%q] lists %d outcomes", key, len(want))
				}
				if value != want[i] {
					t.Errorf("%s as %s: got %s, want %s", key, persona, value, want[i])
				}
			}
			for key := range accessMatrix {
				if _, ok := got[key]; !ok {
					t.Errorf("accessMatrix lists %s, which the server no longer serves", key)
				}
			}
			if fingerprint != accessFingerprints[persona] {
				t.Errorf("state after %s's requests:\n got %s\nwant %s", persona, fingerprint, accessFingerprints[persona])
			}
		})
	}
}

// TestAccessMatrixDump records what every persona receives from every route
// (ACCESS_MATRIX_DUMP=<file>), the exploration behind accessMatrix.
func TestAccessMatrixDump(t *testing.T) {
	out := os.Getenv("ACCESS_MATRIX_DUMP")
	if out == "" {
		t.Skip("set ACCESS_MATRIX_DUMP=<file> to record the observed access matrix")
	}
	var b strings.Builder
	for _, persona := range matrixPersonas {
		got, fingerprint, navigation := newAccessFixture(t).run(t, persona)
		fmt.Fprintf(&b, "%s\tNAVIGATION\t%s\n", persona, navigation)
		keys := make([]string, 0, len(got))
		for key := range got {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Fprintf(&b, "%s\t%s\t%s\n", persona, key, got[key])
		}
		fmt.Fprintf(&b, "%s\tFINGERPRINT\t%s\n", persona, fingerprint)
	}
	if err := os.WriteFile(out, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

type accessTarget struct{ name, path string }

// targets turns a route pattern into concrete paths: repository routes once
// for the granted repository A and once for repository B.
func (f *accessFixture) targets(pattern string) []accessTarget {
	fill := func(repo, pr string) string {
		p := pattern
		for param, value := range map[string]string{
			"{repoID}": f.public[repo], "{number}": f.public[pr], "{workspaceID}": f.public["workspace"], "{workspace}": f.public["workspace"],
			"{userID}": f.public["member"], "{invitationID}": f.public["invitation"], "{id}": f.public["destination"], "{token}": f.token,
			"{installationID}": f.public["installation"], "{finding}": "1", "{position}": "1",
			"{kind}": "privacy", "{file}": "dockerfile", "*": "matrix",
		} {
			p = strings.ReplaceAll(p, param, value)
		}
		return p
	}
	if strings.HasSuffix(pattern, "/setup/check") {
		return []accessTarget{{"A", fill("repoA", "prA") + "?pr=" + f.public["prA"]}, {"B", fill("repoB", "prB") + "?pr=" + f.public["prB"]}}
	}
	if strings.Contains(pattern, "{repoID}") {
		return []accessTarget{{"A", fill("repoA", "prA")}, {"B", fill("repoB", "prB")}}
	}
	return []accessTarget{{"-", fill("repoA", "prA")}}
}

// accessMatrix is what each persona receives from each route the server
// serves, keyed "METHOD pattern target" (A: the repository granted to the
// Developers and the Reviewer; B: a repository only Owners and Admins see).
// Values list the personas in matrixPersonas order: anonymous, owner, admin,
// dev, devnosource, reviewer, removed, outsider; one value stands for all.
// L and W redirect to GitHub sign-in and the workspace chooser, ">path" is
// See Other with public IDs as *. Builds on B answer 409 because the fake
// GitHub does not serve repository B: GitHub refusing access fails closed.
// See docs/28-shared-workspaces.md#roles and docs/30-reviews.md#access.
var accessMatrix = map[string]string{
	"GET / -":                 "200",
	"GET /account -":          "L 200 200 200 200 200 200 200",
	"POST /account/delete -":  "403 409 409 409 409 409 409 409",
	"GET /account/deletion -": "404",
	"POST /account/installations/{installationID}/disconnect -":            "403 409 409 404 404 404 404 404",
	"GET /account/operations/{id} -":                                       "L 404 404 404 404 404 404 404",
	"POST /account/repositories/{repoID}/disconnect A":                     "403 409 409 404 404 404 404 404",
	"POST /account/repositories/{repoID}/disconnect B":                     "403 409 409 404 404 404 404 404",
	"POST /account/workspaces/{workspaceID}/delete -":                      "403 409 404 404 404 404 404 404",
	"GET /auth/github/callback -":                                          "400",
	"POST /auth/github/link -":                                             "403 >https://github.com/login/oauth/authorize >https://github.com/login/oauth/authorize >https://github.com/login/oauth/authorize >https://github.com/login/oauth/authorize >https://github.com/login/oauth/authorize >https://github.com/login/oauth/authorize >https://github.com/login/oauth/authorize",
	"GET /auth/github/login -":                                             ">https://github.com/login/oauth/authorize",
	"GET /auth/google/callback -":                                          "404",
	"POST /auth/google/link -":                                             "403 404 404 404 404 404 404 404",
	"GET /auth/google/login -":                                             "404",
	"GET /auth/login -":                                                    "200",
	"GET /auth/logout -":                                                   "L >/dashboard >/dashboard >/dashboard >/dashboard >/dashboard >/dashboard >/dashboard",
	"POST /auth/logout -":                                                  "403 >/ >/ >/ >/ >/ >/ >/",
	"POST /auth/sessions/revoke -":                                         "403 >/protected >/protected >/protected >/protected >/protected >/protected >/protected",
	"GET /avatars/{userID} -":                                              "L 404 404 404 404 404 404 404",
	"GET /builds -":                                                        "L 200 200 200 200 >/previews W 200",
	"GET /dashboard -":                                                     "L 200 200 200 200 >/previews W 200",
	"GET /developer-tools -":                                               "L 200 200 404 404 >/previews 404 200",
	"GET /early-access -":                                                  "L 200 200 200 200 >/previews 200 200",
	"GET /environments -":                                                  "L 301>/previews 301>/previews 301>/previews 301>/previews 301>/previews 301>/previews 301>/previews",
	"GET /faq -":                                                           ">/#faq 200 200 200 200 200 200 200",
	"CONNECT /favicon.ico -":                                               "421",
	"DELETE /favicon.ico -":                                                "200",
	"GET /favicon.ico -":                                                   "200",
	"HEAD /favicon.ico -":                                                  "200",
	"OPTIONS /favicon.ico -":                                               "200",
	"PATCH /favicon.ico -":                                                 "200",
	"POST /favicon.ico -":                                                  "200",
	"PUT /favicon.ico -":                                                   "200",
	"QUERY /favicon.ico -":                                                 "200",
	"TRACE /favicon.ico -":                                                 "200",
	"GET /help -":                                                          "200",
	"POST /help/requests -":                                                "403 400 400 400 400 400 400 400",
	"GET /help/requests/{id} -":                                            "L 404 404 404 404 404 404 404",
	"GET /installations/authorize/callback -":                              "L 400 400 400 400 400 400 400",
	"GET /installations/callback -":                                        "L 400 400 400 400 400 400 400",
	"POST /installations/start -":                                          "403 >https://github.com/apps/previa-e2e/installations/new >https://github.com/apps/previa-e2e/installations/new 404 404 404 W >https://github.com/apps/previa-e2e/installations/new",
	"GET /open-source-tests -":                                             "200",
	"GET /operator/early-access -":                                         "L 404 404 404 404 404 404 404",
	"POST /operator/early-access/{workspace}/approve -":                    "403 404 404 404 404 404 404 404",
	"POST /operator/incidents -":                                           "403 404 404 404 404 404 404 404",
	"GET /operator/support -":                                              "L 404 404 404 404 404 404 404",
	"GET /operator/support/{id} -":                                         "L 404 404 404 404 404 404 404",
	"POST /operator/support/{id}/resolve -":                                "403 404 404 404 404 404 404 404",
	"GET /policies/{kind} -":                                               "200",
	"GET /previews -":                                                      "L 200 200 200 200 200 W 200",
	"GET /previews/access -":                                               "L 404 404 404 404 404 404 404",
	"CONNECT /previews/auth -":                                             "421",
	"DELETE /previews/auth -":                                              "403",
	"GET /previews/auth -":                                                 "403",
	"HEAD /previews/auth -":                                                "403",
	"OPTIONS /previews/auth -":                                             "403",
	"PATCH /previews/auth -":                                               "403",
	"POST /previews/auth -":                                                "403",
	"PUT /previews/auth -":                                                 "403",
	"QUERY /previews/auth -":                                               "403",
	"TRACE /previews/auth -":                                               "403",
	"GET /previews/unavailable -":                                          "404",
	"GET /previews/wake/* -":                                               "404",
	"POST /previews/wake/* -":                                              "404",
	"GET /protected -":                                                     "L 200 200 200 200 200 200 200",
	"POST /protected/profile -":                                            "403 409 409 409 409 409 409 409",
	"POST /protected/profile/avatar -":                                     "403 400 400 400 400 400 400 400",
	"POST /protected/profile/avatar/remove -":                              "403 >/protected >/protected >/protected >/protected >/protected >/protected >/protected",
	"GET /repositories -":                                                  "L 200 200 200 200 >/previews W 200",
	"GET /repositories/{repoID} A":                                         "L 200 200 200 200 >/previews 404 404",
	"GET /repositories/{repoID} B":                                         "L 200 200 404 404 404 404 404",
	"GET /repositories/{repoID}/events A":                                  "L 200 200 200 200 200 404 404",
	"GET /repositories/{repoID}/events B":                                  "L 200 200 404 404 404 404 404",
	"GET /repositories/{repoID}/outbound A":                                "L 200 200 404 404 >/previews 404 404",
	"POST /repositories/{repoID}/outbound A":                               "403 >/repositories/*/outbound >/repositories/*/outbound 404 404 404 404 404",
	"GET /repositories/{repoID}/outbound B":                                "L 200 200 404 404 404 404 404",
	"POST /repositories/{repoID}/outbound B":                               "403 >/repositories/*/outbound >/repositories/*/outbound 404 404 404 404 404",
	"POST /repositories/{repoID}/outbound/{id}/remove A":                   "403 >/repositories/*/outbound >/repositories/*/outbound 404 404 404 404 404",
	"POST /repositories/{repoID}/outbound/{id}/remove B":                   "403 404 404 404 404 404 404 404",
	"POST /repositories/{repoID}/policy A":                                 "403 >/repositories/* >/repositories/* 404 404 404 404 404",
	"POST /repositories/{repoID}/policy B":                                 "403 >/repositories/* >/repositories/* 404 404 404 404 404",
	"POST /repositories/{repoID}/preview-access A":                         "403 >/repositories/* >/repositories/* 404 404 404 404 404",
	"POST /repositories/{repoID}/preview-access B":                         "403 >/repositories/* >/repositories/* 404 404 404 404 404",
	"GET /repositories/{repoID}/pull-requests/{number} A":                  "L 200 200 200 200 >/repositories/*/pull-requests/*/review 404 404",
	"GET /repositories/{repoID}/pull-requests/{number} B":                  "L 200 200 404 404 404 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/build A":           "403 >/repositories/* >/repositories/* >/repositories/* 404 404 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/build B":           "403 409 409 404 404 404 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/failure/dismiss A": "403 400 400 400 404 404 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/failure/dismiss B": "403 400 400 404 404 404 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/failure/restore A": "403 >/repositories/*/pull-requests/* >/repositories/*/pull-requests/* >/repositories/*/pull-requests/* 404 404 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/failure/restore B": "403 >/repositories/*/pull-requests/* >/repositories/*/pull-requests/* 404 404 404 404 404",
	"GET /repositories/{repoID}/pull-requests/{number}/logs A":             "L 200 200 200 404 >/previews 404 404",
	"GET /repositories/{repoID}/pull-requests/{number}/logs B":             "L 200 200 404 404 404 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/logs/delete A":     "403 400 400 400 404 404 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/logs/delete B":     "403 400 400 404 404 404 404 404",
	"GET /repositories/{repoID}/pull-requests/{number}/logs/download A":    "L 404 404 404 404 >/previews 404 404",
	"GET /repositories/{repoID}/pull-requests/{number}/logs/download B":    "L 404 404 404 404 404 404 404",
	"GET /repositories/{repoID}/pull-requests/{number}/logs/events A":      "L 200 200 200 404 >/previews 404 404",
	"GET /repositories/{repoID}/pull-requests/{number}/logs/events B":      "L 200 200 404 404 404 404 404",
	"GET /repositories/{repoID}/pull-requests/{number}/logs/recent A":      "L 200 200 200 404 >/previews 404 404",
	"GET /repositories/{repoID}/pull-requests/{number}/logs/recent B":      "L 200 200 404 404 404 404 404",
	"GET /repositories/{repoID}/pull-requests/{number}/open A":             "L >https://matrix-preview.preview.example.test >https://matrix-preview.preview.example.test >https://matrix-preview.preview.example.test >https://matrix-preview.preview.example.test >https://matrix-preview.preview.example.test 404 404",
	"GET /repositories/{repoID}/pull-requests/{number}/open B":             "L >/repositories/*/pull-requests/* >/repositories/*/pull-requests/* 404 404 404 404 404",
	"GET /repositories/{repoID}/pull-requests/{number}/output A":           "L 200 200 200 404 >/previews 404 404",
	"GET /repositories/{repoID}/pull-requests/{number}/output B":           "L 200 200 404 404 404 404 404",
	"GET /repositories/{repoID}/pull-requests/{number}/output/events A":    "L 200 200 200 404 >/previews 404 404",
	"GET /repositories/{repoID}/pull-requests/{number}/output/events B":    "L 404 404 404 404 404 404 404",
	"GET /repositories/{repoID}/pull-requests/{number}/review A":           "L 200 200 200 200 200 404 404",
	"GET /repositories/{repoID}/pull-requests/{number}/review B":           "L 200 200 404 404 404 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/review/findings A": "403 >/repositories/*/pull-requests/*/review >/repositories/*/pull-requests/*/review >/repositories/*/pull-requests/*/review >/repositories/*/pull-requests/*/review >/repositories/*/pull-requests/*/review 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/review/findings B": "403 422 422 404 404 404 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/review/findings/{finding}/messages/{position}/edit A":   "403 >/repositories/*/pull-requests/*/review 404 404 404 404 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/review/findings/{finding}/messages/{position}/edit B":   "403 404 404 404 404 404 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/review/findings/{finding}/messages/{position}/remove A": "403 >/repositories/*/pull-requests/*/review >/repositories/*/pull-requests/*/review >/repositories/*/pull-requests/*/review >/repositories/*/pull-requests/*/review 404 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/review/findings/{finding}/messages/{position}/remove B": "403 404 404 404 404 404 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/review/findings/{finding}/remove A":                     "403 >/repositories/*/pull-requests/*/review >/repositories/*/pull-requests/*/review >/repositories/*/pull-requests/*/review >/repositories/*/pull-requests/*/review 404 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/review/findings/{finding}/remove B":                     "403 404 404 404 404 404 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/review/findings/{finding}/reopen A":                     "403 >/repositories/*/pull-requests/*/review >/repositories/*/pull-requests/*/review >/repositories/*/pull-requests/*/review >/repositories/*/pull-requests/*/review 404 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/review/findings/{finding}/reopen B":                     "403 404 404 404 404 404 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/review/findings/{finding}/replies A":                    "403 >/repositories/*/pull-requests/*/review >/repositories/*/pull-requests/*/review >/repositories/*/pull-requests/*/review >/repositories/*/pull-requests/*/review >/repositories/*/pull-requests/*/review 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/review/findings/{finding}/replies B":                    "403 404 404 404 404 404 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/review/findings/{finding}/resolve A":                    "403 >/repositories/*/pull-requests/*/review >/repositories/*/pull-requests/*/review >/repositories/*/pull-requests/*/review >/repositories/*/pull-requests/*/review 404 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/review/findings/{finding}/resolve B":                    "403 404 404 404 404 404 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/review/instructions A":                                  "403 >/repositories/*/pull-requests/*/review >/repositories/*/pull-requests/*/review >/repositories/*/pull-requests/*/review >/repositories/*/pull-requests/*/review 404 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/review/instructions B":                                  "403 >/repositories/*/pull-requests/*/review >/repositories/*/pull-requests/*/review 404 404 404 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/review/test-access A":                                   "403 >/repositories/*/pull-requests/*/review >/repositories/*/pull-requests/*/review 404 404 404 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/review/test-access B":                                   "403 >/repositories/*/pull-requests/*/review >/repositories/*/pull-requests/*/review 404 404 404 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/review/test-access/reveal A":                            "403 200 200 200 200 200 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/review/test-access/reveal B":                            "403 200 200 404 404 404 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/teardown A":                                             "403 >/repositories/* >/repositories/* >/repositories/* 404 404 404 404",
	"POST /repositories/{repoID}/pull-requests/{number}/teardown B":                                             "403 >/repositories/* >/repositories/* 404 404 404 404 404",
	"POST /repositories/{repoID}/refresh A":                                                                     "403 >/repositories/* >/repositories/* >/repositories/* 404 404 404 404",
	"POST /repositories/{repoID}/refresh B":                                                                     "403 >/repositories/* >/repositories/* 404 404 404 404 404",
	"GET /repositories/{repoID}/secrets A":                                                                      "L 200 200 404 404 >/previews 404 404",
	"POST /repositories/{repoID}/secrets A":                                                                     "403 >/repositories/*/secrets >/repositories/*/secrets 404 404 404 404 404",
	"GET /repositories/{repoID}/secrets B":                                                                      "L 200 200 404 404 404 404 404",
	"POST /repositories/{repoID}/secrets B":                                                                     "403 >/repositories/*/secrets >/repositories/*/secrets 404 404 404 404 404",
	"POST /repositories/{repoID}/secrets/build A":                                                               "403 >/repositories/*/secrets >/repositories/*/secrets 404 404 404 404 404",
	"POST /repositories/{repoID}/secrets/build B":                                                               "403 >/repositories/*/secrets >/repositories/*/secrets 404 404 404 404 404",
	"POST /repositories/{repoID}/secrets/delete A":                                                              "403 >/repositories/*/secrets >/repositories/*/secrets 404 404 404 404 404",
	"POST /repositories/{repoID}/secrets/delete B":                                                              "403 >/repositories/*/secrets >/repositories/*/secrets 404 404 404 404 404",
	"GET /repositories/{repoID}/setup A":                                                                        "L 200 200 200 200 >/previews 404 404",
	"GET /repositories/{repoID}/setup B":                                                                        "L 200 200 404 404 404 404 404",
	"POST /repositories/{repoID}/setup/check A":                                                                 "403 >/repositories/*/setup >/repositories/*/setup >/repositories/*/setup 404 404 404 404",
	"POST /repositories/{repoID}/setup/check B":                                                                 "403 >/repositories/*/setup >/repositories/*/setup 404 404 404 404 404",
	"GET /repositories/{repoID}/test-data A":                                                                    "L 200 200 200 200 >/previews 404 404",
	"GET /repositories/{repoID}/test-data B":                                                                    "L 200 200 404 404 404 404 404",
	"POST /repositories/{repoID}/test-data/delete A":                                                            "403 >/repositories/*/test-data >/repositories/*/test-data 404 404 404 404 404",
	"POST /repositories/{repoID}/test-data/delete B":                                                            "403 >/repositories/*/test-data >/repositories/*/test-data 404 404 404 404 404",
	"POST /repositories/{repoID}/test-data/off A":                                                               "403 >/repositories/*/test-data >/repositories/*/test-data 404 404 404 404 404",
	"POST /repositories/{repoID}/test-data/off B":                                                               "403 >/repositories/*/test-data >/repositories/*/test-data 404 404 404 404 404",
	"POST /repositories/{repoID}/test-data/refresh A":                                                           "403 >/repositories/*/test-data >/repositories/*/test-data >/repositories/*/test-data 404 404 404 404",
	"POST /repositories/{repoID}/test-data/refresh B":                                                           "403 >/repositories/*/test-data >/repositories/*/test-data 404 404 404 404 404",
	"GET /repositories/{repoID}/test-data/refreshes/{number}/logs A":                                            "L 404 404 404 404 >/previews 404 404",
	"GET /repositories/{repoID}/test-data/refreshes/{number}/logs B":                                            "L 404 404 404 404 404 404 404",
	"GET /repositories/{repoID}/test-data/refreshes/{number}/logs/download A":                                   "L 404 404 404 404 >/previews 404 404",
	"GET /repositories/{repoID}/test-data/refreshes/{number}/logs/download B":                                   "L 404 404 404 404 404 404 404",
	"POST /repositories/{repoID}/test-data/settings A":                                                          "403 >/repositories/*/test-data >/repositories/*/test-data 404 404 404 404 404",
	"POST /repositories/{repoID}/test-data/settings B":                                                          "403 >/repositories/*/test-data >/repositories/*/test-data 404 404 404 404 404",
	"GET /starter-kits -":                  "L 200 200 200 200 >/previews 200 200",
	"POST /starter-kits/check -":           "403 200 200 200 200 200 200 200",
	"POST /starter-kits/download/{file} -": "403 200 200 200 200 200 200 200",
	"CONNECT /static/* -":                  "421",
	"DELETE /static/* -":                   "404",
	"GET /static/* -":                      "404",
	"HEAD /static/* -":                     "404",
	"OPTIONS /static/* -":                  "404",
	"PATCH /static/* -":                    "404",
	"POST /static/* -":                     "404",
	"PUT /static/* -":                      "404",
	"QUERY /static/* -":                    "404",
	"TRACE /static/* -":                    "404",
	"GET /status -":                        "200",
	"POST /webhooks/github -":              "401",
	"POST /workspace-invitations/received/{invitationID} -":       "403",
	"GET /workspace-invitations/{token} -":                        "L 403 403 403 403 403 403 403",
	"POST /workspace-invitations/{token} -":                       "403",
	"GET /workspaces -":                                           "L 200 200 200 200 200 200 200",
	"POST /workspaces -":                                          "403 400 400 400 400 400 400 400",
	"GET /workspaces/{workspaceID} -":                             "L 200 200 200 200 200 404 404",
	"POST /workspaces/{workspaceID}/delete -":                     "403 409 404 404 404 404 404 404",
	"POST /workspaces/{workspaceID}/invitations -":                "403 200 200 404 404 404 404 404",
	"POST /workspaces/{workspaceID}/invitations/{invitationID} -": "403 >/workspaces/*/members >/workspaces/*/members 404 404 404 404 404",
	"GET /workspaces/{workspaceID}/logo -":                        "L 404 404 404 404 404 404 404",
	"POST /workspaces/{workspaceID}/logo -":                       "403 400 400 404 404 404 404 404",
	"POST /workspaces/{workspaceID}/logo/remove -":                "403 >/workspaces/*/settings >/workspaces/*/settings 404 404 404 404 404",
	"GET /workspaces/{workspaceID}/members -":                     "L 200 200 200 200 200 404 404",
	"GET /workspaces/{workspaceID}/members/{userID} -":            "L 200 200 200 200 200 404 404",
	"POST /workspaces/{workspaceID}/members/{userID} -":           "403 >/workspaces/*/members >/workspaces/*/members 404 404 404 404 404",
	"POST /workspaces/{workspaceID}/ownership -":                  "403 >/workspaces/*/settings 400 404 404 404 404 404",
	"GET /workspaces/{workspaceID}/previews -":                    "L >/previews >/previews >/previews >/previews >/previews 404 404",
	"POST /workspaces/{workspaceID}/select -":                     "403 >/dashboard >/dashboard >/dashboard >/dashboard >/dashboard 404 404",
	"GET /workspaces/{workspaceID}/settings -":                    "L 200 200 404 404 404 404 404",
	"POST /workspaces/{workspaceID}/settings -":                   "403 >/workspaces/*/settings >/workspaces/*/settings 404 404 404 404 404",
	"GET /workspaces/{workspaceID}/setup -":                       "L 200 200 404 404 404 404 404",
	"GET /workspaces/{workspaceID}/usage -":                       "L 200 200 200 200 404 404 404",
}

// accessUnchanged is the seeded state; refused requests must leave it so.
const accessUnchanged = "policy=manual/false,manual/false builds=0 teardown=0 destinations=octo/app:cache.example.com secrets=0 devGrants=1 workspace='owner''s workspace' invitations=admin/accepted,developer/accepted,developer/accepted,reviewer/accepted,developer/accepted,reviewer/pending transfers=0 findings=1/open instructions=0 testLogins=A@1"

// accessFingerprints is the state each persona's requests leave behind.
var accessFingerprints = map[string]string{
	personaAnonymous:   accessUnchanged,
	personaOwner:       "policy=automatic/true,automatic/true builds=1 teardown=2 destinations=octo/app:db.example.com,octo/other:db.example.com secrets=2 devGrants=2 workspace='Matrix team' invitations=admin/accepted,developer/accepted,developer/accepted,reviewer/accepted,developer/accepted,reviewer/revoked,reviewer/pending transfers=1 findings=1/removed,2/open instructions=2 testLogins=A@2,B@1",
	personaAdmin:       "policy=automatic/true,automatic/true builds=1 teardown=2 destinations=octo/app:db.example.com,octo/other:db.example.com secrets=2 devGrants=2 workspace='Matrix team' invitations=admin/accepted,developer/accepted,developer/accepted,reviewer/accepted,developer/accepted,reviewer/revoked,reviewer/pending transfers=0 findings=1/removed,2/open instructions=2 testLogins=A@2,B@1",
	personaDeveloper:   "policy=manual/false,manual/false builds=1 teardown=2 destinations=octo/app:cache.example.com secrets=0 devGrants=1 workspace='owner''s workspace' invitations=admin/accepted,developer/accepted,developer/accepted,reviewer/accepted,developer/accepted,reviewer/pending transfers=0 findings=1/removed,2/open instructions=1 testLogins=A@1",
	personaDevNoSource: "policy=manual/false,manual/false builds=0 teardown=0 destinations=octo/app:cache.example.com secrets=0 devGrants=1 workspace='owner''s workspace' invitations=admin/accepted,developer/accepted,developer/accepted,reviewer/accepted,developer/accepted,reviewer/pending transfers=0 findings=1/removed,2/open instructions=1 testLogins=A@1",
	personaReviewer:    "policy=manual/false,manual/false builds=0 teardown=0 destinations=octo/app:cache.example.com secrets=0 devGrants=1 workspace='owner''s workspace' invitations=admin/accepted,developer/accepted,developer/accepted,reviewer/accepted,developer/accepted,reviewer/pending transfers=0 findings=1/open,2/open instructions=0 testLogins=A@1",
	personaRemoved:     accessUnchanged,
	personaOutsider:    accessUnchanged,
}

// accessNavigation is the sidebar each persona is offered: a Reviewer sees
// only previews, members and help; Developers no settings or tools.
var accessNavigation = map[string]string{
	personaAnonymous:   "",
	personaOwner:       "/ /workspaces /dashboard /repositories /builds /previews /starter-kits /workspaces/*/members /workspaces/*/usage /workspaces/*/settings /developer-tools /faq /help /account /early-access",
	personaAdmin:       "/ /workspaces /dashboard /repositories /builds /previews /starter-kits /workspaces/*/members /workspaces/*/usage /workspaces/*/settings /developer-tools /faq /help /account /early-access",
	personaDeveloper:   "/ /workspaces /dashboard /repositories /builds /previews /starter-kits /workspaces/*/members /workspaces/*/usage /faq /help /account /early-access",
	personaDevNoSource: "/ /workspaces /dashboard /repositories /builds /previews /starter-kits /workspaces/*/members /workspaces/*/usage /faq /help /account /early-access",
	personaReviewer:    "/ /workspaces /previews /workspaces/*/members /faq /help /account",
	personaRemoved:     "/ /workspaces /faq /help /account",
	personaOutsider:    "/ /workspaces /dashboard /repositories /builds /previews /starter-kits /workspaces/*/members /workspaces/*/usage /workspaces/*/settings /developer-tools /faq /help /account /early-access",
}

// usability crawls every page persona can open and lists what it offers them
// that they cannot use: disabled controls, forms the server refuses them, and
// links that are refused or bounce them elsewhere. People see only what they
// can act on; temporary blocks are explained in text, not as dead controls.
func (f *accessFixture) usability(t *testing.T, persona string, got map[string]string) []string {
	t.Helper()
	finder, ok := f.router.(interface {
		Find(*chi.Context, string, string) string
	})
	if !ok {
		t.Fatalf("router %T cannot resolve routes", f.router)
	}
	target := func(path string) string {
		switch {
		case strings.Contains(path, f.public["repoA"]):
			return "A"
		case strings.Contains(path, f.public["repoB"]):
			return "B"
		}
		return "-"
	}
	resolve := func(method, path string) (string, bool) {
		pattern := finder.Find(chi.NewRouteContext(), method, path)
		if pattern == "" {
			return "", false
		}
		outcome, ok := got[method+" "+pattern+" "+target(path)]
		return outcome, ok
	}
	var problems []string
	seen := map[string]bool{}
	report := func(page, problem string) {
		if line := page + ": " + problem; !seen[line] {
			seen[line] = true
			problems = append(problems, line)
		}
	}
	for _, route := range matrixRoutes(t, f.router) {
		method, pattern, _ := strings.Cut(route, " ")
		if method != http.MethodGet || strings.HasSuffix(pattern, "/events") || strings.Contains(pattern, "/download") || strings.HasSuffix(pattern, "/logo") {
			continue
		}
		for _, page := range f.targets(pattern) {
			if got[route+" "+page.name] != "200" {
				continue
			}
			w := f.send(t, persona, http.MethodGet, page.path, nil)
			if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
				continue
			}
			doc, err := html.Parse(w.Body)
			if err != nil {
				t.Fatal(err)
			}
			name := route + " " + page.name
			for n := range doc.Descendants() {
				if n.Type != html.ElementNode {
					continue
				}
				attr := func(key string) (string, bool) {
					for _, a := range n.Attr {
						if a.Key == key {
							return a.Val, true
						}
					}
					return "", false
				}
				switch n.Data {
				case "button", "input", "select", "textarea", "fieldset":
					if _, disabled := attr("disabled"); disabled {
						id, _ := attr("name")
						report(name, fmt.Sprintf("disabled <%s> %q %q", n.Data, id, strings.Join(strings.Fields(textOf(n)), " ")))
					}
				case "form":
					formMethod, _ := attr("method")
					action, _ := attr("action")
					if !strings.EqualFold(formMethod, "post") || !strings.HasPrefix(action, "/") {
						continue
					}
					path, _, _ := strings.Cut(action, "?")
					if _, perFinding := attr("data-remove-form"); perFinding {
						continue // the remove dialog's action is set for each finding by script
					}
					if !f.matrixIDs(path) || (persona != personaAnonymous && strings.Contains(path, f.publicID(t, "users", f.users[persona]))) {
						continue // another workspace's resource, or the person's own (Leave)
					}
					if outcome, ok := resolve(http.MethodPost, path); !ok {
						report(name, "form to unknown "+action)
					} else if refused(outcome) {
						report(name, "form to "+publicIDPattern.ReplaceAllString(path, "*")+" answers "+outcome)
					}
				case "a":
					href, _ := attr("href")
					if !strings.HasPrefix(href, "/") || strings.HasPrefix(href, "//") {
						continue
					}
					path, _, _ := strings.Cut(href, "?")
					path, _, _ = strings.Cut(path, "#")
					if path == "" || path == "/" {
						continue
					}
					// Following a link has no side effects: check it for real.
					outcome := outcome(f.send(t, persona, http.MethodGet, path, nil))
					if refused(outcome) || outcome == "W" || (outcome == ">/previews" && path != "/previews") {
						report(name, "link to "+publicIDPattern.ReplaceAllString(path, "*")+" answers "+outcome)
					}
				}
			}
		}
	}
	sort.Strings(problems)
	return problems
}

// matrixIDs reports whether every public ID in path is one the matrix
// requests used, so its recorded answer applies to this persona.
func (f *accessFixture) matrixIDs(path string) bool {
	known := map[string]bool{}
	for _, name := range []string{"workspace", "repoA", "repoB", "prA", "prB", "member", "invitation", "destination", "installation"} {
		known[f.public[name]] = true
	}
	for _, id := range publicIDPattern.FindAllString(path, -1) {
		if !known[id] {
			return false
		}
	}
	return true
}

func refused(outcome string) bool {
	return outcome == "403" || outcome == "404" || outcome == "L"
}

func textOf(n *html.Node) string {
	var b strings.Builder
	for d := range n.Descendants() {
		if d.Type == html.TextNode {
			b.WriteString(d.Data)
		}
	}
	return b.String()
}

// usabilityProblems seeds a fresh fixture and crawls it as persona. Pages are
// read first, so the crawl sees the seeded state; mutations are judged by the
// reviewed accessMatrix rather than sent.
func usabilityProblems(t *testing.T, persona string) []string {
	t.Helper()
	f := newAccessFixture(t)
	got := map[string]string{}
	for _, route := range matrixRoutes(t, f.router) {
		method, pattern, _ := strings.Cut(route, " ")
		if method != http.MethodGet {
			continue
		}
		for _, target := range f.targets(pattern) {
			got[route+" "+target.name] = outcome(f.send(t, persona, method, target.path, nil))
		}
	}
	index := slices.Index(matrixPersonas, persona)
	for key, value := range accessMatrix {
		if strings.HasPrefix(key, "POST ") {
			values := strings.Fields(value)
			got[key] = values[min(index, len(values)-1)]
		}
	}
	return f.usability(t, persona, got)
}

// TestPagesOfferOnlyWhatPeopleCanUse crawls every page each persona can open:
// none may offer a disabled control, a form the server refuses them or a link
// that is refused or bounces them. Roles hide what they can never use; a
// temporary block is explained in text.
func TestPagesOfferOnlyWhatPeopleCanUse(t *testing.T) {
	for _, persona := range matrixPersonas {
		t.Run(persona, func(t *testing.T) {
			t.Parallel()
			for _, problem := range usabilityProblems(t, persona) {
				t.Error(problem)
			}
		})
	}
}

// TestUsabilityDump records, per persona, every control or link a page offers
// that the person cannot use (USABILITY_DUMP=<file>).
func TestUsabilityDump(t *testing.T) {
	out := os.Getenv("USABILITY_DUMP")
	if out == "" {
		t.Skip("set USABILITY_DUMP=<file> to record unusable controls")
	}
	var b strings.Builder
	for _, persona := range matrixPersonas {
		for _, p := range usabilityProblems(t, persona) {
			fmt.Fprintf(&b, "%s\t%s\n", persona, p)
		}
	}
	if err := os.WriteFile(out, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}
