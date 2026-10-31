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

// save posts the editor's form: the file text and the revision it opened.
func save(b *browser, token, path, text string, revision int64, extra ...string) *httptest.ResponseRecorder {
	form := url.Values{"csrf_token": {token}, "env": {text}, "revision": {fmt.Sprint(revision)}}
	for i := 0; i+1 < len(extra); i += 2 {
		form.Set(extra[i], extra[i+1])
	}
	return b.do("POST", path, form)
}

func countRows(t *testing.T, f *fixture, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEnvFileHTTPFlowCSRFAndIsolation(t *testing.T) {
	f := setup(t)
	k := keyring(t, 1)
	service, err := secrets.NewService(secrets.NewRepository(f.pool), k)
	if err != nil {
		t.Fatal(err)
	}
	b, token := newBrowser(t, f, service)
	path := fmt.Sprintf("/repositories/%d/secrets", f.repoAvailable)
	const value = "sk_test_value_1234567890"

	if w := b.do("POST", path, url.Values{"env": {"API_KEY=" + value}, "revision": {"0"}}); w.Code != http.StatusForbidden {
		t.Fatalf("POST without CSRF token: %d", w.Code)
	}
	if page := b.do("GET", path, nil).Body.String(); !strings.Contains(page, `id="env-text"`) {
		t.Fatal("an empty file opens the editor straight away")
	}
	w := save(b, token, path, "# Sandbox only\nAPI_KEY="+value+"\nUNSET_FOR_NOW=\n", 0)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != path+"?added=1&saved=1" {
		t.Fatalf("save: %d %s", w.Code, w.Header().Get("Location"))
	}
	page := b.do("GET", path+"?added=1&saved=1", nil).Body.String()
	for _, want := range []string{"Saved the .env file: 1 added.", "# Sandbox only", "API_KEY", "••••••••", "UNSET_FOR_NOW", "not set", "Show values and edit", "Sets 1 variable · 63 B of 64 KiB"} {
		if !strings.Contains(page, want) {
			t.Fatalf("masked view lacks %q", want)
		}
	}
	if strings.Contains(page, value) {
		t.Fatal("the masked view must never contain a value")
	}
	var raw, file []byte
	var version int32
	if err := f.pool.QueryRow(t.Context(), "SELECT encrypted_value, key_version FROM env_secrets WHERE repo_id=$1 AND key='API_KEY'", f.repoAvailable).Scan(&raw, &version); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), "SELECT encrypted_text FROM repository_env_files WHERE repo_id=$1", f.repoAvailable).Scan(&file); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), value) || strings.Contains(string(file), "Sandbox") || strings.Contains(string(file), value) || version != 1 || storedValue(t, f, k, f.repoAvailable, "API_KEY") != value {
		t.Fatal("the value and the file must be stored encrypted under the active key and decrypt back")
	}
	if countRows(t, f, "SELECT count(*) FROM env_secrets WHERE repo_id=$1", f.repoAvailable) != 1 {
		t.Fatal("a name without a value must not become a secret")
	}

	// Show values and edit returns the file as written, never cached.
	edit := b.do("GET", path+"?edit=1", nil)
	if !strings.Contains(edit.Body.String(), "API_KEY="+value) || !strings.Contains(edit.Body.String(), `name="revision" value="1"`) || edit.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("editor: revision or value missing, cache %q", edit.Header().Get("Cache-Control"))
	}

	// CRLF from the textarea is normalized; a changed value is reported.
	if w := save(b, token, path, "# Sandbox only\r\nAPI_KEY=\"line one\r\nline two\"\r\nUNSET_FOR_NOW=\r\n", 1); w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "changed=1") {
		t.Fatalf("change: %d %s", w.Code, w.Header().Get("Location"))
	}
	if got := storedValue(t, f, k, f.repoAvailable, "API_KEY"); got != "line one\nline two" {
		t.Fatalf("changed value = %q", got)
	}

	// A save from an outdated editor is refused, keeps the typed text and
	// offers the current revision so saving again is a deliberate choice.
	stale := save(b, token, path, "API_KEY=from-a-stale-tab", 1)
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), "Someone saved this file") || !strings.Contains(stale.Body.String(), "from-a-stale-tab") || !strings.Contains(stale.Body.String(), `name="revision" value="2"`) {
		t.Fatalf("stale save: %d", stale.Code)
	}

	// Rejected files name the line and change nothing.
	for text, want := range map[string]string{
		"1BAD=x":             "Line 1:",
		"OK=1\nhas-dash=x":   "Line 2:",
		"PREVIA_TOKEN=x":     "reserved for the platform",
		"A=1\nA=2":           "A is already set on line 1",
		"not a line":         "Line 1: expected NAME=value",
		"KEY=\"never closed": "is not closed",
		"BIG=" + strings.Repeat("x", secrets.MaxFileBytes): "64 KiB",
	} {
		w := save(b, token, path, text, 2)
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), want) || !strings.Contains(w.Body.String(), "Nothing was saved") {
			t.Fatalf("%q: %d, want %q", text, w.Code, want)
		}
	}
	if got := storedValue(t, f, k, f.repoAvailable, "API_KEY"); got != "line one\nline two" {
		t.Fatalf("a refused save changed a value: %q", got)
	}

	// Removing a name asks first; confirming removes it and keeps the file.
	w = save(b, token, path, "# Sandbox only\n", 2)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `id="confirm-env-removals"`) || !strings.Contains(w.Body.String(), "Remove API_KEY?") {
		t.Fatalf("removal must be confirmed first: %d", w.Code)
	}
	if countRows(t, f, "SELECT count(*) FROM env_secrets WHERE repo_id=$1", f.repoAvailable) != 1 {
		t.Fatal("an unconfirmed removal changed the secrets")
	}
	if w := save(b, token, path, "# Sandbox only\n", 2, "confirm_removals", "1"); w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "removed=1") {
		t.Fatalf("confirmed removal: %d %s", w.Code, w.Header().Get("Location"))
	}
	if countRows(t, f, "SELECT count(*) FROM env_secrets WHERE repo_id=$1", f.repoAvailable) != 0 || countRows(t, f, "SELECT count(*) FROM repository_env_files WHERE repo_id=$1 AND revision=3", f.repoAvailable) != 1 {
		t.Fatal("confirmed removal must delete the secret and keep the file")
	}

	// Another owner's repository is invisible; a revoked one takes no new
	// values but can still drop names and comments.
	others := fmt.Sprintf("/repositories/%d/secrets", f.repoOthers)
	if w := b.do("GET", others, nil); w.Code != http.StatusNotFound {
		t.Fatalf("other owner's page: %d", w.Code)
	}
	if w := save(b, token, others, "STOLEN=x", 0); w.Code != http.StatusNotFound {
		t.Fatalf("other owner's write: %d", w.Code)
	}
	revoked := fmt.Sprintf("/repositories/%d/secrets", f.repoRevoked)
	if w := save(b, token, revoked, "NEW=x-value", 0); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "GitHub access to this repository was removed") {
		t.Fatalf("new value on a revoked repository: %d", w.Code)
	}
	if w := save(b, token, revoked, "# notes only\nLATER=\n", 0); w.Code != http.StatusSeeOther {
		t.Fatalf("comments on a revoked repository: %d", w.Code)
	}
}

func TestEnvFileOpensOlderSecretsAndKeepsBuildAccess(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	k := keyring(t, 1)
	service, err := secrets.NewService(secrets.NewRepository(f.pool), k)
	if err != nil {
		t.Fatal(err)
	}
	// Secrets saved one at a time before .env files existed have no file row.
	for name, value := range map[string]string{"B_TOKEN": "two words", "A_URL": "postgres://preview"} {
		sealed, version, err := k.Seal(f.repoAvailable, name, []byte(value))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, "INSERT INTO env_secrets(repo_id,key,encrypted_value,key_version) VALUES ($1,$2,$3,$4)", f.repoAvailable, name, sealed, version); err != nil {
			t.Fatal(err)
		}
	}
	file, err := service.File(ctx, f.owner, f.repoAvailable)
	if err != nil || file.Revision != 0 || string(file.Text) != "A_URL=postgres://preview\nB_TOKEN=\"two words\"\n" {
		t.Fatalf("file from older secrets = %q (revision %d), %v", file.Text, file.Revision, err)
	}
	saved, err := service.SaveFile(ctx, f.owner, f.repoAvailable, file.Text, 0, false)
	if err != nil || len(saved.Added)+len(saved.Changed)+len(saved.Removed) != 0 || saved.Revision != 1 {
		t.Fatalf("saving it unchanged: %+v %v", saved, err)
	}

	// Build access stays with a name through value changes and goes with it.
	if ok, err := service.SetBuild(ctx, f.owner, f.repoAvailable, "A_URL", true); err != nil || !ok {
		t.Fatalf("allow builds: %v %v", ok, err)
	}
	if _, err := service.SaveFile(ctx, f.owner, f.repoAvailable, []byte("A_URL=postgres://other\nB_TOKEN=\"two words\"\n"), 1, false); err != nil {
		t.Fatal(err)
	}
	if countRows(t, f, "SELECT count(*) FROM env_secrets WHERE repo_id=$1 AND key='A_URL' AND build", f.repoAvailable) != 1 {
		t.Fatal("changing a value must keep its build access")
	}
	if _, err := service.SaveFile(ctx, f.owner, f.repoAvailable, []byte("B_TOKEN=\"two words\"\nA_URL=\n"), 2, true); err != nil {
		t.Fatal(err)
	}
	if countRows(t, f, "SELECT count(*) FROM env_secrets WHERE repo_id=$1 AND key='A_URL'", f.repoAvailable) != 0 {
		t.Fatal("a name left empty must stop being a secret")
	}

	// Put changes one line in place and keeps the rest of the file.
	if _, err := service.SaveFile(ctx, f.owner, f.repoAvailable, []byte("# keep me\nB_TOKEN=\"two words\"\n"), 3, false); err != nil {
		t.Fatal(err)
	}
	if created, err := service.Put(ctx, f.owner, f.repoAvailable, "B_TOKEN", []byte("new")); err != nil || created {
		t.Fatalf("put existing: %v %v", created, err)
	}
	if created, err := service.Put(ctx, f.owner, f.repoAvailable, "C_NEW", []byte("added")); err != nil || !created {
		t.Fatalf("put new: %v %v", created, err)
	}
	file, err = service.File(ctx, f.owner, f.repoAvailable)
	if err != nil || string(file.Text) != "# keep me\nB_TOKEN=new\nC_NEW=added\n" {
		t.Fatalf("file after put = %q, %v", file.Text, err)
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
	var full strings.Builder
	for i := range secrets.MaxSecretsPerRepository {
		fmt.Fprintf(&full, "KEY_%03d=value\n", i)
	}
	if _, err := service.SaveFile(ctx, f.owner, f.repoAvailable, []byte(full.String()+"ONE_TOO_MANY=value\n"), 0, false); err != secrets.ErrLimit {
		t.Fatalf("limit not enforced: %v", err)
	}
	if _, err := service.SaveFile(ctx, f.owner, f.repoAvailable, []byte(full.String()), 0, false); err != nil {
		t.Fatal(err)
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
	if w := save(b, token, path, "NEW=x-value", 1); w.Code != http.StatusServiceUnavailable {
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
	// Three values and the repository's .env file.
	report, err := secrets.Reencrypt(ctx, secrets.NewRepository(f.pool), rotated.both, 2)
	if err != nil || report.Rotated != 4 || report.Skipped != 0 || len(report.Remaining) != 0 {
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
	if file, err := secrets.NewService(secrets.NewRepository(f.pool), rotated.onlyV2); err != nil {
		t.Fatal(err)
	} else if opened, err := file.File(ctx, f.owner, f.repoAvailable); err != nil || !strings.Contains(string(opened.Text), "C_KEY=value-C_KEY") {
		t.Fatalf("the .env file after rotation: %v", err)
	}

	// A value still on the old key, like one left from before a rotation.
	oldKeyValue := func(name, value string) {
		t.Helper()
		sealed, version, err := rotated.v1.Seal(f.repoAvailable, name, []byte(value))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, "INSERT INTO env_secrets(repo_id,key,encrypted_value,key_version) VALUES ($1,$2,$3,$4)", f.repoAvailable, name, sealed, version); err != nil {
			t.Fatal(err)
		}
	}

	// A concurrent owner edit during rotation wins and is not overwritten.
	oldKeyValue("D_KEY", "old")
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
	oldKeyValue("E_KEY", "v1-only")
	if _, err := secrets.Reencrypt(ctx, secrets.NewRepository(f.pool), rotated.onlyV2, 10); err == nil || !strings.Contains(err.Error(), "key version 1") {
		t.Fatalf("missing old key must stop re-encryption: %v", err)
	}
	if got := storedValue(t, f, rotated.both, f.repoAvailable, "E_KEY"); got != "v1-only" {
		t.Fatalf("failed rotation changed data: %q", got)
	}
}
