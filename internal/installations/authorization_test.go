package installations

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

func TestAppUserAuthorizationIdentityAccessAndPagination(t *testing.T) {
	for _, scenario := range []string{"success", "different identity", "no installation access"} {
		t.Run(scenario, func(t *testing.T) {
			pages := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				var body string
				switch r.URL.Path {
				case "/token":
					body = `{"access_token":"ephemeral-user-token","token_type":"bearer"}`
				case "/user":
					if scenario == "different identity" {
						body = `{"id":777}`
					} else {
						body = `{"id":42}`
					}
				case "/user/installations":
					switch {
					case r.URL.Query().Get("page") == "1":
						w.Header().Set("Link", fmt.Sprintf(`<http://%s/user/installations?page=2>; rel="next"`, r.Host))
						body = `{"installations":[]}`
					case scenario == "no installation access":
						body = `{"installations":[]}`
					default:
						body = `{"installations":[{"id":99}]}`
					}
				case "/user/installations/99/repositories":
					pages++
					if r.URL.Query().Get("page") == "1" {
						w.Header().Set("Link", fmt.Sprintf(`<http://%s/user/installations/99/repositories?page=2>; rel="next"`, r.Host))
						body = `{"repositories":[{"id":456,"full_name":"owner/first"}]}`
					} else {
						body = `{"repositories":[{"id":789,"full_name":"owner/second"}]}`
					}
				default:
					t.Errorf("unexpected API path %s", r.URL.Path)
					w.WriteHeader(404)
					return
				}
				if r.URL.Path != "/token" && r.Header.Get("Authorization") != "Bearer ephemeral-user-token" {
					t.Error("user verification not authenticated")
				}
				if _, err := fmt.Fprint(w, body); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			// Route only GitHub API traffic to the local fixture, retaining OAuth endpoint configuration.
			client := server.Client()
			client.Transport = redirectAPI{base: server.URL, underlying: client.Transport}
			authorization := &GitHubAuthorization{oauth: oauth2.Config{ClientID: "app-id", ClientSecret: "app-secret", Endpoint: oauth2.Endpoint{AuthURL: server.URL + "/authorize", TokenURL: server.URL + "/token"}}, client: client}
			repos, err := authorization.Verify(t.Context(), "code", 42, 99)
			if scenario == "success" {
				if err != nil || len(repos) != 2 || pages != 2 {
					t.Fatalf("pagination/verification failed: %v", err)
				}
			} else if err == nil {
				t.Fatal("unauthorized installation accepted")
			}
		})
	}
}

type redirectAPI struct {
	base       string
	underlying http.RoundTripper
}

func (t redirectAPI) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	if strings.Contains(clone.URL.Host, "api.github.com") {
		target := strings.TrimPrefix(t.base, "http://")
		clone.URL.Scheme = "http"
		clone.URL.Host = target
	}
	transport := t.underlying
	if transport == nil {
		transport = http.DefaultTransport
	}
	return transport.RoundTrip(clone)
}
