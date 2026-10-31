package reviews

import (
	"context"
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

	"github.com/ajaykathwatee/previa/internal/platform/httpx"
	"github.com/ajaykathwatee/previa/internal/platform/realtime"
	"github.com/ajaykathwatee/previa/web"
	"github.com/jackc/pgx/v5"
)

// TestReviewBrowser drives the review page in Chromium against PostgreSQL
// with the real session, CSRF, workspace guard and live updates. Test-only
// routes stand in for a worker publishing a deployment and a teammate
// replying. Run it with make test-review-browser.
func TestReviewBrowser(t *testing.T) {
	if os.Getenv("RUN_REVIEW_BROWSER_TESTS") != "1" {
		t.Skip("run make test-review-browser with TEST_DATABASE_URL")
	}
	f := newFixture(t)
	f.service = f.sealing(t, keyring(t, testKey("1"), "1"))
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	if _, err := f.pool.Exec(ctx, "UPDATE pull_requests SET title='Validate the checkout address',description=$2 WHERE id=$1", f.pr,
		"Checkout used to crash on an empty address.\n\nThis adds inline validation and keeps the cart.\n\n<script>window.injected=true</script>"); err != nil {
		t.Fatal(err)
	}
	f.publish(t, f.pr, shaA)
	// Deployment 1 ran a setup command with a database and built successfully,
	// so the page shows its setup and offers Reset test data.
	for _, query := range []string{
		`INSERT INTO preview_components(pull_request_id, generation, revision, services, setup, setup_ms) VALUES ($1, 1, 1, '[{"name":"postgres"}]', true, 12400)`,
		`INSERT INTO build_intents(pull_request_id, revision, base_revision, generation, head_sha, source, request_action, requested_by, idempotency_key, state, available_at, started_at, completed_at)
			SELECT id, 1, 0, generation, head_sha, 'manual', 'build', $2, 'key-browser-00000001', 'succeeded', now(), now(), now() FROM pull_requests WHERE id=$1`,
	} {
		args := []any{f.pr}
		if strings.Contains(query, "$2") {
			args = append(args, f.owner)
		}
		if _, err := f.pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := realtime.NewHub()
	defer hub.Close()
	go realtime.Listen(ctx, func(ctx context.Context) (*pgx.Conn, error) {
		return pgx.ConnectConfig(ctx, f.pool.Config().ConnConfig.Copy())
	}, hub, logger)
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
	// A new commit is pushed and a worker publishes it as the next deployment.
	root.Post("/__fixture/publish", func(w http.ResponseWriter, r *http.Request) {
		if _, err := f.pool.Exec(r.Context(), "UPDATE pull_requests SET head_sha=$2, generation=generation+1 WHERE id=$1", f.pr, shaB); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := f.deploy(r.Context(), f.pr, shaB); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// Something else changes on the repository: open review pages refresh.
	root.Post("/__fixture/notify", func(w http.ResponseWriter, r *http.Request) {
		if _, err := f.pool.Exec(r.Context(), "SELECT previa_notify_repository($1, 'fixture')", f.repo); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// A teammate replies to the first finding in another browser.
	root.Post("/__fixture/teammate-reply", func(w http.ResponseWriter, r *http.Request) {
		if _, err := f.service.Reply(r.Context(), f.as(f.dev), 1, ReplyInput{Body: "Thanks, looking at it now.", RequestKey: NewRequestKey()}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewUnstartedServer(root)
	origin := "http://" + server.Listener.Addr().String()
	root.Mount("/", f.router(t, origin, hub))
	server.Start()
	defer server.Close()
	cmd := exec.CommandContext(ctx, "node", "--test", "--test-concurrency=1", "reviews.test.cjs")
	cmd.Dir = "../../web/tests/browser"
	cmd.Env = append(os.Environ(), "REVIEW_BROWSER_URL="+server.URL, "REVIEW_PATH="+f.reviewPath(t))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("review browser checks: %v\n%s", err, output)
	}
	t.Log(string(output))
}
