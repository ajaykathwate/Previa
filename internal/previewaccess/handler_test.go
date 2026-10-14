package previewaccess

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
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/ajaykathwatee/previa/internal/platform/workers"
	"github.com/ajaykathwatee/previa/internal/routing"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"
)

func TestSignedValuesAreBoundToKindHostAndTime(t *testing.T) {
	s := newSigner("secret")
	now := time.Now()
	value := s.sign("cookie", claims{Host: "a.preview.test", User: 7, Expires: now.Add(time.Hour).Unix()})
	if c, ok := s.verify("cookie", value, "a.preview.test", now); !ok || c.User != 7 {
		t.Fatal("a valid cookie was refused")
	}
	for name, check := range map[string]func() bool{
		"another kind": func() bool { _, ok := s.verify("token", value, "a.preview.test", now); return ok },
		"another host": func() bool { _, ok := s.verify("cookie", value, "b.preview.test", now); return ok },
		"expired":      func() bool { _, ok := s.verify("cookie", value, "a.preview.test", now.Add(2*time.Hour)); return ok },
		"another key":  func() bool { _, ok := newSigner("other").verify("cookie", value, "a.preview.test", now); return ok },
		"tampered":     func() bool { _, ok := s.verify("cookie", "x"+value, "a.preview.test", now); return ok },
	} {
		if check() {
			t.Errorf("%s was accepted", name)
		}
	}
}

func isolatedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for private preview database tests")
	}
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("previewaccess_test_%d", time.Now().UnixNano())
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
	// The handler must not read worker metadata using a second connection
	// while its transaction holds the only one.
	cfg.MaxConns = 1
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

type userKey struct{}

type session struct{ names map[int64]string }

func (s session) UserIDs(ctx context.Context) (int64, int64, bool) {
	id, ok := ctx.Value(userKey{}).(int64)
	return id, 0, ok && id > 0
}

func (s session) Username(ctx context.Context) (string, bool) {
	id, _ := ctx.Value(userKey{}).(int64)
	name, ok := s.names[id]
	return name, ok
}

type github map[string]string

func (g github) RepositoryUserPermission(_ context.Context, _ int64, fullName, login string, _ int64) (string, error) {
	if fullName != "owner/app" {
		return "", fmt.Errorf("unexpected repository %s", fullName)
	}
	if p, ok := g[login]; ok {
		return p, nil
	}
	return "none", nil
}

type jobs struct{ syncs int }

type capabilities struct {
	ready bool
	err   error
}

func (c capabilities) Current(context.Context) (workers.Capabilities, error) {
	return workers.Capabilities{PrivatePreviews: c.ready}, c.err
}

func TestAccessSettingUsesRealQueue(t *testing.T) {
	pool := isolatedPool(t)
	ctx := t.Context()
	m, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		"INSERT INTO users(github_id,username) VALUES(1,'owner')",
		"INSERT INTO installations(github_installation_id,owner_user_id) VALUES(99,1)",
		"INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES(1,100,'owner/app')",
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	producer, err := queue.NewProducer(pool, logger)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(pool, session{}, nil, producer, "secret", "preview.test", "https://previa.test", logger).WithCapabilities(capabilities{ready: true})
	r := chi.NewRouter()
	h.Mount(r, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), userKey{}, int64(1))))
		})
	})
	set := func(private bool, status int) {
		t.Helper()
		req := httptest.NewRequest("POST", "/repositories/1/preview-access", strings.NewReader(url.Values{"private": {strconv.FormatBool(private)}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != status {
			t.Fatalf("toggle: %d: %s", w.Code, w.Body)
		}
	}
	check := func(private bool, wantJobs int) int64 {
		t.Helper()
		var got bool
		var version int64
		var n int
		if err := pool.QueryRow(ctx, "SELECT private_previews FROM repos WHERE id=1").Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != private {
			t.Fatalf("private = %v, want %v", got, private)
		}
		if err := pool.QueryRow(ctx, "SELECT version FROM route_table WHERE singleton").Scan(&version); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM river_job").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != wantJobs {
			t.Fatalf("jobs = %d, want %d", n, wantJobs)
		}
		return version
	}
	initial := check(false, 0)
	h.capabilities = nil
	set(true, http.StatusSeeOther)
	if check(false, 0) != initial {
		t.Fatal("missing capabilities changed privacy or route version")
	}
	h.capabilities = workers.NewReader(pool)
	set(true, http.StatusSeeOther)
	if check(false, 0) != initial {
		t.Fatal("unconfigured workers changed privacy or route version")
	}
	h.capabilities = capabilities{err: errors.New("heartbeat unavailable")}
	set(true, http.StatusServiceUnavailable)
	if check(false, 0) != initial {
		t.Fatal("failed readiness check changed privacy or route version")
	}
	h.capabilities = capabilities{ready: true}
	set(true, http.StatusSeeOther)
	if version := check(true, 1); version != initial+1 {
		t.Fatal("route version did not advance")
	}
	if _, err := pool.Exec(ctx, "UPDATE river_job SET state='running', attempted_at=now()"); err != nil {
		t.Fatal(err)
	}
	// Turning private previews off remains an owner decision even without auth.
	h.capabilities = capabilities{}
	set(false, http.StatusSeeOther)
	version := check(false, 2)
	set(false, http.StatusSeeOther)
	if check(false, 2) != version {
		t.Fatal("unchanged setting advanced route version")
	}
	h.jobs = rejectedJobs{}
	h.capabilities = capabilities{ready: true}
	set(true, http.StatusServiceUnavailable)
	if check(false, 2) != version {
		t.Fatal("failed enqueue did not roll back route version")
	}
}

type rejectedJobs struct{}

func (rejectedJobs) InsertTx(_ context.Context, _ pgx.Tx, _ river.JobArgs, _ *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	return nil, errors.New("queue unavailable")
}

func (j *jobs) InsertTx(_ context.Context, _ pgx.Tx, args river.JobArgs, _ *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	if _, ok := args.(routing.SyncArgs); ok {
		j.syncs++
	}
	return &rivertype.JobInsertResult{}, nil
}

// A visitor to a private preview signs in, is checked against GitHub, and
// comes back with a host-only cookie; without one the router sends them to
// sign in. The owner switches the repository between private and public.
func TestPrivatePreviewRoundTrip(t *testing.T) {
	pool := isolatedPool(t)
	ctx := t.Context()
	var owner, reviewer, stranger, install, repo, pr int64
	scan := func(dst *int64, sql string, args ...any) {
		t.Helper()
		if err := pool.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	scan(&owner, `INSERT INTO users(github_id,username) VALUES (1,'owner') RETURNING id`)
	scan(&reviewer, `INSERT INTO users(github_id,username) VALUES (2,'reviewer') RETURNING id`)
	scan(&stranger, `INSERT INTO users(github_id,username) VALUES (3,'stranger') RETURNING id`)
	scan(&install, `INSERT INTO installations(github_installation_id,owner_user_id) VALUES (99,$1) RETURNING id`, owner)
	scan(&repo, `INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES ($1,100,'owner/app') RETURNING id`, install)
	scan(&pr, `INSERT INTO pull_requests(repo_id,number,head_sha) VALUES ($1,1,'sha') RETURNING id`, repo)
	if _, err := pool.Exec(ctx, `INSERT INTO environments(pull_request_id,subdomain,expires_at,status) VALUES ($1,'pr-1-x',now()+interval '1 day','running')`, pr); err != nil {
		t.Fatal(err)
	}
	queue := &jobs{}
	h := NewHandler(pool, session{names: map[int64]string{owner: "owner", reviewer: "reviewer", stranger: "stranger"}},
		github{"reviewer": "read"}, queue, "secret", "preview.test", "https://previa.test", slog.New(slog.NewTextHandler(io.Discard, nil))).WithCapabilities(capabilities{ready: true})
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			id, _ := strconv.ParseInt(req.Header.Get("X-User"), 10, 64)
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), userKey{}, id)))
		})
	})
	h.MountPublic(r)
	h.Mount(r, func(next http.Handler) http.Handler { return next })
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(method, path string, user int64, header map[string]string, form url.Values) *http.Response {
		t.Helper()
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req, _ := http.NewRequestWithContext(ctx, method, server.URL+path, body)
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		req.Header.Set("X-User", strconv.FormatInt(user, 10))
		for k, v := range header {
			req.Header.Set(k, v)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		return res
	}
	host := "pr-1-x.preview.test"
	check := func(uri, cookie string) *http.Response {
		t.Helper()
		headers := map[string]string{"X-Forwarded-Host": host, "X-Forwarded-Uri": uri}
		if cookie != "" {
			headers["Cookie"] = cookieName + "=" + cookie
		}
		return do(http.MethodGet, routing.AccessPath, 0, headers, nil)
	}

	// The owner makes the repository's previews private.
	if res := do(http.MethodPost, "/repositories/"+strconv.FormatInt(repo, 10)+"/preview-access", stranger, nil, url.Values{"private": {"true"}}); res.StatusCode != http.StatusNotFound {
		t.Fatalf("another user changed the setting: %d", res.StatusCode)
	}
	if res := do(http.MethodPost, "/repositories/"+strconv.FormatInt(repo, 10)+"/preview-access", owner, nil, url.Values{"private": {"true"}}); res.StatusCode != http.StatusSeeOther || queue.syncs != 1 {
		t.Fatalf("set private: %d, %d syncs", res.StatusCode, queue.syncs)
	}

	// No cookie: off to sign in, remembering where they were going.
	res := check("/dashboard?x=1", "")
	signIn, _ := url.Parse(res.Header.Get("Location"))
	if res.StatusCode != http.StatusFound || signIn.Host != "previa.test" || signIn.Path != accessPage || signIn.Query().Get("path") != "/dashboard?x=1" {
		t.Fatalf("no cookie: %d %s", res.StatusCode, signIn)
	}
	if res := do(http.MethodGet, signIn.RequestURI(), 0, nil, nil); res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(res.Header.Get("Location"), "/auth/github/login?next=") {
		t.Fatalf("anonymous: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if res := do(http.MethodGet, signIn.RequestURI(), stranger, nil, nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("a user who cannot see the repository: %d", res.StatusCode)
	}
	for _, user := range []int64{reviewer, owner} {
		res := do(http.MethodGet, signIn.RequestURI(), user, nil, nil)
		back, _ := url.Parse(res.Header.Get("Location"))
		if res.StatusCode != http.StatusSeeOther || back.Host != host || back.Path != returnPath {
			t.Fatalf("user %d: %d %s", user, res.StatusCode, back)
		}
		// The preview turns the token into a host-only cookie.
		res = check(back.RequestURI(), "")
		var cookie *http.Cookie
		for _, c := range res.Cookies() {
			if c.Name == cookieName {
				cookie = c
			}
		}
		if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/dashboard?x=1" || cookie == nil || !cookie.Secure || !cookie.HttpOnly || cookie.Domain != "" {
			t.Fatalf("token: %d %s %+v", res.StatusCode, res.Header.Get("Location"), cookie)
		}
		if res := check("/anything", cookie.Value); res.StatusCode != http.StatusOK {
			t.Fatalf("with the cookie: %d", res.StatusCode)
		}
		headers := map[string]string{"X-Forwarded-Host": "pr-2-y.preview.test", "X-Forwarded-Uri": "/", "Cookie": cookieName + "=" + cookie.Value}
		if res := do(http.MethodGet, routing.AccessPath, 0, headers, nil); res.StatusCode != http.StatusFound {
			t.Fatalf("another preview's cookie let a visitor in: %d", res.StatusCode)
		}
		if res := check(back.RequestURI()+"x", ""); res.StatusCode != http.StatusForbidden {
			t.Fatalf("a tampered token: %d", res.StatusCode)
		}
	}
	if res := do(http.MethodGet, routing.AccessPath, 0, map[string]string{"X-Forwarded-Host": "evil.test"}, nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("another host: %d", res.StatusCode)
	}

	// Public again: the access page sends anyone straight to the preview.
	if res := do(http.MethodPost, "/repositories/"+strconv.FormatInt(repo, 10)+"/preview-access", owner, nil, url.Values{"private": {"false"}}); res.StatusCode != http.StatusSeeOther || queue.syncs != 2 {
		t.Fatalf("set public: %d, %d syncs", res.StatusCode, queue.syncs)
	}
	if res := do(http.MethodPost, "/repositories/"+strconv.FormatInt(repo, 10)+"/preview-access", owner, nil, url.Values{"private": {"false"}}); queue.syncs != 2 {
		t.Fatalf("an unchanged setting asks for no route sync: %d", res.StatusCode)
	}
	if res := do(http.MethodGet, signIn.RequestURI(), stranger, nil, nil); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "https://"+host+"/dashboard?x=1" {
		t.Fatalf("public: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
}

func TestPrivatePreviewCookieAndTokenStopAfterMemberRemoval(t *testing.T) {
	pool := isolatedPool(t)
	ctx := t.Context()
	for _, sql := range []string{
		"INSERT INTO users(github_id,username) VALUES(1,'owner'),(2,'reviewer')",
		"INSERT INTO installations(github_installation_id,owner_user_id) VALUES(99,1)",
		"INSERT INTO repos(installation_id,github_repo_id,full_name,private_previews) VALUES(1,100,'owner/app',true)",
		"INSERT INTO pull_requests(repo_id,number,head_sha) VALUES(1,1,'sha')",
		"INSERT INTO environments(pull_request_id,subdomain,status,expires_at) VALUES(1,'private-host','running',now()+interval '1 day')",
		"INSERT INTO workspace_memberships(workspace_id,user_id,role) VALUES(1,2,'reviewer')",
		"INSERT INTO repository_memberships(repo_id,workspace_id,user_id) VALUES(1,1,2)",
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	h := NewHandler(pool, session{names: map[int64]string{2: "reviewer"}}, github{"reviewer": "read"}, nil, "secret", "preview.test", "https://previa.test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodGet, "/previews/access?host=private-host.preview.test&path=/", nil)
	req = req.WithContext(context.WithValue(ctx, userKey{}, int64(2)))
	res := httptest.NewRecorder()
	h.access(res, req)
	if res.Code != 303 {
		t.Fatal("reviewer without source access denied", res.Code, res.Body.String())
	}
	location, err := url.Parse(res.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	forward := func(uri, cookie string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, routing.AccessPath, nil)
		request.Header.Set("X-Forwarded-Host", "private-host.preview.test")
		request.Header.Set("X-Forwarded-Uri", uri)
		if cookie != "" {
			request.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		}
		result := httptest.NewRecorder()
		h.check(result, request)
		return result
	}
	issued := forward(location.RequestURI(), "")
	cookies := issued.Result().Cookies()
	if issued.Code != 302 || len(cookies) != 1 {
		t.Fatal("cookie was not issued", issued.Code)
	}
	if got := forward("/", cookies[0].Value); got.Code != 200 {
		t.Fatal("valid local grant denied", got.Code)
	}
	for _, status := range []string{"publishing", "failed"} {
		if _, err := pool.Exec(ctx, "UPDATE environments SET status=$1 WHERE pull_request_id=1", status); err != nil {
			t.Fatal(err)
		}
		if got := forward("/", cookies[0].Value); got.Code != 200 {
			t.Fatal("previous private preview denied during replacement", status, got.Code)
		}
	}
	if _, err := pool.Exec(ctx, "UPDATE workspace_memberships SET state='removed',revision=revision+1 WHERE workspace_id=1 AND user_id=2"); err != nil {
		t.Fatal(err)
	}
	if got := forward("/", cookies[0].Value); got.Code == 200 {
		t.Fatal("removed member's cookie retained access")
	}
	if got := forward(location.RequestURI(), ""); got.Code != 403 {
		t.Fatal("pre-removal token replayed", got.Code)
	}
	denied := httptest.NewRecorder()
	h.access(denied, req)
	if denied.Code != 403 {
		t.Fatal("removed member fell back to GitHub outsider permission", denied.Code)
	}
}
