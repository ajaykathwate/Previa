package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/realtime"
	"github.com/alexedwards/scs/pgxstore"
)

const (
	testAppHost       = "previa.example.test"
	testWebhookSecret = "regression-webhook-secret"
)

// TestServerRoutesRegression drives the server's HTTP surface exactly as
// RunServer assembles it (every feature, its ingress controls and its routes)
// against a migrated database, the way a browser, an attacker or GitHub would.
func TestServerRoutesRegression(t *testing.T) {
	pool := e2eSchema(t)
	store := pgxstore.New(pool)
	t.Cleanup(store.StopCleanup)
	cfg := config.ServerConfig{
		AppBaseURL: "https://" + testAppHost, GitHubOAuthClientID: "regression-client", GitHubOAuthClientSecret: "regression-secret",
		SessionSecret: strings.Repeat("regression-session-", 3), GitHubWebhookSecret: testWebhookSecret,
	}
	cfg.AppEnv = config.Production
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := realtime.NewHub()
	t.Cleanup(hub.Close)
	features, err := newServerFeatures(cfg, pool, store, hub, logger)
	if err != nil {
		t.Fatal(err)
	}
	router, err := features.routes(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	send := func(method, path string, body io.Reader, edit func(*http.Request)) *http.Response {
		t.Helper()
		r := httptest.NewRequest(method, "https://"+testAppHost+path, body)
		r.RemoteAddr = "192.0.2.10:40000"
		if edit != nil {
			edit(r)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w.Result()
	}
	from := func(addr string) func(*http.Request) { return func(r *http.Request) { r.RemoteAddr = addr } }

	t.Run("public pages carry the security headers and a fresh nonce", func(t *testing.T) {
		first, second := send(http.MethodGet, "/", nil, nil), send(http.MethodGet, "/", nil, nil)
		if first.StatusCode != http.StatusOK {
			t.Fatalf("home: %d", first.StatusCode)
		}
		want := map[string]string{
			"X-Frame-Options": "DENY", "X-Content-Type-Options": "nosniff", "Referrer-Policy": "strict-origin-when-cross-origin",
			"Cross-Origin-Opener-Policy": "same-origin", "Strict-Transport-Security": "max-age=31536000",
		}
		for name, value := range want {
			if got := first.Header.Get(name); got != value {
				t.Errorf("%s = %q, want %q", name, got, value)
			}
		}
		csp := first.Header.Get("Content-Security-Policy")
		for _, directive := range []string{"default-src 'self'", "object-src 'none'", "base-uri 'none'", "frame-ancestors 'none'", "'nonce-"} {
			if !strings.Contains(csp, directive) {
				t.Errorf("CSP lacks %q: %s", directive, csp)
			}
		}
		if csp == second.Header.Get("Content-Security-Policy") {
			t.Error("two responses share a CSP nonce")
		}
	})

	t.Run("the session cookie is host-only, secure, HttpOnly and SameSite=Lax", func(t *testing.T) {
		var session *http.Cookie
		for _, c := range send(http.MethodGet, "/", nil, nil).Cookies() {
			if strings.Contains(c.Name, "previa_session") {
				session = c
			}
		}
		if session == nil {
			t.Fatal("no session cookie on the first page")
		}
		if session.Name != "__Host-previa_session" || !session.Secure || !session.HttpOnly || session.SameSite != http.SameSiteLaxMode || session.Domain != "" || session.Path != "/" {
			t.Errorf("session cookie %+v", session)
		}
	})

	t.Run("requests for another host are refused", func(t *testing.T) {
		for _, host := range []string{"evil.example.test", "pr-1-abc.preview.example.test", testAppHost + ".evil.test"} {
			response := send(http.MethodGet, "/", nil, func(r *http.Request) { r.Host = host })
			if response.StatusCode != http.StatusMisdirectedRequest {
				t.Errorf("host %s: %d, want 421", host, response.StatusCode)
			}
		}
		// Forwarded hosts are believed only from trusted proxies (none here).
		response := send(http.MethodGet, "/", nil, func(r *http.Request) { r.Header.Set("X-Forwarded-Host", "evil.example.test") })
		if response.StatusCode != http.StatusOK {
			t.Errorf("untrusted X-Forwarded-Host changed the outcome: %d", response.StatusCode)
		}
	})

	t.Run("protected pages send anonymous visitors to sign in", func(t *testing.T) {
		for _, path := range []string{"/dashboard", "/repositories", "/builds", "/previews", "/environments", "/repositories/1",
			"/repositories/1/pull-requests/1", "/repositories/1/secrets", "/repositories/1/outbound", "/repositories/1/pull-requests/1/logs", "/auth/logout"} {
			login := "/auth/github/login?" + url.Values{"next": {path}}.Encode()
			response := send(http.MethodGet, path, nil, nil)
			if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != login {
				t.Errorf("%s: %d to %q", path, response.StatusCode, response.Header.Get("Location"))
			}
			partial := send(http.MethodGet, path, nil, func(r *http.Request) { r.Header.Set("HX-Request", "true") })
			if partial.StatusCode != http.StatusUnauthorized || partial.Header.Get("HX-Redirect") != login {
				t.Errorf("%s (htmx): %d, HX-Redirect %q", path, partial.StatusCode, partial.Header.Get("HX-Redirect"))
			}
		}
	})

	t.Run("mutations without a CSRF token or from another site are refused", func(t *testing.T) {
		form := func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }
		for _, path := range []string{"/auth/logout", "/repositories/1/policy", "/repositories/1/refresh", "/repositories/1/pull-requests/1/build",
			"/repositories/1/pull-requests/1/teardown", "/repositories/1/secrets", "/repositories/1/outbound"} {
			if response := send(http.MethodPost, path, strings.NewReader("mode=automatic"), form); response.StatusCode != http.StatusForbidden {
				t.Errorf("%s without a token: %d, want 403", path, response.StatusCode)
			}
			crossSite := send(http.MethodPost, path, strings.NewReader("csrf_token=guess"), func(r *http.Request) {
				form(r)
				r.Header.Set("Origin", "https://evil.example.test")
			})
			if crossSite.StatusCode != http.StatusForbidden {
				t.Errorf("%s from another origin: %d, want 403", path, crossSite.StatusCode)
			}
		}
	})

	t.Run("oversized form bodies are refused before parsing", func(t *testing.T) {
		big := strings.NewReader("value=" + strings.Repeat("a", maxFormBytes))
		response := send(http.MethodPost, "/repositories/1/secrets", big, func(r *http.Request) {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		})
		if response.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("oversized form: %d, want 413", response.StatusCode)
		}
	})

	t.Run("sign-in is rate limited per client", func(t *testing.T) {
		limited := 0
		for i := 0; i < 25; i++ {
			response := send(http.MethodGet, "/auth/github/login", nil, from("198.51.100.7:1000"))
			if response.StatusCode == http.StatusTooManyRequests {
				limited++
				if response.Header.Get("Retry-After") == "" {
					t.Error("429 without Retry-After")
				}
			} else if i < 20 && response.StatusCode >= 400 {
				t.Errorf("request %d within the burst: %d", i, response.StatusCode)
			}
		}
		if limited == 0 {
			t.Error("25 sign-in starts in a burst were never limited")
		}
		if other := send(http.MethodGet, "/auth/github/login", nil, from("198.51.100.8:1000")); other.StatusCode == http.StatusTooManyRequests {
			t.Error("one client's burst limited another client")
		}
		if page := send(http.MethodGet, "/", nil, from("198.51.100.7:1000")); page.StatusCode != http.StatusOK {
			t.Errorf("the limit spilled onto ordinary pages: %d", page.StatusCode)
		}
	})

	t.Run("webhooks need GitHub's signature and a sound envelope", func(t *testing.T) {
		body := []byte(`{"zen":"Keep it logically awesome."}`)
		hook := func(event, delivery, signature string, payload []byte, host string) int {
			return send(http.MethodPost, webhookPath, strings.NewReader(string(payload)), func(r *http.Request) {
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("X-GitHub-Event", event)
				r.Header.Set("X-GitHub-Delivery", delivery)
				if signature != "" {
					r.Header.Set("X-Hub-Signature-256", signature)
				}
				if host != "" {
					r.Host = host
				}
			}).StatusCode
		}
		for name, tt := range map[string]struct {
			event, delivery, signature string
			payload                    []byte
			want                       int
		}{
			"unsigned":                   {"ping", "d-1", "", body, http.StatusUnauthorized},
			"signed with another key":    {"ping", "d-2", sign([]byte("other-secret"), body), body, http.StatusUnauthorized},
			"signature without a scheme": {"ping", "d-3", hex.EncodeToString(hmacOf([]byte(testWebhookSecret), body)), body, http.StatusUnauthorized},
			"a malformed delivery ID":    {"ping", "../etc/passwd", sign([]byte(testWebhookSecret), body), body, http.StatusBadRequest},
			"no event kind":              {"", "d-4", sign([]byte(testWebhookSecret), body), body, http.StatusBadRequest},
			"not JSON":                   {"ping", "d-5", sign([]byte(testWebhookSecret), []byte("not json")), []byte("not json"), http.StatusBadRequest},
			"an event Previa ignores":    {"ping", "d-6", sign([]byte(testWebhookSecret), body), body, http.StatusNoContent},
			"a broken pull_request":      {"pull_request", "d-7", sign([]byte(testWebhookSecret), body), body, http.StatusBadRequest},
		} {
			if got := hook(tt.event, tt.delivery, tt.signature, tt.payload, ""); got != tt.want {
				t.Errorf("%s: %d, want %d", name, got, tt.want)
			}
		}
		// GitHub may reach the webhook through a tunnel: the host is not checked there.
		if got := hook("ping", "d-8", sign([]byte(testWebhookSecret), body), body, "tunnel.example.net"); got != http.StatusNoContent {
			t.Errorf("webhook through another host: %d", got)
		}
		huge := []byte(`{"pad":"` + strings.Repeat("a", 2<<20) + `"}`)
		if got := hook("ping", "d-9", sign([]byte(testWebhookSecret), huge), huge, ""); got != http.StatusRequestEntityTooLarge {
			t.Errorf("a body over 2 MiB: %d, want 413", got)
		}
	})

	t.Run("a pull_request delivery is recorded once and a reused ID is a conflict", func(t *testing.T) {
		event := pullRequestEvent(t, 7, "opened", "1111111111111111111111111111111111111111")
		deliver := func(payload []byte) int {
			return send(http.MethodPost, webhookPath, strings.NewReader(string(payload)), func(r *http.Request) {
				r.Header.Set("X-GitHub-Event", "pull_request")
				r.Header.Set("X-GitHub-Delivery", "regression-delivery-1")
				r.Header.Set("X-Hub-Signature-256", sign([]byte(testWebhookSecret), payload))
			}).StatusCode
		}
		if got := deliver(event); got != http.StatusNoContent {
			t.Fatalf("first delivery: %d", got)
		}
		if got := deliver(event); got != http.StatusNoContent {
			t.Errorf("redelivery: %d, want an idempotent 204", got)
		}
		if got := deliver(pullRequestEvent(t, 7, "synchronize", "2222222222222222222222222222222222222222")); got != http.StatusConflict {
			t.Errorf("same delivery ID, other payload: %d, want 409", got)
		}
		var deliveries int
		if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM webhook_deliveries WHERE delivery_id = 'regression-delivery-1'").Scan(&deliveries); err != nil {
			t.Fatal(err)
		}
		if deliveries != 1 {
			t.Errorf("%d recorded deliveries, want 1", deliveries)
		}
	})

	t.Run("a burst of concurrent page loads is served without errors", func(t *testing.T) {
		var wg sync.WaitGroup
		statuses := make(chan int, 200)
		for i := range 200 {
			wg.Go(func() {
				statuses <- send(http.MethodGet, "/", nil, from(fmt.Sprintf("203.0.113.%d:5000", i%250))).StatusCode
			})
		}
		wg.Wait()
		close(statuses)
		for status := range statuses {
			if status != http.StatusOK {
				t.Fatalf("a concurrent page load answered %d", status)
			}
		}
	})

	t.Run("the same delivery sent concurrently is recorded exactly once", func(t *testing.T) {
		event := pullRequestEvent(t, 8, "opened", "3333333333333333333333333333333333333333")
		var wg sync.WaitGroup
		statuses := make(chan int, 20)
		for range 20 {
			wg.Go(func() {
				statuses <- send(http.MethodPost, webhookPath, strings.NewReader(string(event)), func(r *http.Request) {
					r.Header.Set("X-GitHub-Event", "pull_request")
					r.Header.Set("X-GitHub-Delivery", "regression-concurrent-delivery")
					r.Header.Set("X-Hub-Signature-256", sign([]byte(testWebhookSecret), event))
				}).StatusCode
			})
		}
		wg.Wait()
		close(statuses)
		for status := range statuses {
			// Copies that lose the race wait on the delivery's row, then are
			// answered as redeliveries.
			if status != http.StatusNoContent {
				t.Errorf("concurrent delivery answered %d, want 204", status)
			}
		}
		var deliveries int
		if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM webhook_deliveries WHERE delivery_id = 'regression-concurrent-delivery'").Scan(&deliveries); err != nil {
			t.Fatal(err)
		}
		if deliveries != 1 {
			t.Errorf("%d recorded deliveries, want exactly 1", deliveries)
		}
	})

	t.Run("static assets are served and nothing else leaks", func(t *testing.T) {
		for path, contentType := range map[string]string{"/static/css/app.css": "text/css", "/static/js/ui.js": "text/javascript", "/favicon.ico": "image/png"} {
			response := send(http.MethodGet, path, nil, nil)
			if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), contentType) {
				t.Errorf("%s: %d %q", path, response.StatusCode, response.Header.Get("Content-Type"))
			}
		}
		for _, path := range []string{"/static/../go.mod", "/static/%2e%2e/go.mod", "/static/assets.go", "/static/missing.js"} {
			response := send(http.MethodGet, path, nil, nil)
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode == http.StatusOK || strings.Contains(string(body), "module github.com") {
				t.Errorf("%s: %d", path, response.StatusCode)
			}
		}
	})

	t.Run("operational endpoints and unknown routes are not public", func(t *testing.T) {
		for _, path := range []string{"/metrics", "/livez", "/readyz", "/debug/pprof/", "/no-such-page", "/.env", "/.git/config"} {
			if response := send(http.MethodGet, path, nil, nil); response.StatusCode != http.StatusNotFound {
				t.Errorf("%s: %d, want 404", path, response.StatusCode)
			}
		}
		if response := send(http.MethodDelete, "/auth/github/login", nil, nil); response.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("DELETE sign-in: %d, want 405", response.StatusCode)
		}
	})

	t.Run("the unavailable-preview page is public, uncached and sets no cookie", func(t *testing.T) {
		response := send(http.MethodGet, "/previews/unavailable", nil, nil)
		if response.StatusCode != http.StatusNotFound || len(response.Cookies()) != 0 || !strings.Contains(response.Header.Get("Cache-Control"), "no-store") {
			t.Errorf("unavailable page: %d, cookies %v, Cache-Control %q", response.StatusCode, response.Cookies(), response.Header.Get("Cache-Control"))
		}
	})
}

func hmacOf(key, body []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	return mac.Sum(nil)
}

func sign(key, body []byte) string { return "sha256=" + hex.EncodeToString(hmacOf(key, body)) }

// pullRequestEvent is a complete pull_request delivery for a repository
// Previa does not track, so accepting it changes nothing else.
func pullRequestEvent(t *testing.T, number int, action, sha string) []byte {
	t.Helper()
	state := "open"
	if action == "closed" {
		state = "closed"
	}
	return fmt.Appendf(nil, `{"action":%q,"number":%d,"installation":{"id":424242},"repository":{"id":98989898,"full_name":"someone/untracked"},
"pull_request":{"number":%d,"state":%q,"title":"Regression","updated_at":"2026-10-09T10:00:00Z","html_url":"https://github.com/someone/untracked/pull/%d",
"user":{"login":"someone"},"head":{"sha":%q,"ref":"feature","repo":{"id":98989898}},"base":{"repo":{"id":98989898}}}}`,
		action, number, number, state, number, sha)
}
