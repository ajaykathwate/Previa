package secrets_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"crypto/rand"
	"encoding/base64"

	"github.com/ajaykathwatee/previa/internal/auth"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/secrets"
	"github.com/alexedwards/scs/v2/memstore"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fixture struct {
	pool                                   *pgxpool.Pool
	owner, other                           int64
	repoAvailable, repoRevoked, repoOthers int64
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for isolated database tests")
	}
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("secrets_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		defer admin.Close()
		if _, err := admin.Exec(c, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	files, err := filepath.Glob("../../db/migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
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
	f := &fixture{pool: pool}
	scan := func(dst *int64, sql string, args ...any) {
		t.Helper()
		if err := pool.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	var ownInstall, otherInstall int64
	scan(&f.owner, `INSERT INTO users(github_id,username) VALUES (42,'owner') RETURNING id`)
	scan(&f.other, `INSERT INTO users(github_id,username) VALUES (43,'other') RETURNING id`)
	scan(&ownInstall, `INSERT INTO installations(github_installation_id,owner_user_id) VALUES (99,$1) RETURNING id`, f.owner)
	scan(&otherInstall, `INSERT INTO installations(github_installation_id,owner_user_id) VALUES (98,$1) RETURNING id`, f.other)
	scan(&f.repoAvailable, `INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES ($1,100,'owner/app') RETURNING id`, ownInstall)
	scan(&f.repoRevoked, `INSERT INTO repos(installation_id,github_repo_id,full_name,accessible) VALUES ($1,101,'owner/revoked',false) RETURNING id`, ownInstall)
	scan(&f.repoOthers, `INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES ($1,102,'other/app') RETURNING id`, otherInstall)
	return f
}

func keyring(t *testing.T, versions ...int) *secrets.Keyring {
	t.Helper()
	var parts []string
	for _, v := range versions {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, fmt.Sprintf("%d:%s", v, base64.StdEncoding.EncodeToString(key)))
	}
	k, err := secrets.ParseKeyring(strings.Join(parts, ","), fmt.Sprint(versions[len(versions)-1]))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

type testUsers struct{ id int64 }

func (u testUsers) UpsertUser(context.Context, auth.Identity) (auth.User, error) {
	return auth.User{ID: u.id, GitHubID: 42, Username: "owner"}, nil
}
func (u testUsers) GetUser(context.Context, int64) (auth.User, error) {
	return auth.User{ID: u.id, GitHubID: 42, Username: "owner"}, nil
}

type testOAuth struct{}

func (testOAuth) AuthCodeURL(s string) string {
	return "/auth/github/callback?code=test&state=" + url.QueryEscape(s)
}
func (testOAuth) Authenticate(context.Context, string) (auth.Identity, error) {
	return auth.Identity{ID: 42, Username: "owner"}, nil
}

// browser drives the real session, CSRF and secrets handlers.
type browser struct {
	t       *testing.T
	router  http.Handler
	cookies []*http.Cookie
}

func (b *browser) do(method, path string, form url.Values) *httptest.ResponseRecorder {
	b.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range b.cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	b.router.ServeHTTP(w, r)
	if c := w.Result().Cookies(); len(c) > 0 {
		b.cookies = c
	}
	return w
}

var csrfField = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

func newBrowser(t *testing.T, f *fixture, service *secrets.Service) (*browser, string) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := memstore.NewWithCleanupInterval(0)
	sessions := auth.NewSessionManager(store, false, log)
	authHandler := auth.NewHandler(config.ServerConfig{AppBaseURL: "http://localhost:8080", SessionSecret: strings.Repeat("s", 32)}, sessions, testUsers{f.owner}, testOAuth{}, log)
	router := chi.NewRouter()
	router.Use(sessions.LoadAndSave, authHandler.LoadUser, authHandler.CSRF)
	authHandler.Mount(router)
	secrets.NewHandler(service, authHandler, log).Mount(router, auth.RequireUser)
	b := &browser{t: t, router: router}
	if w := b.do("GET", fmt.Sprintf("/repositories/%d/secrets", f.repoAvailable), nil); w.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated access: %d", w.Code)
	}
	w := b.do("GET", "/auth/github/login", nil)
	w = b.do("GET", w.Header().Get("Location"), nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("login: %d", w.Code)
	}
	page := b.do("GET", fmt.Sprintf("/repositories/%d/secrets", f.repoAvailable), nil)
	m := csrfField.FindStringSubmatch(page.Body.String())
	if page.Code != http.StatusOK || m == nil {
		t.Fatalf("secrets page: %d", page.Code)
	}
	return b, m[1]
}

func storedValue(t *testing.T, f *fixture, k *secrets.Keyring, repoID int64, name string) string {
	t.Helper()
	p, err := secrets.NewProvider(secrets.NewRepository(f.pool), k)
	if err != nil {
		t.Fatal(err)
	}
	env, err := p.ForContainer(t.Context(), repoID, secrets.Source{BaseRepoID: 1, HeadRepoID: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer env.Wipe()
	for _, entry := range env.Environ() {
		if n, v, _ := strings.Cut(entry, "="); n == name {
			return v
		}
	}
	return ""
}

func TestSecretsHTTPOwnerFlowCSRFAndIsolation(t *testing.T) {
	f := setup(t)
	k := keyring(t, 1)
	service, err := secrets.NewService(secrets.NewRepository(f.pool), k)
	if err != nil {
		t.Fatal(err)
	}
	b, token := newBrowser(t, f, service)
	path := fmt.Sprintf("/repositories/%d/secrets", f.repoAvailable)
	const value = "sk_test_value_1234567890"

	if w := b.do("POST", path, url.Values{"name": {"API_KEY"}, "value": {value}}); w.Code != http.StatusForbidden {
		t.Fatalf("POST without CSRF token: %d", w.Code)
	}
	w := b.do("POST", path, url.Values{"csrf_token": {token}, "name": {"API_KEY"}, "value": {value}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != path+"?saved=API_KEY" {
		t.Fatalf("save: %d %s", w.Code, w.Header().Get("Location"))
	}
	page := b.do("GET", path+"?saved=API_KEY", nil).Body.String()
	if !strings.Contains(page, "API_KEY") || !strings.Contains(page, "Saved API_KEY") || strings.Contains(page, value) {
		t.Fatal("page must show the name and never the value")
	}
	var raw []byte
	var version int32
	if err := f.pool.QueryRow(t.Context(), "SELECT encrypted_value, key_version FROM env_secrets WHERE repo_id=$1 AND key='API_KEY'", f.repoAvailable).Scan(&raw, &version); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), value) || version != 1 || storedValue(t, f, k, f.repoAvailable, "API_KEY") != value {
		t.Fatal("value must be stored encrypted under the active key and decrypt back")
	}

	// Replace keeps one row; CRLF from a textarea is normalized.
	if w := b.do("POST", path, url.Values{"csrf_token": {token}, "name": {"API_KEY"}, "value": {"line one\r\nline two"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("replace: %d", w.Code)
	}
	if got := storedValue(t, f, k, f.repoAvailable, "API_KEY"); got != "line one\nline two" {
		t.Fatalf("replaced value = %q", got)
	}
	if replace := b.do("GET", path+"?replace=API_KEY", nil).Body.String(); !strings.Contains(replace, "Replace API_KEY") || strings.Contains(replace, "line one") {
		t.Fatal("replace form must not reveal the current value")
	}

	// Rejected submissions never echo the value.
	for _, name := range []string{"1BAD", "has-dash", "PREVIA_TOKEN", ""} {
		w := b.do("POST", path, url.Values{"csrf_token": {token}, "name": {name}, "value": {"echo-me-not-123"}})
		if w.Code != http.StatusUnprocessableEntity || strings.Contains(w.Body.String(), "echo-me-not-123") {
			t.Fatalf("invalid name %q: %d (value echoed: %v)", name, w.Code, strings.Contains(w.Body.String(), "echo-me-not-123"))
		}
	}
	if w := b.do("POST", path, url.Values{"csrf_token": {token}, "name": {"EMPTY"}, "value": {""}}); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty value: %d", w.Code)
	}

	// Another owner's repository is invisible; a revoked one is delete-only.
	others := fmt.Sprintf("/repositories/%d/secrets", f.repoOthers)
	if w := b.do("GET", others, nil); w.Code != http.StatusNotFound {
		t.Fatalf("other owner's page: %d", w.Code)
	}
	if w := b.do("POST", others, url.Values{"csrf_token": {token}, "name": {"STOLEN"}, "value": {"x-value"}}); w.Code != http.StatusNotFound {
		t.Fatalf("other owner's write: %d", w.Code)
	}
	if w := b.do("POST", others+"/delete", url.Values{"csrf_token": {token}, "name": {"ANY"}}); w.Code != http.StatusNotFound {
		t.Fatalf("other owner's delete: %d", w.Code)
	}
	revoked := fmt.Sprintf("/repositories/%d/secrets", f.repoRevoked)
	if w := b.do("POST", revoked, url.Values{"csrf_token": {token}, "name": {"NEW"}, "value": {"x-value"}}); w.Code != http.StatusConflict {
		t.Fatalf("write to revoked repository: %d", w.Code)
	}

	// Delete through the confirmation form.
	if w := b.do("POST", path+"/delete", url.Values{"name": {"API_KEY"}}); w.Code != http.StatusForbidden {
		t.Fatalf("delete without CSRF: %d", w.Code)
	}
	if w := b.do("POST", path+"/delete", url.Values{"csrf_token": {token}, "name": {"API_KEY"}}); w.Code != http.StatusSeeOther || !strings.HasSuffix(w.Header().Get("Location"), "deleted=API_KEY") {
		t.Fatalf("delete: %d %s", w.Code, w.Header().Get("Location"))
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM env_secrets").Scan(&count); err != nil || count != 0 {
		t.Fatalf("rows after delete: %d %v", count, err)
	}
}

func TestSecretsLimitConstraintsAndUnconfiguredServer(t *testing.T) {
	f := setup(t)
	k := keyring(t, 1)
	service, err := secrets.NewService(secrets.NewRepository(f.pool), k)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for i := range secrets.MaxSecretsPerRepository {
		if _, err := service.Put(ctx, f.owner, f.repoAvailable, fmt.Sprintf("KEY_%03d", i), []byte("value")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.Put(ctx, f.owner, f.repoAvailable, "ONE_TOO_MANY", []byte("value")); err != secrets.ErrLimit {
		t.Fatalf("limit not enforced: %v", err)
	}
	if created, err := service.Put(ctx, f.owner, f.repoAvailable, "KEY_000", []byte("replaced")); err != nil || created {
		t.Fatalf("replacing at the limit must work: %v created=%v", err, created)
	}
	if _, err := service.Put(ctx, f.other, f.repoAvailable, "INTRUDER", []byte("value")); err != secrets.ErrUnavailable {
		t.Fatalf("cross-owner write: %v", err)
	}
	// The database rejects malformed names and values even if code regressed.
	if _, err := f.pool.Exec(ctx, "INSERT INTO env_secrets(repo_id,key,encrypted_value,key_version) VALUES ($1,'bad-name',$2,1)", f.repoAvailable, make([]byte, 40)); err == nil {
		t.Fatal("database accepted an invalid secret name")
	}
	if _, err := f.pool.Exec(ctx, "INSERT INTO env_secrets(repo_id,key,encrypted_value,key_version) VALUES ($1,'TINY',$2,1)", f.repoAvailable, []byte("x")); err == nil {
		t.Fatal("database accepted a value too short to be ciphertext")
	}

	unconfigured, err := secrets.NewService(secrets.NewRepository(f.pool), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, token := newBrowser(t, f, unconfigured)
	path := fmt.Sprintf("/repositories/%d/secrets", f.repoAvailable)
	if page := b.do("GET", path, nil).Body.String(); !strings.Contains(page, "not configured") || !strings.Contains(page, "KEY_000") {
		t.Fatal("unconfigured server must explain itself and still list names")
	}
	if w := b.do("POST", path, url.Values{"csrf_token": {token}, "name": {"NEW"}, "value": {"x-value"}}); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("write without keyring: %d", w.Code)
	}
}

// interferingStore replaces a value between the rotation read and write.
type interferingStore struct {
	*secrets.Repository
	once func()
}

func (s interferingStore) Rotate(ctx context.Context, row secrets.StoredSecret, sealed []byte, version int32) (bool, error) {
	if s.once != nil {
		s.once()
	}
	return s.Repository.Rotate(ctx, row, sealed, version)
}

// rotationKeys shares key material across the stages of a rotation.
type rotationKeys struct{ v1, both, onlyV2 *secrets.Keyring }

func rotationKeyring(t *testing.T) rotationKeys {
	t.Helper()
	key := func() string {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		return base64.StdEncoding.EncodeToString(b)
	}
	k1, k2 := key(), key()
	parse := func(spec, active string) *secrets.Keyring {
		k, err := secrets.ParseKeyring(spec, active)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	return rotationKeys{v1: parse("1:"+k1, "1"), both: parse("1:"+k1+",2:"+k2, "2"), onlyV2: parse("2:"+k2, "2")}
}

func TestReencryptMovesValuesToTheActiveKey(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	rotated := rotationKeyring(t)
	service, err := secrets.NewService(secrets.NewRepository(f.pool), rotated.v1)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"A_KEY", "B_KEY", "C_KEY"} {
		if _, err := service.Put(ctx, f.owner, f.repoAvailable, name, []byte("value-"+name)); err != nil {
			t.Fatal(err)
		}
	}
	// The rotation keyring keeps v1 so existing values stay readable throughout.
	report, err := secrets.Reencrypt(ctx, secrets.NewRepository(f.pool), rotated.both, 2)
	if err != nil || report.Rotated != 3 || report.Skipped != 0 || len(report.Remaining) != 0 {
		t.Fatalf("rotation report %+v, %v", report, err)
	}
	for _, name := range []string{"A_KEY", "B_KEY", "C_KEY"} {
		if got := storedValue(t, f, rotated.onlyV2, f.repoAvailable, name); got != "value-"+name {
			t.Fatalf("%s after rotation = %q", name, got)
		}
	}
	if again, err := secrets.Reencrypt(ctx, secrets.NewRepository(f.pool), rotated.both, 2); err != nil || again.Rotated != 0 {
		t.Fatalf("second run must be a no-op: %+v %v", again, err)
	}

	// A concurrent owner edit during rotation wins and is not overwritten.
	if _, err := service.Put(ctx, f.owner, f.repoAvailable, "D_KEY", []byte("old")); err != nil {
		t.Fatal(err)
	}
	editor, err := secrets.NewService(secrets.NewRepository(f.pool), rotated.both)
	if err != nil {
		t.Fatal(err)
	}
	report, err = secrets.Reencrypt(ctx, interferingStore{Repository: secrets.NewRepository(f.pool), once: func() {
		if _, err := editor.Put(ctx, f.owner, f.repoAvailable, "D_KEY", []byte("edited")); err != nil {
			t.Error(err)
		}
	}}, rotated.both, 10)
	if err != nil || report.Skipped != 1 || storedValue(t, f, rotated.onlyV2, f.repoAvailable, "D_KEY") != "edited" {
		t.Fatalf("concurrent edit lost: %+v %v", report, err)
	}

	// Removing the old key too early is reported, and nothing is changed.
	if _, err := service.Put(ctx, f.owner, f.repoAvailable, "E_KEY", []byte("v1-only")); err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.Reencrypt(ctx, secrets.NewRepository(f.pool), rotated.onlyV2, 10); err == nil || !strings.Contains(err.Error(), "key version 1") {
		t.Fatalf("missing old key must stop re-encryption: %v", err)
	}
	if got := storedValue(t, f, rotated.both, f.repoAvailable, "E_KEY"); got != "v1-only" {
		t.Fatalf("failed rotation changed data: %q", got)
	}
}
