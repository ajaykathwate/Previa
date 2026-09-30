package auth

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ajaykathwatee/previa/internal/platform/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGitHubExchangeAndIdentity(t *testing.T) {
	for _, scenario := range []string{"success", "exchange-down", "identity-down", "invalid-identity"} {
		t.Run(scenario, func(t *testing.T) {
			g := NewGitHubOAuth(config.Config{AppBaseURL: "http://localhost:8080", GitHubOAuthClientID: "client-id", GitHubOAuthClientSecret: "client-secret"})
			if g.oauth.RedirectURL != "http://localhost:8080/auth/github/callback" || len(g.oauth.Scopes) != 0 {
				t.Fatal("unexpected redirect or excessive OAuth scopes")
			}
			calls := 0
			g.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				body := `{"access_token":"temporary-token","token_type":"bearer"}`
				if r.URL.Host == "github.com" && r.URL.Path == "/login/oauth/access_token" {
					if err := r.ParseForm(); err != nil {
						t.Fatal(err)
					}
					clientID, secret, basic := r.BasicAuth()
					if !basic {
						clientID, secret = r.Form.Get("client_id"), r.Form.Get("client_secret")
					}
					if r.Form.Get("code") != "valid-code" || clientID != "client-id" || secret != "client-secret" {
						t.Fatal("OAuth exchange omitted expected credentials/code")
					}
					if scenario == "exchange-down" {
						return nil, errors.New("exchange unavailable")
					}
				} else if r.URL.Host == "api.github.com" && r.URL.Path == "/user" {
					if r.Header.Get("Authorization") != "Bearer temporary-token" {
						t.Fatal("identity request did not use exchanged token")
					}
					if scenario == "identity-down" {
						return nil, errors.New("identity unavailable")
					}
					body = `{"id":42,"login":"octocat","avatar_url":"https://example.com/avatar"}`
					if scenario == "invalid-identity" {
						body = `{"id":0,"login":""}`
					}
				} else {
					t.Fatalf("unexpected request: %s", r.URL)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			identity, err := g.Authenticate(t.Context(), "valid-code")
			if scenario != "success" {
				if err == nil || identity != (Identity{}) {
					t.Fatal("provider failure returned a partial identity")
				}
				return
			}
			if err != nil || calls != 2 || identity.ID != 42 || identity.Username != "octocat" || identity.AvatarURL != "https://example.com/avatar" {
				t.Fatalf("exchange/profile flow failed: %+v %v, %d calls", identity, err, calls)
			}
		})
	}
}
