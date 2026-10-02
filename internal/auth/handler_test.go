package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/dashboard"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/alexedwards/scs/v2"
	"github.com/alexedwards/scs/v2/memstore"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

type fakeProvider struct {
	identity Identity
	err      error
	calls    int
}

func (p *fakeProvider) AuthCodeURL(state string) string {
	return "https://github.com/login/oauth/authorize?state=" + url.QueryEscape(state)
}

func (p *fakeProvider) Authenticate(context.Context, string) (Identity, error) {
	p.calls++
	return p.identity, p.err
}

type fakeUsers struct {
	user    User
	err     error
	upserts int
}

func (f *fakeUsers) UpsertUser(_ context.Context, i Identity) (User, error) {
	f.upserts++
	if f.err != nil {
		return User{}, f.err
	}
	f.user = User{ID: 1, GitHubID: i.ID, Username: i.Username, AvatarURL: i.AvatarURL}
	return f.user, nil
}

func (f *fakeUsers) GetUser(context.Context, int64) (User, error) {
	if f.err != nil {
		return User{}, f.err
	}
	if f.user.ID == 0 {
		return User{}, pgx.ErrNoRows
	}
	return f.user, nil
}

type testBrowser struct {
	t       *testing.T
	router  http.Handler
	cookies map[string]*http.Cookie
}

func newTestBrowser(t *testing.T, store scs.Store, users userRepository, provider identityProvider) (*testBrowser, *Handler, *scs.SessionManager) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := NewSessionManager(store, false, logger)
	h := NewHandler(config.ServerConfig{AppBaseURL: "http://localhost:8080", SessionSecret: strings.Repeat("s", 32)}, s, users, provider, logger)
	r := chi.NewRouter()
	r.Use(s.LoadAndSave, h.LoadUser, h.CSRF)
	h.Mount(r)
	dashboard.Mount(r, h, RequireUser)
	return &testBrowser{t: t, router: r, cookies: make(map[string]*http.Cookie)}, h, s
}

func (b *testBrowser) request(method, path, body string, headers ...map[string]string) *httptest.ResponseRecorder {
	b.t.Helper()
	r := httptest.NewRequest(method, "http://localhost:8080"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, cookie := range b.cookies {
		if strings.HasPrefix(r.URL.Path, cookie.Path) {
			r.AddCookie(cookie)
		}
	}
	for _, values := range headers {
		for key, value := range values {
			r.Header.Set(key, value)
		}
	}
	w := httptest.NewRecorder()
	b.router.ServeHTTP(w, r)
	for _, cookie := range w.Result().Cookies() {
		if cookie.MaxAge < 0 {
			delete(b.cookies, cookie.Name)
		} else {
			b.cookies[cookie.Name] = cookie
		}
	}
	return w
}

func (b *testBrowser) loginState() string {
	b.t.Helper()
	w := b.request(http.MethodGet, "/auth/github/login", "")
	if w.Code != http.StatusSeeOther {
		b.t.Fatalf("login status = %d: %s", w.Code, w.Body)
	}
	u, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		b.t.Fatal(err)
	}
	return u.Query().Get("state")
}

func storeToken(cookie string) string {
	hash := sha256.Sum256([]byte(cookie))
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

func TestLoginRotationProtectedRouteAndLogout(t *testing.T) {
	store := memstore.NewWithCleanupInterval(0)
	users := &fakeUsers{}
	provider := &fakeProvider{identity: Identity{ID: 42, Username: "octocat", AvatarURL: "https://example.com/avatar"}}
	b, _, _ := newTestBrowser(t, store, users, provider)
	if w := b.request(http.MethodGet, "/", ""); !strings.Contains(w.Body.String(), "Get started with GitHub") {
		t.Fatal("anonymous page has no login link")
	}
	oldCookie := b.cookies["previa_session"].Value
	if w := b.request(http.MethodGet, "/dashboard", ""); w.Code != 303 || w.Header().Get("Location") != "/auth/github/login" {
		t.Fatal("anonymous dashboard request was not redirected")
	}
	if w := b.request(http.MethodGet, "/protected", ""); w.Code != 303 || w.Header().Get("Location") != "/auth/github/login" {
		t.Fatal("anonymous protected request was not redirected")
	}
	state := b.loginState()
	stateCookie := b.cookies[stateCookieName]
	if !stateCookie.HttpOnly || stateCookie.SameSite != http.SameSiteLaxMode || stateCookie.MaxAge != 600 {
		t.Fatalf("unsafe state cookie: %+v", stateCookie)
	}
	w := b.request(http.MethodGet, "/auth/github/callback?code=valid&state="+state, "")
	if w.Code != 303 || w.Header().Get("Location") != "/dashboard" {
		t.Fatalf("callback failed: %d %s", w.Code, w.Body)
	}
	newCookie := b.cookies["previa_session"]
	if oldCookie == newCookie.Value || !newCookie.HttpOnly || newCookie.SameSite != http.SameSiteLaxMode {
		t.Fatal("session was not rotated with safe cookie flags")
	}
	if _, found, err := store.Find(storeToken(oldCookie)); err != nil || found {
		t.Fatalf("old session remains: %v %v", found, err)
	}
	if w := b.request(http.MethodGet, "/dashboard", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "octocat") || !strings.Contains(w.Body.String(), "Workspace navigation") {
		t.Fatal("logged-in dashboard does not show username")
	}
	if w := b.request(http.MethodGet, "/", ""); !strings.Contains(w.Body.String(), "Open your dashboard") || strings.Contains(w.Body.String(), "Get started with GitHub") {
		t.Fatal("signed-in public home does not link to the dashboard")
	}
	if w := b.request(http.MethodGet, "/protected", ""); w.Code != 200 {
		t.Fatalf("protected route status = %d", w.Code)
	}
	if w := b.request(http.MethodGet, "/auth/logout", ""); w.Code != 303 || w.Header().Get("Location") != "/dashboard?confirm=logout" {
		t.Fatal("legacy logout link did not open confirmation on dashboard")
	}
	form := b.request(http.MethodGet, "/dashboard", "")
	if !strings.Contains(form.Body.String(), `<dialog id="logout-confirmation"`) || !strings.Contains(form.Body.String(), `aria-haspopup="dialog"`) {
		t.Fatal("dashboard has no logout confirmation dialog")
	}
	match := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`).FindStringSubmatch(form.Body.String())
	if len(match) != 2 {
		t.Fatalf("missing logout CSRF token: %s", form.Body)
	}
	for _, body := range []string{"", "csrf_token=wrong"} {
		if w := b.request(http.MethodPost, "/auth/logout", body); w.Code != 403 {
			t.Fatalf("invalid CSRF status = %d", w.Code)
		}
	}
	if w := b.request(http.MethodPost, "/auth/logout", "csrf_token="+match[1], map[string]string{"Origin": "https://evil.example"}); w.Code != 403 {
		t.Fatal("cross-origin logout accepted")
	}
	if w := b.request(http.MethodPost, "/auth/logout", "", map[string]string{"X-CSRF-Token": match[1]}); w.Code != 303 {
		t.Fatalf("valid logout status = %d", w.Code)
	}
	if _, exists := b.cookies["previa_session"]; exists {
		t.Fatal("logout did not clear session cookie")
	}
	if _, found, err := store.Find(storeToken(newCookie.Value)); err != nil || found {
		t.Fatalf("logout did not delete server-side session: %v %v", found, err)
	}
	if w := b.request(http.MethodGet, "/dashboard", ""); w.Code != 303 {
		t.Fatal("logged-out user can access dashboard")
	}
	b.cookies["previa_session"] = newCookie
	if w := b.request(http.MethodGet, "/protected", ""); w.Code != 303 {
		t.Fatal("replayed logged-out cookie was accepted")
	}
}

func TestCallbackFailuresDoNotAuthenticate(t *testing.T) {
	for _, scenario := range []string{"missing-cookie", "missing-state", "wrong-state", "tampered-cookie", "expired-cookie", "denied", "missing-code", "provider-down", "repository-down"} {
		t.Run(scenario, func(t *testing.T) {
			users := &fakeUsers{}
			provider := &fakeProvider{identity: Identity{ID: 42, Username: "octocat"}}
			b, h, _ := newTestBrowser(t, memstore.NewWithCleanupInterval(0), users, provider)
			state := b.loginState()
			query := url.Values{"state": {state}, "code": {"valid"}}
			switch scenario {
			case "missing-cookie":
				delete(b.cookies, stateCookieName)
			case "missing-state":
				query.Del("state")
			case "wrong-state":
				query.Set("state", "wrong")
			case "tampered-cookie":
				b.cookies[stateCookieName].Value += "tampered"
			case "expired-cookie":
				b.cookies[stateCookieName].Value = state + ".1.invalid"
			case "denied":
				query.Set("error", "access_denied")
			case "missing-code":
				query.Del("code")
			case "provider-down":
				provider.err = errors.New("provider unavailable")
			case "repository-down":
				users.err = errors.New("database unavailable")
			}
			w := b.request(http.MethodGet, "/auth/github/callback?"+query.Encode(), "")
			if w.Code < 400 || !strings.Contains(w.Body.String(), "Sign-in") {
				t.Fatalf("expected clear failure page, got %d %s", w.Code, w.Body)
			}
			if _, exists := b.cookies[stateCookieName]; exists {
				t.Fatal("failure left state cookie")
			}
			users.err = nil
			if w := b.request(http.MethodGet, "/protected", ""); w.Code != 303 {
				t.Fatal("failure created an authenticated session")
			}
			if h.validState(h.signState(state), "wrong") {
				t.Fatal("signed state accepted mismatch")
			}
		})
	}
}

func TestSessionTimeouts(t *testing.T) {
	for _, scenario := range []string{"idle", "absolute"} {
		t.Run(scenario, func(t *testing.T) {
			store := memstore.NewWithCleanupInterval(0)
			provider := &fakeProvider{identity: Identity{ID: 42, Username: "octocat"}}
			b, _, sessions := newTestBrowser(t, store, &fakeUsers{}, provider)
			if sessions.Lifetime != 24*time.Hour || sessions.IdleTimeout != 30*time.Minute {
				t.Fatal("incorrect timeout configuration")
			}
			state := b.loginState()
			b.request(http.MethodGet, "/auth/github/callback?code=valid&state="+state, "")
			key := storeToken(b.cookies["previa_session"].Value)
			data, found, err := store.Find(key)
			if err != nil || !found {
				t.Fatalf("session not persisted: %v", err)
			}
			expiry := time.Now().Add(-time.Second)
			if scenario == "absolute" {
				_, values, err := sessions.Codec.Decode(data)
				if err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(time.Minute).UTC()
				data, err = sessions.Codec.Encode(deadline, values)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.Commit(key, data, deadline); err != nil {
					t.Fatal(err)
				}
				loaded, err := sessions.Load(t.Context(), b.cookies["previa_session"].Value)
				if err != nil {
					t.Fatal(err)
				}
				_, cappedExpiry, err := sessions.Commit(loaded)
				if err != nil || !cappedExpiry.Equal(deadline) {
					t.Fatalf("idle renewal exceeded absolute deadline: %v %v", cappedExpiry, err)
				}
				data, err = sessions.Codec.Encode(expiry, values)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Commit(key, data, expiry); err != nil {
				t.Fatal(err)
			}
			if w := b.request(http.MethodGet, "/protected", ""); w.Code != 303 {
				t.Fatalf("expired %s session accepted", scenario)
			}
		})
	}
}

func TestHTTPSCookieFlagsAndCallbackReplay(t *testing.T) {
	provider := &fakeProvider{identity: Identity{ID: 42, Username: "octocat"}}
	b, h, sessions := newTestBrowser(t, memstore.NewWithCleanupInterval(0), &fakeUsers{}, provider)
	h.secure = true
	sessions.Cookie.Secure = true
	state := b.loginState()
	if !b.cookies[stateCookieName].Secure || !b.cookies["previa_session"].Secure {
		t.Fatal("HTTPS cookies missing Secure flag")
	}
	signed := *b.cookies[stateCookieName]
	if w := b.request(http.MethodGet, "/auth/github/callback?code=valid&state="+state, ""); w.Code != 303 {
		t.Fatal("initial callback failed")
	}
	b.cookies[stateCookieName] = &signed
	if w := b.request(http.MethodGet, "/auth/github/callback?code=valid&state="+state, ""); w.Code != 400 || provider.calls != 1 {
		t.Fatal("replayed state was exchanged again")
	}
}

type failingStore struct {
	scs.Store
	failDelete bool
	failCommit bool
}

func (s *failingStore) Delete(token string) error {
	if s.failDelete {
		return errors.New("delete unavailable")
	}
	return s.Store.Delete(token)
}

func (s *failingStore) Commit(token string, data []byte, expiry time.Time) error {
	if s.failCommit {
		return errors.New("commit unavailable")
	}
	return s.Store.Commit(token, data, expiry)
}

func TestSessionStorageFailureDoesNotAuthenticate(t *testing.T) {
	for _, scenario := range []string{"rotation", "commit"} {
		t.Run(scenario, func(t *testing.T) {
			store := &failingStore{Store: memstore.NewWithCleanupInterval(0)}
			provider := &fakeProvider{identity: Identity{ID: 42, Username: "octocat"}}
			b, _, _ := newTestBrowser(t, store, &fakeUsers{}, provider)
			state := b.loginState()
			store.failDelete = scenario == "rotation"
			store.failCommit = scenario == "commit"
			w := b.request(http.MethodGet, "/auth/github/callback?code=valid&state="+state, "")
			if w.Code != 503 || w.Header().Get("Location") != "" {
				t.Fatalf("storage failure returned success: %d %s", w.Code, w.Body)
			}
			store.failDelete, store.failCommit = false, false
			if w := b.request(http.MethodGet, "/protected", ""); w.Code != 303 {
				t.Fatal("storage failure left an authenticated client")
			}
		})
	}
}
