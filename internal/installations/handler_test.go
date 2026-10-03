package installations

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/auth"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/alexedwards/scs/v2/memstore"
	"github.com/go-chi/chi/v5"
)

type fixtureUsers struct{}

func (fixtureUsers) UpsertUser(context.Context, auth.Identity) (auth.User, error) {
	return auth.User{}, nil
}
func (fixtureUsers) GetUser(_ context.Context, id int64) (auth.User, error) {
	return auth.User{ID: id, GitHubID: 42, Username: "owner"}, nil
}

type fixtureOAuth struct{}

func (fixtureOAuth) AuthCodeURL(string) string { return "" }
func (fixtureOAuth) Authenticate(context.Context, string) (auth.Identity, error) {
	return auth.Identity{}, nil
}
func TestInstallRedirectCallbackStateAndSession(t *testing.T) {
	for _, scenario := range []string{"success", "forged setup state", "forged authorization state", "replayed callback", "expired", "different session user", "declined", "approval requested", "missing installation", "anonymous", "CSRF"} {
		t.Run(scenario, func(t *testing.T) {
			service, store, _, _ := testService(t)
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			memory := memstore.NewWithCleanupInterval(0)
			sessions := auth.NewSessionManager(memory, false, logger)
			authHandler := auth.NewHandler(config.ServerConfig{AppBaseURL: "http://localhost", SessionSecret: strings.Repeat("s", 32)}, sessions, fixtureUsers{}, fixtureOAuth{}, logger)
			handler := NewHandler(service, authHandler, sessions, "test-app", logger)
			router := chi.NewRouter()
			router.Use(sessions.LoadAndSave, authHandler.LoadUser, authHandler.CSRF)
			router.Get("/fixture-login", func(w http.ResponseWriter, r *http.Request) {
				sessions.Put(r.Context(), "user_id", int64(1))
				sessions.Put(r.Context(), "csrf_token", "test-csrf")
				w.WriteHeader(200)
			})
			router.Get("/fixture-expire", func(w http.ResponseWriter, r *http.Request) {
				sessions.Put(r.Context(), "installation_expires", time.Now().Add(-time.Minute).Unix())
				w.WriteHeader(200)
			})
			router.Get("/fixture-switch", func(w http.ResponseWriter, r *http.Request) {
				sessions.Put(r.Context(), "user_id", int64(2))
				w.WriteHeader(200)
			})
			handler.Mount(router, auth.RequireUser)
			server := httptest.NewServer(router)
			defer server.Close()
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			request := func(method, path, body string) (int, string) {
				t.Helper()
				req, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				if method == "POST" {
					req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				}
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				if err := resp.Body.Close(); err != nil {
					t.Fatal(err)
				}
				return resp.StatusCode, resp.Header.Get("Location")
			}
			if scenario == "anonymous" {
				code, _ := request("POST", "/installations/start", "")
				if code != 403 {
					t.Fatal("anonymous unsafe request bypassed CSRF")
				}
				return
			}
			request("GET", "/fixture-login", "")
			if scenario == "CSRF" {
				code, _ := request("POST", "/installations/start", "csrf_token=wrong")
				if code != 403 {
					t.Fatal("installation start lacked CSRF")
				}
				return
			}
			code, location := request("POST", "/installations/start", "csrf_token=test-csrf")
			if code != 303 || !strings.HasPrefix(location, "https://github.com/apps/test-app/installations/new?state=") {
				t.Fatalf("start: %d %s", code, location)
			}
			parsed, err := url.Parse(location)
			if err != nil {
				t.Fatal(err)
			}
			state := parsed.Query().Get("state")
			if scenario == "forged setup state" {
				state = "forged"
			}
			if scenario == "expired" {
				request("GET", "/fixture-expire", "")
			}
			if scenario == "different session user" {
				request("GET", "/fixture-switch", "")
			}
			setup := "/installations/callback?installation_id=99&setup_action=install&state=" + state
			if scenario == "approval requested" {
				setup = strings.Replace(setup, "setup_action=install", "setup_action=request", 1)
			}
			if scenario == "missing installation" {
				setup = strings.Replace(setup, "installation_id=99", "installation_id=invalid", 1)
			}
			code, location = request("GET", setup, "")
			switch scenario {
			case "forged setup state", "expired", "different session user", "missing installation":
				if code != 400 || store.linked {
					t.Fatal("unsafe setup accepted")
				}
				return
			case "approval requested":
				if code != 202 || store.linked {
					t.Fatal("request linked before approval")
				}
				return
			}
			if code != 303 {
				t.Fatalf("setup status %d", code)
			}
			parsed, err = url.Parse(location)
			if err != nil {
				t.Fatal(err)
			}
			state = parsed.Query().Get("state")
			if scenario == "forged authorization state" {
				state = "forged"
			}
			callback := "/installations/authorize/callback?code=valid&state=" + state
			if scenario == "declined" {
				callback += "&error=access_denied"
			}
			code, location = request("GET", callback, "")
			if scenario == "forged authorization state" || scenario == "declined" {
				if code != 400 || store.linked {
					t.Fatal("unsafe authorization accepted")
				}
				return
			}
			if code != 303 || location != "/dashboard" || !store.linked {
				t.Fatalf("link did not finish: %d %s", code, location)
			}
			if scenario == "replayed callback" {
				if code, _ := request("GET", callback, ""); code != 400 {
					t.Fatal("callback replay accepted")
				}
			}
		})
	}
}
