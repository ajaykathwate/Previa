package auth

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/ajaykathwatee/previa/internal/dashboard"
	"github.com/alexedwards/scs/v2/memstore"
	"github.com/go-chi/chi/v5"
)

type fakePings struct{ calls int }

func (p *fakePings) EnqueuePing(context.Context, string) (int64, bool, error) {
	p.calls++
	return 123, false, nil
}

func TestPingRequiresLoginAndCSRF(t *testing.T) {
	provider := &fakeProvider{identity: Identity{ID: 42, Username: "octocat"}}
	b, h, sessions := newTestBrowser(t, memstore.NewWithCleanupInterval(0), &fakeUsers{}, provider)
	pings := &fakePings{}
	r := chi.NewRouter()
	r.Use(sessions.LoadAndSave, h.LoadUser, h.CSRF)
	h.Mount(r)
	dashboard.Mount(r, h, RequireUser)
	dashboard.MountPing(r, pings, RequireUser, slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.router = r
	token := func() string {
		page := b.request(http.MethodGet, "/auth/logout", "")
		// Anonymous sessions get their token from the middleware directly below.
		if page.Code != http.StatusOK {
			ctx, err := sessions.Load(t.Context(), b.cookies["previa_session"].Value)
			if err != nil {
				t.Fatal(err)
			}
			return sessions.GetString(ctx, "csrf_token")
		}
		match := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`).FindStringSubmatch(page.Body.String())
		if len(match) != 2 {
			t.Fatal("missing CSRF token")
		}
		return match[1]
	}
	b.request(http.MethodGet, "/", "")
	if w := b.request(http.MethodPost, "/queue/ping", "pr_key=demo:pr:1", map[string]string{"X-CSRF-Token": token()}); w.Code != http.StatusSeeOther || pings.calls != 0 {
		t.Fatal("anonymous ping was accepted")
	}
	state := b.loginState()
	b.request(http.MethodGet, "/auth/github/callback?code=valid&state="+state, "")
	if w := b.request(http.MethodPost, "/queue/ping", "pr_key=demo:pr:1"); w.Code != http.StatusForbidden || pings.calls != 0 {
		t.Fatal("ping without CSRF was accepted")
	}
	csrf := token()
	if w := b.request(http.MethodPost, "/queue/ping", "pr_key=bad+key", map[string]string{"X-CSRF-Token": csrf}); w.Code != http.StatusBadRequest || pings.calls != 0 {
		t.Fatal("invalid PR key was accepted")
	}
	if w := b.request(http.MethodPost, "/queue/ping", "pr_key=demo:pr:1", map[string]string{"X-CSRF-Token": csrf}); w.Code != http.StatusAccepted || pings.calls != 1 || !strings.Contains(w.Body.String(), `"job_id":123`) {
		t.Fatalf("valid ping failed: %d %s", w.Code, w.Body)
	}
}
