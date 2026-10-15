package workspaces

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/httpx"
	"github.com/ajaykathwatee/previa/internal/platform/publicid"
	"github.com/ajaykathwatee/previa/web"
)

// The team UI uses real PostgreSQL, session/CSRF middleware and feature routes;
// only external GitHub identity/permission responses are fixtures.
func TestWorkspaceBrowser(t *testing.T) {
	if os.Getenv("RUN_WORKSPACE_BROWSER_TESTS") != "1" {
		t.Skip("run make test-workspace-browser with TEST_DATABASE_URL")
	}
	f := newFixture(t)
	if _, err := f.pool.Exec(t.Context(), "INSERT INTO repos(installation_id,github_repo_id,full_name) SELECT installation_id,400+n,'acme/service-'||lpad(n::text,2,'0') FROM repos CROSS JOIN generate_series(1,28) AS n WHERE id=$1", f.repo); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), "UPDATE pull_requests SET title='Make review previews easier to inspect',author='contributor',head_branch='feature/review-previews',description=$2 WHERE id=$1", f.pr, "Why this change\n\nA focused preview workflow for the whole team.\n\n<script>window.descriptionExecuted=true</script>"); err != nil {
		t.Fatal(err)
	}
	seedPreview(t, f)
	f.invite(t, "admin", "admin", f.admin, 102, nil)
	f.invite(t, "dev", "developer", f.dev, 103, []int64{f.repo})
	f.invite(t, "reviewer", "reviewer", f.reviewer, 104, []int64{f.repo})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := httpx.NewRouter(logger)
	router.Handle("/static/*", web.StaticHandler())
	router.Get("/__fixture/identity", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "fixture_identity", Value: r.URL.Query().Get("login"), Path: "/", HttpOnly: true})
		w.WriteHeader(http.StatusNoContent)
	})
	router.Post("/__fixture/failed-build", func(w http.ResponseWriter, r *http.Request) {
		_, err := f.pool.Exec(r.Context(), `INSERT INTO build_intents(pull_request_id,revision,base_revision,generation,head_sha,source,request_action,idempotency_key,state,available_at,started_at,completed_at,execution_error_code)
      SELECT id,1,0,generation,head_sha,'automatic','automatic','browser-failed-build','failed',now(),now(),now(),'worker_failed' FROM pull_requests WHERE id=$1`, f.pr)
		if err != nil {
			http.Error(w, "fixture failed", 500)
			return
		}
		if _, err := f.pool.Exec(r.Context(), "UPDATE environments SET status='publishing' WHERE pull_request_id=$1", f.pr); err != nil {
			http.Error(w, "fixture failed", 500)
			return
		}
		w.WriteHeader(204)
	})
	router.Get("/test-github", func(w http.ResponseWriter, r *http.Request) {
		login := "owner"
		if c, err := r.Cookie("fixture_identity"); err == nil {
			login = c.Value
		}
		http.Redirect(w, r, "/auth/github/callback?"+url.Values{"state": {r.URL.Query().Get("state")}, "code": {login}}.Encode(), http.StatusSeeOther)
	})
	server := httptest.NewUnstartedServer(router)
	origin := "http://" + server.Listener.Addr().String()
	router.Mount("/", configuredFixtureRouter(t, f, origin, true))
	server.Start()
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "--test", "workspaces.test.cjs")
	cmd.Dir = "../../web/tests/browser"
	catalog := publicid.New(f.pool)
	workspaceID, err := catalog.Public(t.Context(), "workspace", f.wid, 0)
	if err != nil {
		t.Fatal(err)
	}
	developerID, err := catalog.Public(t.Context(), "user", f.dev, 0)
	if err != nil {
		t.Fatal(err)
	}
	repositoryID, err := catalog.Public(t.Context(), "repository", f.repo, 0)
	if err != nil {
		t.Fatal(err)
	}
	pullRequestID, err := catalog.Public(t.Context(), "pull-request", 1, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), "WORKSPACE_LOGO_PNG="+base64.StdEncoding.EncodeToString(logoPNG(t, 64, 64)), "WORKSPACE_BROWSER_URL="+server.URL, "WORKSPACE_TEST_ID="+workspaceID, "WORKSPACE_DEV_ID="+developerID, "WORKSPACE_REPO_COUNT=30", "WORKSPACE_PR_PATH=/repositories/"+repositoryID+"/pull-requests/"+pullRequestID)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("workspace browser checks: %v\n%s", err, output)
	}
	t.Log(string(output))
}
