package auth

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"testing"

	"github.com/alexedwards/scs/v2/memstore"
)

type testGoogle struct{ calls int }

func (*testGoogle) AuthCodeURL(state, _, _ string) string {
	return "https://accounts.google.test/authorize?state=" + url.QueryEscape(state)
}
func (g *testGoogle) Authenticate(_ context.Context, _ string, nonce, verifier string) (Identity, error) {
	g.calls++
	return Identity{GoogleSubject: "subject", Email: "client@example.test", Name: nonce + verifier}, nil
}

type linkedFakeUsers struct {
	fakeUsers
	linkedTo    int64
	googleSaves int
}

func (f *linkedFakeUsers) SaveGoogle(_ context.Context, i Identity, user int64) (User, error) {
	f.linkedTo = user
	f.googleSaves++
	if user == 0 {
		f.user = User{ID: 2, Username: "Client", Email: i.Email}
	}
	f.user.GoogleSubject = i.GoogleSubject
	return f.user, nil
}
func (f *linkedFakeUsers) LinkGitHub(_ context.Context, i Identity, user int64) (User, error) {
	f.linkedTo = user
	f.user.GitHubID = i.ID
	return f.user, nil
}
func TestGoogleCallbackStateProviderAndReplay(t *testing.T) {
	for _, scenario := range []string{"valid", "wrong-provider", "wrong-state", "missing-cookie", "replay"} {
		t.Run(scenario, func(t *testing.T) {
			users := &linkedFakeUsers{}
			google := &testGoogle{}
			b, h, _ := newTestBrowser(t, memstore.NewWithCleanupInterval(0), users, &fakeProvider{identity: Identity{ID: 42, Username: "owner"}})
			h.WithGoogle(google)
			login := "/auth/google/login"
			if scenario == "wrong-provider" {
				login = "/auth/github/login"
			}
			start := b.request(http.MethodGet, login, "")
			u, err := url.Parse(start.Header().Get("Location"))
			if err != nil {
				t.Fatal(err)
			}
			state := u.Query().Get("state")
			if scenario == "wrong-state" {
				state = "bad"
			}
			if scenario == "missing-cookie" {
				delete(b.cookies, stateCookieName)
			}
			callback := "/auth/google/callback?code=code&state=" + state
			w := b.request(http.MethodGet, callback, "")
			if scenario == "valid" || scenario == "replay" {
				if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/workspaces" || users.user.GitHubID != 0 {
					t.Fatal(w.Code, w.Header())
				}
				if scenario == "replay" {
					w = b.request(http.MethodGet, callback, "")
					if w.Code != http.StatusBadRequest || google.calls != 1 {
						t.Fatal("replayed Google code exchanged")
					}
				}
			} else if w.Code != http.StatusBadRequest || google.calls != 0 || users.googleSaves != 0 {
				t.Fatal("invalid Google callback authenticated", w.Code)
			}
		})
	}
}
func TestProviderLinkingRequiresExplicitCSRFProtectedAction(t *testing.T) {
	users := &linkedFakeUsers{}
	b, h, _ := newTestBrowser(t, memstore.NewWithCleanupInterval(0), users, &fakeProvider{identity: Identity{ID: 42, Username: "owner"}})
	h.WithGoogle(&testGoogle{})
	state := b.loginState()
	b.request(http.MethodGet, "/auth/github/callback?code=code&state="+state, "")
	if w := b.request(http.MethodPost, "/auth/google/link", "csrf_token=wrong"); w.Code != http.StatusForbidden {
		t.Fatal("link accepted without CSRF")
	}
	profile := b.request(http.MethodGet, "/protected", "")
	match := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`).FindStringSubmatch(profile.Body.String())
	if len(match) != 2 {
		t.Fatal("missing CSRF")
	}
	start := b.request(http.MethodPost, "/auth/google/link", "csrf_token="+match[1])
	target, err := url.Parse(start.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	w := b.request(http.MethodGet, "/auth/google/callback?code=code&state="+target.Query().Get("state"), "")
	if w.Code != http.StatusSeeOther || users.linkedTo != 1 || users.user.ID != 1 || users.user.GitHubID != 42 {
		t.Fatal("link lost original identity", w.Code, users.user)
	}
	start = b.request(http.MethodGet, "/auth/google/login", "")
	target, err = url.Parse(start.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	b.request(http.MethodGet, "/auth/google/callback?code=code&state="+target.Query().Get("state"), "")
	if users.linkedTo != 0 {
		t.Fatal("ordinary provider login silently linked an account")
	}
}

func TestConnectingGitHubAsksWhichAccount(t *testing.T) {
	users := &linkedFakeUsers{}
	b, h, _ := newTestBrowser(t, memstore.NewWithCleanupInterval(0), users, &fakeProvider{identity: Identity{ID: 42, Username: "owner"}})
	h.WithGoogle(&testGoogle{})
	start := b.request(http.MethodGet, "/auth/google/login", "")
	target, err := url.Parse(start.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if w := b.request(http.MethodGet, "/auth/google/callback?code=code&state="+target.Query().Get("state"), ""); w.Code != http.StatusSeeOther {
		t.Fatalf("Google sign-in status = %d", w.Code)
	}
	profile := b.request(http.MethodGet, "/protected", "")
	match := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`).FindStringSubmatch(profile.Body.String())
	if len(match) != 2 {
		t.Fatal("missing CSRF")
	}
	start = b.request(http.MethodPost, "/auth/github/link", "csrf_token="+match[1])
	target, err = url.Parse(start.Header().Get("Location"))
	if err != nil || target.Query().Get("state") == "" || target.Query().Get("prompt") != "select_account" {
		t.Fatalf("connecting GitHub redirects to %q, want GitHub's account picker", start.Header().Get("Location"))
	}
}
