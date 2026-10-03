package pullrequests_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/installations"
	"github.com/ajaykathwatee/previa/internal/platform/queue"
	pr "github.com/ajaykathwatee/previa/internal/pullrequests"
	"github.com/ajaykathwatee/previa/internal/webhooks"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"
)

type fixture struct {
	pool           *pgxpool.Pool
	repoID, userID int64
	producer       *river.Client[pgx.Tx]
	hook           http.Handler
	log            *slog.Logger
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for isolated database tests")
	}
	admin, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("workspace_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		defer admin.Close()
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = name
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	files, err := filepath.Glob("../../db/migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(b), "-- +goose Down")
		if _, err := pool.Exec(t.Context(), up); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
	}
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrator.Migrate(t.Context(), rivermigrate.DirectionUp, nil); err != nil {
		t.Fatal(err)
	}
	f := &fixture{pool: pool, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := pool.QueryRow(t.Context(), `INSERT INTO users(github_id,username)VALUES(42,'owner')RETURNING id`).Scan(&f.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO installations(github_installation_id,owner_user_id,account_login)VALUES(99,$1,'owner')`, f.userID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `INSERT INTO repos(installation_id,github_repo_id,full_name)SELECT id,100,'owner/repo' FROM installations RETURNING id`).Scan(&f.repoID); err != nil {
		t.Fatal(err)
	}
	f.producer, err = queue.NewProducer(pool, f.log)
	if err != nil {
		t.Fatal(err)
	}
	f.hook = webhooks.NewHandler("fixture-secret", webhooks.NewDeliveryService(pool, environments.NewBroker(pool, f.producer), nil), f.log)
	return f
}
func payload(action, sha, state, at string) string {
	return fmt.Sprintf(`{"action":%q,"installation":{"id":99},"repository":{"id":100},"pull_request":{"number":7,"title":"Improve preview","state":%q,"updated_at":%q,"user":{"login":"contributor"},"html_url":"https://github.com/owner/repo/pull/7","head":{"sha":%q,"ref":"feature","repo":{"id":101}}}}`, action, state, at, sha)
}
func send(h http.Handler, id, kind, body string) int {
	req := httptest.NewRequest("POST", "/webhooks/github", strings.NewReader(body))
	req.Header.Set("X-GitHub-Delivery", id)
	req.Header.Set("X-GitHub-Event", kind)
	mac := hmac.New(sha256.New, []byte("fixture-secret"))
	mac.Write([]byte(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code
}
func (f *fixture) count(t *testing.T, sql string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(t.Context(), sql).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func TestWebhookAtomicDedupSourceOrderingAndManual(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	if status := send(f.hook, "unsupported", "issue_comment", `{"action":"created"}`); status != 204 {
		t.Fatal("valid unsupported event rejected")
	}
	if status := send(f.hook, "missing-action", "pull_request", `{}`); status != 400 {
		t.Fatal("malformed supported payload accepted")
	}
	if f.count(t, "SELECT count(*) FROM webhook_deliveries") != 0 {
		t.Fatal("unsupported/invalid event persisted effects")
	}
	body := payload("opened", "sha-a", "open", "2026-10-03T10:00:00Z")
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if status := send(f.hook, "same-id", "pull_request", body); status != 204 {
				t.Errorf("concurrent delivery: %d", status)
			}
		}()
	}
	wg.Wait()
	if f.count(t, "SELECT count(*) FROM webhook_deliveries") != 1 || f.count(t, "SELECT generation FROM pull_requests") != 1 || f.count(t, "SELECT count(*) FROM build_intents") != 0 || f.count(t, "SELECT count(*) FROM environments") != 0 || f.count(t, "SELECT count(*) FROM river_job") != 0 {
		t.Fatal("duplicate or Manual observation created extra effects")
	}
	if status := send(f.hook, "same-id", "pull_request", payload("synchronize", "different", "open", "2026-10-03T10:01:00Z")); status != 409 {
		t.Fatal(status)
	}
	for _, e := range []struct{ id, action, sha, state, at string }{
		{"newer", "synchronize", "sha-b", "open", "2026-10-03T10:02:00Z"},
		{"stale", "synchronize", "sha-a", "open", "2026-10-03T10:01:00Z"},
		{"closed", "closed", "sha-b", "closed", "2026-10-03T10:03:00Z"},
		{"late", "synchronize", "sha-a", "open", "2026-10-03T10:02:00Z"},
	} {
		if status := send(f.hook, e.id, "pull_request", payload(e.action, e.sha, e.state, e.at)); status != 204 {
			t.Fatal(status)
		}
	}
	var sha, state string
	var fork bool
	if err := f.pool.QueryRow(ctx, "SELECT head_sha,state,is_fork FROM pull_requests").Scan(&sha, &state, &fork); err != nil {
		t.Fatal(err)
	}
	if sha != "sha-b" || state != "closed" || !fork {
		t.Fatalf("stale state/fork lost: %s %s %v", sha, state, fork)
	}
	if f.count(t, "SELECT generation FROM pull_requests") != 3 {
		t.Fatal("stale event changed generation")
	}
	// An absent runtime leaves durable coordination, never an executable build.
	if f.count(t, "SELECT count(*) FROM river_job WHERE kind IN ('build','teardown')") != 0 {
		t.Fatal("executable work queued")
	}
}

type brokenObserver struct{}

func (brokenObserver) ObserveTx(context.Context, pgx.Tx, environments.Request) (environments.Request, error) {
	return environments.Request{}, errors.New("transaction failure")
}
func TestWebhookRollbackCanRetrySameDelivery(t *testing.T) {
	f := setup(t)
	body := payload("opened", "sha-a", "open", "2026-10-03T10:00:00Z")
	bad := webhooks.NewHandler("fixture-secret", webhooks.NewDeliveryService(f.pool, brokenObserver{}, nil), f.log)
	if send(bad, "retry-me", "pull_request", body) != 503 {
		t.Fatal("failure acknowledged")
	}
	if f.count(t, "SELECT count(*) FROM webhook_deliveries") != 0 || f.count(t, "SELECT count(*) FROM pull_requests") != 0 {
		t.Fatal("partial commit")
	}
	if send(f.hook, "retry-me", "pull_request", body) != 204 {
		t.Fatal("retry failed")
	}
}
func TestInstallationRevocationIsDurableAndAdditionCannotUndoTie(t *testing.T) {
	f := setup(t)
	for _, action := range []string{"removed", "added"} {
		key := "repositories_" + action
		b := fmt.Sprintf(`{"action":%q,"installation":{"id":99,"updated_at":"2026-10-03T10:00:00Z","account":{"login":"owner"}},%q:[{"id":100,"full_name":"owner/repo"}]}`, action, key)
		if status := send(f.hook, action, "installation_repositories", b); status != 204 {
			t.Fatal(status)
		}
	}
	if f.count(t, "SELECT count(*) FROM repos WHERE accessible") != 0 || f.count(t, "SELECT count(*) FROM installations WHERE cleanup_pending") != 1 {
		t.Fatal("revocation lost or cleanup falsely completed")
	}
	b := `{"action":"deleted","installation":{"id":99,"updated_at":"2026-10-03T10:01:00Z","account":{"login":"owner"}}}`
	if send(f.hook, "deleted", "installation", b) != 204 {
		t.Fatal("delete failed")
	}
	// Newer created must not resurrect this ID.
	b = `{"action":"created","installation":{"id":99,"updated_at":"2026-10-03T10:02:00Z","account":{"login":"owner"}},"repositories":[{"id":100,"full_name":"owner/repo"}]}`
	if send(f.hook, "created", "installation", b) != 204 || f.count(t, "SELECT count(*) FROM installations WHERE status='inactive'") != 1 {
		t.Fatal("installation resurrected")
	}
}

type sourceFunc func(context.Context, int64, int64, int32) ([]pr.Observation, int32, error)

func (f sourceFunc) Page(ctx context.Context, i, r int64, p int32) ([]pr.Observation, int32, error) {
	return f(ctx, i, r, p)
}
func observation(n int32) pr.Observation {
	return pr.Observation{Number: n, SHA: fmt.Sprintf("sha-%d", n), State: "open", Title: fmt.Sprintf("Change %d", n), Author: "owner", Branch: "branch", HeadRepoID: 100, UpdatedAt: time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)}
}
func TestDiscoveryPaginationCoalescingAndOwnership(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	policy, _ := installations.NewBuildPolicyService(installations.NewRepository(f.pool))
	if _, err := policy.Set(ctx, f.userID, f.repoID, installations.AutomaticBuilds); err != nil {
		t.Fatal(err)
	}
	source := sourceFunc(func(_ context.Context, i, r int64, p int32) ([]pr.Observation, int32, error) {
		if i != 99 || r != 100 {
			t.Fatal("wrong installation identity")
		}
		if p == 1 {
			var rows []pr.Observation
			for n := int32(1); n <= 100; n++ {
				rows = append(rows, observation(n))
			}
			return rows, 2, nil
		}
		return []pr.Observation{observation(101)}, 0, nil
	})
	service := pr.NewSyncService(f.pool, f.producer, source)
	if err := service.Request(ctx, f.userID+1, f.repoID); !errors.Is(err, pr.ErrUnavailable) {
		t.Fatal("foreign refresh allowed", err)
	}
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := service.Request(ctx, f.userID, f.repoID); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f.count(t, "SELECT count(*) FROM river_job WHERE kind='pr_discovery'") != 1 {
		t.Fatal("refresh clicks not coalesced")
	}
	if err := service.RunPage(ctx, pr.SyncArgs{RepoID: f.repoID, Revision: 1, Page: 1}); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "SELECT count(*) FROM pr_syncs WHERE state='syncing' AND next_page=2 AND fetched=100") != 1 {
		t.Fatal("partial page claimed complete")
	}
	if err := service.RunPage(ctx, pr.SyncArgs{RepoID: f.repoID, Revision: 1, Page: 1}); err != nil {
		t.Fatal(err)
	} // crash replay
	if err := service.RunPage(ctx, pr.SyncArgs{RepoID: f.repoID, Revision: 1, Page: 2}); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "SELECT count(*) FROM pull_requests") != 101 || f.count(t, "SELECT count(*) FROM build_intents") != 0 || f.count(t, "SELECT count(*) FROM environments") != 0 {
		t.Fatal("discovery built or lost records")
	}
	repo := pr.NewRepository(f.pool)
	page, err := repo.Page(ctx, f.userID, f.repoID, pr.Filter{Page: 2, State: "open"})
	if err != nil || len(page.Rows) != 10 || page.Total != 101 {
		t.Fatalf("pagination: %+v %v", page, err)
	}
	page, err = repo.Page(ctx, f.userID, f.repoID, pr.Filter{Page: 1, State: "all", Search: "Change 101"})
	if err != nil || page.Total != 1 {
		t.Fatal("search", err)
	}
	if _, err := repo.Page(ctx, f.userID+1, f.repoID, pr.Filter{Page: 1, State: "all"}); !errors.Is(err, pr.ErrUnavailable) {
		t.Fatal("foreign list exposed")
	}
	if _, err := repo.Detail(ctx, f.userID+1, f.repoID, 1); !errors.Is(err, pr.ErrUnavailable) {
		t.Fatal("foreign detail exposed")
	}
	if _, err := repo.Activity(ctx, f.userID+1, f.repoID, 1, false); !errors.Is(err, pr.ErrUnavailable) {
		t.Fatal("foreign activity exposed")
	}
}
func TestDiscoverySnapshotFencesWebhookAndInstallation(t *testing.T) {
	for _, change := range []string{"webhook", "installation"} {
		t.Run(change, func(t *testing.T) {
			f := setup(t)
			ctx := t.Context()
			source := sourceFunc(func(context.Context, int64, int64, int32) ([]pr.Observation, int32, error) {
				if change == "webhook" {
					if send(f.hook, "newer", "pull_request", payload("opened", "sha-new", "open", "2026-10-03T10:02:00Z")) != 204 {
						t.Fatal("webhook")
					}
				} else {
					if _, err := f.pool.Exec(ctx, "UPDATE installations SET revision=revision+1"); err != nil {
						t.Fatal(err)
					}
				}
				return []pr.Observation{observation(7)}, 0, nil
			})
			service := pr.NewSyncService(f.pool, f.producer, source)
			if err := service.Request(ctx, f.userID, f.repoID); err != nil {
				t.Fatal(err)
			}
			if err := service.RunPage(ctx, pr.SyncArgs{RepoID: f.repoID, Revision: 1, Page: 1}); !errors.Is(err, pr.ErrSnapshotChanged) {
				t.Fatalf("snapshot was not fenced: %v", err)
			}
			if f.count(t, "SELECT count(*) FROM pull_requests WHERE head_sha='sha-7'") != 0 {
				t.Fatal("stale snapshot saved")
			}
		})
	}
}
func TestDiscoveryRealRiverContinuation(t *testing.T) {
	f := setup(t)
	source := sourceFunc(func(_ context.Context, _, _ int64, page int32) ([]pr.Observation, int32, error) {
		next := int32(0)
		if page == 1 {
			next = 2
		}
		return []pr.Observation{observation(page)}, next, nil
	})
	service := pr.NewSyncService(f.pool, f.producer, source)
	workers := river.NewWorkers()
	river.AddWorker(workers, &pr.SyncWorker{Service: service})
	client, err := river.NewClient(riverpgxv5.New(f.pool), &river.Config{Workers: workers, Queues: map[string]river.QueueConfig{pr.SyncQueue: {MaxWorkers: 2}}, Logger: f.log})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stop, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		if err := client.Stop(stop); err != nil {
			t.Error(err)
		}
	}()
	if err := service.Request(t.Context(), f.userID, f.repoID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if f.count(t, "SELECT count(*) FROM pr_syncs WHERE state='complete' AND fetched=2") == 1 {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("durable River continuation did not complete")
}

func TestSignedFixtureAndAmbiguousTimestamp(t *testing.T) {
	f := setup(t)
	body, err := os.ReadFile("../webhooks/testdata/pull_request.json")
	if err != nil {
		t.Fatal(err)
	}
	if send(f.hook, "fixture", "pull_request", string(body)) != 204 {
		t.Fatal("fixture rejected")
	}
	policy, _ := installations.NewBuildPolicyService(installations.NewRepository(f.pool))
	if _, err := policy.Set(t.Context(), f.userID, f.repoID, installations.AutomaticBuilds); err != nil {
		t.Fatal(err)
	}
	if send(f.hook, "tie", "pull_request", payload("synchronize", "sha-unknown", "open", "2026-10-03T10:00:00Z")) != 204 {
		t.Fatal("tie rejected")
	}
	if f.count(t, "SELECT count(*) FROM pull_requests WHERE needs_refresh AND head_sha='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'") != 1 {
		t.Fatal("ambiguous source guessed")
	}
	if f.count(t, "SELECT count(*) FROM build_intents") != 0 {
		t.Fatal("ambiguous source authorized")
	}
	// A later closed event must invalidate a running generation exactly as Prompt 3.
	if send(f.hook, "newhead", "pull_request", payload("synchronize", "sha-b", "open", "2026-10-03T10:01:00Z")) != 204 {
		t.Fatal("new event")
	}
	if f.count(t, "SELECT count(*) FROM build_intents WHERE state='pending'") != 1 {
		t.Fatal("automatic intent missing")
	}
	if send(f.hook, "close", "pull_request", payload("closed", "sha-b", "closed", "2026-10-03T10:02:00Z")) != 204 {
		t.Fatal("closed")
	}
	if f.count(t, "SELECT count(*) FROM build_intents WHERE state='superseded'") != 1 {
		t.Fatal("close did not supersede")
	}
}

func TestDiscoveryFailurePreservesPartialDataAndRetryCompletes(t *testing.T) {
	f := setup(t)
	if send(f.hook, "existing", "pull_request", payload("opened", "sha-existing", "open", "2026-10-03T10:00:00Z")) != 204 {
		t.Fatal("seed")
	}
	fail := true
	source := sourceFunc(func(_ context.Context, _, _ int64, page int32) ([]pr.Observation, int32, error) {
		if page == 1 {
			return []pr.Observation{observation(1)}, 2, nil
		}
		if fail {
			return nil, 0, errors.New("private provider response")
		}
		return []pr.Observation{observation(2)}, 0, nil
	})
	service := pr.NewSyncService(f.pool, f.producer, source)
	if err := service.Request(t.Context(), 0, f.repoID); !errors.Is(err, pr.ErrUnavailable) {
		t.Fatal("zero owner accepted")
	}
	if err := service.Request(t.Context(), f.userID, f.repoID); err != nil {
		t.Fatal(err)
	}
	if err := service.RunPage(t.Context(), pr.SyncArgs{RepoID: f.repoID, Revision: 1, Page: 1}); err != nil {
		t.Fatal(err)
	}
	worker := &pr.SyncWorker{Service: service}
	job := &river.Job[pr.SyncArgs]{JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 8}, Args: pr.SyncArgs{RepoID: f.repoID, Revision: 1, Page: 2}}
	if err := worker.Work(t.Context(), job); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("failure lost/unsafe", err)
	}
	if f.count(t, "SELECT count(*) FROM pr_syncs WHERE state='failed' AND fetched=1 AND next_page=2") != 1 {
		t.Fatal("failure not durable")
	}
	if f.count(t, "SELECT count(*) FROM pull_requests WHERE number=7 AND state='open'") != 1 {
		t.Fatal("missing page inferred closure")
	}
	fail = false
	if err := worker.Work(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "SELECT count(*) FROM pr_syncs WHERE state='complete' AND fetched=2 AND last_success_at IS NOT NULL") != 1 {
		t.Fatal("retry did not complete")
	}
}
func TestOldDiscoveryFailureCannotDowngradeCompletedPage(t *testing.T) {
	f := setup(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	source := sourceFunc(func(_ context.Context, _, _ int64, _ int32) ([]pr.Observation, int32, error) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		if call == 1 {
			close(entered)
			<-release
			return nil, 0, errors.New("late provider failure")
		}
		return []pr.Observation{observation(1)}, 0, nil
	})
	service := pr.NewSyncService(f.pool, f.producer, source)
	if err := service.Request(t.Context(), f.userID, f.repoID); err != nil {
		t.Fatal(err)
	}
	worker := &pr.SyncWorker{Service: service}
	args := pr.SyncArgs{RepoID: f.repoID, Revision: 1, Page: 1}
	done := make(chan error, 1)
	go func() {
		done <- worker.Work(t.Context(), &river.Job[pr.SyncArgs]{JobRow: &rivertype.JobRow{Attempt: 1}, Args: args})
	}()
	<-entered
	err := service.RunPage(t.Context(), args)
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("late failure absent")
	}
	if f.count(t, "SELECT count(*) FROM pr_syncs WHERE state='complete'") != 1 {
		t.Fatal("late failed job downgraded a complete page")
	}
}

func TestLegacyPRNeedsSourceBaselineBeforeReopen(t *testing.T) {
	f := setup(t)
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO pull_requests(repo_id,number,head_sha,state,generation)VALUES($1,7,'legacy-sha','closed',4)`, f.repoID); err != nil {
		t.Fatal(err)
	}
	if send(f.hook, "unknown-baseline", "pull_request", payload("reopened", "incoming-sha", "open", "2026-10-03T10:00:00Z")) != 204 {
		t.Fatal("delivery rejected")
	}
	if f.count(t, "SELECT count(*) FROM pull_requests WHERE state='closed' AND needs_refresh AND head_sha='legacy-sha'") != 1 {
		t.Fatal("unknown source baseline reopened closed PR")
	}
	source := sourceFunc(func(context.Context, int64, int64, int32) ([]pr.Observation, int32, error) {
		return []pr.Observation{observation(7)}, 0, nil
	})
	service := pr.NewSyncService(f.pool, f.producer, source)
	if err := service.Request(t.Context(), f.userID, f.repoID); err != nil {
		t.Fatal(err)
	}
	if err := service.RunPage(t.Context(), pr.SyncArgs{RepoID: f.repoID, Revision: 1, Page: 1}); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "SELECT count(*) FROM pull_requests WHERE state='open' AND NOT needs_refresh AND source_updated_at IS NOT NULL") != 1 {
		t.Fatal("authoritative refresh did not establish baseline")
	}
	if f.count(t, "SELECT count(*) FROM build_intents") != 0 {
		t.Fatal("baseline refresh authorized a build")
	}
}
