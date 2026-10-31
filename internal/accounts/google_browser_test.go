package accounts

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/auth"
	"github.com/ajaykathwatee/previa/internal/dashboard"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/httpx"
	"github.com/ajaykathwatee/previa/internal/platform/publicid"
	"github.com/ajaykathwatee/previa/internal/reviews"
	"github.com/ajaykathwatee/previa/internal/workspaces"
	"github.com/ajaykathwatee/previa/web"
	"github.com/alexedwards/scs/v2/memstore"
	"github.com/go-chi/chi/v5"
)

type browserGoogle struct{}

func (browserGoogle) AuthCodeURL(state, _, _ string) string {
	return "/test-google?state=" + url.QueryEscape(state)
}
func (browserGoogle) Authenticate(_ context.Context, code, nonce, verifier string) (auth.Identity, error) {
	if nonce == "" || verifier == "" {
		return auth.Identity{}, fmt.Errorf("missing proof")
	}
	return auth.Identity{GoogleSubject: "google-" + code, Email: code + "@example.test", Name: "Client Reviewer"}, nil
}
func TestGoogleReviewerBrowser(t *testing.T) {
	if os.Getenv("RUN_GOOGLE_BROWSER_TESTS") != "1" {
		t.Skip("run make test-google-browser")
	}
	f := newFixture(t)
	pr := f.id(t, `INSERT INTO pull_requests(repo_id,number,head_sha,title,head_branch,state,build_status) VALUES($1,42,repeat('1',40),'Review the checkout','feature','open','open') RETURNING id`, f.repo)
	f.exec(t, `INSERT INTO preview_publications(pull_request_id,repo_id,workspace_id,sequence,head_sha,generation,revision,container_id,image_tag) SELECT id,repo_id,$2,1,head_sha,generation,1,'fixture','fixture' FROM pull_requests WHERE id=$1`, pr, f.workspace)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	root := httpx.NewRouter(logger)
	root.Handle("/static/*", web.StaticHandler())
	root.Get("/test-github", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/auth/github/callback?"+url.Values{"state": {r.URL.Query().Get("state")}, "code": {"owner"}}.Encode(), http.StatusSeeOther)
	})
	root.Get("/test-google", func(w http.ResponseWriter, r *http.Request) {
		code := "reviewer-js"
		if cookie, err := r.Cookie("fixture_google"); err == nil {
			code = cookie.Value
		}
		http.Redirect(w, r, "/auth/google/callback?"+url.Values{"state": {r.URL.Query().Get("state")}, "code": {code}}.Encode(), http.StatusSeeOther)
	})
	server := httptest.NewUnstartedServer(root)
	origin := "http://" + server.Listener.Addr().String()
	store := memstore.NewWithCleanupInterval(0)
	defer store.StopCleanup()
	sessions := auth.NewSessionManager(store, false, logger)
	users := auth.NewHandler(config.ServerConfig{AppBaseURL: origin, SessionSecret: strings.Repeat("s", 32)}, sessions, auth.NewRepository(f.pool), browserIdentity{}, logger).WithGoogle(browserGoogle{})
	team := workspaces.NewService(workspaces.NewRepository(f.pool), nil, nil, nil)
	users.WithAvatarAudience(team).WithActivity(team)
	r := chi.NewRouter()
	r.Use(sessions.LoadAndSave, users.LoadUser, users.CSRF, publicid.New(f.pool).Middleware, workspaces.NewMiddleware(team, users, logger).Load)
	users.Mount(r)
	dashboard.Mount(r, users, auth.RequireUser)
	workspaces.NewHandler(team, users, logger, origin).Mount(r, auth.RequireUser)
	reviewService, err := reviews.NewService(f.pool, nil, reviews.Options{})
	if err != nil {
		t.Fatal(err)
	}
	reviews.NewHandler(reviewService, users, nil, logger, origin).Mount(r, auth.RequireUser)
	root.Mount("/", r)
	server.Start()
	defer server.Close()
	workspace := f.text(t, `SELECT public_id::text FROM workspaces WHERE id=$1`, f.workspace)
	repository := f.text(t, `SELECT public_id::text FROM repos WHERE id=$1`, f.repo)
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "--test", "google.test.cjs")
	cmd.Dir = "../../web/tests/browser"
	cmd.Env = append(os.Environ(), "GOOGLE_BROWSER_URL="+origin, "GOOGLE_WORKSPACE="+workspace, "GOOGLE_REVIEW_PATH=/repositories/"+repository+"/pull-requests/42/review")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Google browser: %v\n%s", err, out)
	}
	t.Log(string(out))
}
