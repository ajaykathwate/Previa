package onboarding

import (
	"context"
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
	"github.com/ajaykathwatee/previa/internal/platform/httpx"
	"github.com/ajaykathwatee/previa/internal/platform/realtime"
	"github.com/ajaykathwatee/previa/internal/platform/scope"
	"github.com/ajaykathwatee/previa/web"
	dashboardpages "github.com/ajaykathwatee/previa/web/pages/dashboard"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// TestSetupBrowser drives the Setup tab in Chromium against PostgreSQL with
// the real session, CSRF, workspace and live-update code. Test-only routes
// stand in for the worker and GitHub (a finished check, a pushed commit, a
// build's progress, a deployment). Run it with make test-setup-browser.
func TestSetupBrowser(t *testing.T) {
	if os.Getenv("RUN_SETUP_BROWSER_TESTS") != "1" {
		t.Skip("run make test-setup-browser with TEST_DATABASE_URL")
	}
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	f.addPR(t, 43, strings.Repeat("a", 40), -2*time.Hour)
	f.secrets.names = []string{"API_KEY", "STRIPE_TEST_KEY"}
	logger := slogDiscard()
	hub := realtime.NewHub()
	defer hub.Close()
	go realtime.Listen(ctx, func(ctx context.Context) (*pgx.Conn, error) {
		return pgx.ConnectConfig(ctx, f.pool.Config().ConnConfig.Copy())
	}, hub, logger)

	files := &fakeFiles{tree: readyTree()}
	run := runner(t, f, files)
	apply := func(w http.ResponseWriter, r *http.Request, query string, args ...any) bool {
		if _, err := f.pool.Exec(r.Context(), query, args...); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return false
		}
		return true
	}
	root := httpx.NewRouter(logger)
	root.Handle("/static/*", web.StaticHandler())
	root.Get("/__fixture/identity", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "fixture_identity", Value: r.URL.Query().Get("login"), Path: "/", HttpOnly: true})
		w.WriteHeader(http.StatusNoContent)
	})
	root.Get("/test-github", func(w http.ResponseWriter, r *http.Request) {
		login := "owner"
		if c, err := r.Cookie("fixture_identity"); err == nil {
			login = c.Value
		}
		http.Redirect(w, r, "/auth/github/callback?"+url.Values{"state": {r.URL.Query().Get("state")}, "code": {login}}.Encode(), http.StatusSeeOther)
	})
	// The worker finishes the latest requested check with the named tree.
	root.Post("/__fixture/check", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("tree") {
		case "missing":
			files.tree = &fakeTree{files: map[string]string{"Dockerfile": "FROM scratch"}}
		default:
			files.tree = &fakeTree{files: map[string]string{
				".previewrc.yaml": "port: 8080\nhealth_check_path: /healthz\nservices: { postgres: 17 }\nsetup: [\"bin/migrate\", \"up\"]\nenv:\n  - LOG_LEVEL=debug\n",
				"Dockerfile":      "FROM scratch"}}
		}
		if err := run.Run(r.Context(), f.jobs.last(), true); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// A fix pushed to the pull request's branch.
	root.Post("/__fixture/push", func(w http.ResponseWriter, r *http.Request) {
		if apply(w, r, "UPDATE pull_requests SET head_sha=$2,generation=generation+1,source_updated_at=now() WHERE id=$1", f.pr, strings.Repeat("b", 40)) {
			w.WriteHeader(http.StatusNoContent)
		}
	})
	// The Build action: the real one is covered in internal/pullrequests; this
	// checks what the Setup tab posts and records a queued request.
	root.Post("/repositories/{repoID}/pull-requests/{number}/build", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.PostForm.Get("csrf_token") == "" || r.PostForm.Get("return") != "setup" ||
			r.PostForm.Get("action") != "build" || r.PostForm.Get("request_key") == "" || !strings.HasPrefix(r.PostForm.Get("workspace_query"), "pr=") {
			http.Error(w, "unexpected build form: "+r.PostForm.Encode(), http.StatusBadRequest)
			return
		}
		if !apply(w, r, `INSERT INTO build_intents(pull_request_id,revision,base_revision,generation,head_sha,source,request_action,idempotency_key,state,available_at,requested_by)
			SELECT id,1,0,generation,head_sha,'manual','build',$2,'queued',now(),$3 FROM pull_requests WHERE id=$1`, f.pr, r.PostForm.Get("request_key"), f.owner) {
			return
		}
		setup := "/repositories/" + chi.URLParam(r, "repoID") + "/setup?" + r.PostForm.Get("workspace_query") + "&saved=build"
		http.Redirect(w, r, setup, http.StatusSeeOther)
	})
	root.Post("/__fixture/building", func(w http.ResponseWriter, r *http.Request) {
		if apply(w, r, "UPDATE build_intents SET state='running',started_at=now() WHERE pull_request_id=$1", f.pr) {
			w.WriteHeader(http.StatusNoContent)
		}
	})
	root.Post("/__fixture/deploy", func(w http.ResponseWriter, r *http.Request) {
		if !apply(w, r, "UPDATE build_intents SET state='succeeded',completed_at=now() WHERE pull_request_id=$1", f.pr) {
			return
		}
		if apply(w, r, `INSERT INTO environments(pull_request_id,subdomain,status,head_sha,applied_generation,applied_revision)
			SELECT id,'pr-42-fixture','running',head_sha,generation,1 FROM pull_requests WHERE id=$1`, f.pr) {
			w.WriteHeader(http.StatusNoContent)
		}
	})
	server := httptest.NewUnstartedServer(root)
	origin := "http://" + server.Listener.Addr().String()
	root.Mount("/", f.routerWith(t, origin, hub, func(r chi.Router, users *auth.Handler) {
		dashboard.MountWithSummary(r, users, auth.RequireUser, dashboard.Options{Identity: users.UserIDs, Summary: func(ctx context.Context, userID int64) (dashboardpages.Summary, error) {
			selection := scope.Current(ctx)
			j, err := f.service.Journey(ctx, selection.WorkspaceID, userID)
			if err != nil {
				return dashboardpages.Summary{}, err
			}
			journey := dashboardpages.Journey{Manager: selection.Role == "owner" || selection.Role == "admin", Repositories: j.Repositories, Available: j.Available,
				Checked: j.Checked, Published: j.Published, Opened: j.Opened, FocusID: j.FocusID, FocusName: j.FocusName}
			for _, m := range j.Managers {
				journey.Managers = append(journey.Managers, dashboardpages.Person{Login: m.Login, Name: m.Name})
			}
			return dashboardpages.Summary{Repositories: j.Repositories, Available: j.Available, Workers: dashboardpages.Workers{Execution: true, Discovery: true}, Journey: journey}, nil
		}})
	}))
	server.Start()
	defer server.Close()
	repo := f.public(t, "repository", f.repo, 0)
	cmd := exec.CommandContext(ctx, "node", "--test", "onboarding.test.cjs")
	cmd.Dir = "../../web/tests/browser"
	cmd.Env = append(os.Environ(), "SETUP_BROWSER_URL="+server.URL, "SETUP_PATH=/repositories/"+repo+"/setup",
		"SETUP_PR42="+f.public(t, "pull-request", 42, f.repo), "SETUP_PR43="+f.public(t, "pull-request", 43, f.repo))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("setup browser checks: %v\n%s", err, output)
	}
	t.Log(string(output))
}
