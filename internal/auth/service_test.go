package auth

import (
	"context"
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
	for _, scenario := range []string{"success", "exchange-down", "identity-down", "invalid-identity", "email-down", "unverified-email"} {
		t.Run(scenario, func(t *testing.T) {
			g := NewGitHubOAuth(config.ServerConfig{AppBaseURL: "http://localhost:8080", GitHubOAuthClientID: "client-id", GitHubOAuthClientSecret: "client-secret"})
			if g.oauth.RedirectURL != "http://localhost:8080/auth/github/callback" || strings.Join(g.oauth.Scopes, ",") != "user:email" {
				t.Fatal("unexpected redirect or excessive OAuth scopes")
			}
			calls := 0
			g.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				body := `{"access_token":"temporary-token","token_type":"bearer"}`
				switch {
				case r.URL.Host == "github.com" && r.URL.Path == "/login/oauth/access_token":
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
				case r.URL.Host == "api.github.com" && r.URL.Path == "/user":
					if r.Header.Get("Authorization") != "Bearer temporary-token" {
						t.Fatal("identity request did not use exchanged token")
					}
					if scenario == "identity-down" {
						return nil, errors.New("identity unavailable")
					}
					body = `{"id":42,"login":"octocat","name":"Octo Cat","bio":"Builds things","company":"Acme","location":"Earth","created_at":"2011-01-01T00:00:00Z","avatar_url":"https://example.com/avatar"}`
					if scenario == "invalid-identity" {
						body = `{"id":0,"login":""}`
					}
				case r.URL.Host == "api.github.com" && r.URL.Path == "/user/emails":
					body = `[{"email":"other@example.com","primary":false,"verified":true},{"email":"octo@example.com","primary":true,"verified":true}]`
					if scenario == "email-down" {
						return nil, errors.New("email unavailable")
					}
					if scenario == "unverified-email" {
						body = `[{"email":"unverified@example.com","primary":true,"verified":false}]`
					}
				default:
					t.Fatalf("unexpected request: %s", r.URL)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			identity, err := g.Authenticate(t.Context(), "valid-code")
			if scenario == "exchange-down" || scenario == "identity-down" || scenario == "invalid-identity" {
				if err == nil || identity != (Identity{}) {
					t.Fatal("provider failure returned a partial identity")
				}
				return
			}
			if err != nil || calls != 3 || identity.ID != 42 || identity.Username != "octocat" || identity.AvatarURL != "https://example.com/avatar" {
				t.Fatalf("exchange/profile flow failed: %+v %v, %d calls", identity, err, calls)
			}
			if identity.Name != "Octo Cat" || identity.ProfileURL != "https://github.com/octocat" || identity.Company != "Acme" || identity.GitHubCreatedAt.IsZero() {
				t.Fatal("lost GitHub metadata")
			}
			wantEmail := "octo@example.com"
			if scenario == "email-down" || scenario == "unverified-email" {
				wantEmail = ""
			}
			if identity.Email != wantEmail || identity.EmailSyncError != (scenario == "email-down") {
				t.Fatalf("unsafe email result: %+v", identity)
			}
		})
	}
}

func TestPersonalNameDoesNotReplaceGitHubPermissionIdentity(t *testing.T) {
	ctx := context.WithValue(t.Context(), userKey, User{Username: "octocat", FirstName: "Ada", LastName: "Lovelace"})
	h := &Handler{}
	if name, ok := h.Username(ctx); !ok || name != "Ada Lovelace" {
		t.Fatal("display name missing")
	}
	if login, ok := h.GitHubUsername(ctx); !ok || login != "octocat" {
		t.Fatal("custom name leaked into GitHub permissions")
	}
}
