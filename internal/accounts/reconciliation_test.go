package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/installations"
	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/ajaykathwatee/previa/internal/platform/scope"
	"github.com/ajaykathwatee/previa/internal/webhooks"
	"github.com/google/go-github/v91/github"
)

type lifecycleGitHub struct {
	server *httptest.Server
	mode   string
	hook   func()
	once   sync.Once
}

func newLifecycleGitHub(t *testing.T, mode string) *lifecycleGitHub {
	f := &lifecycleGitHub{mode: mode}
	f.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var value any
		switch {
		case r.URL.Path == "/app":
			if mode == "unauthorized" {
				w.WriteHeader(401)
				return
			}
			id := int64(77)
			if mode == "wrong_app" {
				id = 78
			}
			value = map[string]any{"id": id}
		case strings.HasPrefix(r.URL.Path, "/app/installations/"):
			f.once.Do(func() {
				if f.hook != nil {
					f.hook()
				}
			})
			id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/app/installations/"), 10, 64)
			if id == 201 && (mode == "deleted" || mode == "race" || mode == "relink" || mode == "late_error") {
				w.WriteHeader(404)
				return
			}
			if mode == "late_error" {
				w.WriteHeader(502)
				return
			}
			if mode == "outage" {
				w.WriteHeader(502)
				return
			}
			if mode == "rate" {
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.WriteHeader(403)
				return
			}
			if mode == "timeout" {
				<-r.Context().Done()
				return
			}
			m := map[string]any{"id": id, "app_id": 77}
			if mode == "suspended" {
				m["suspended_at"] = time.Now().UTC().Format(time.RFC3339)
			}
			value = m
		case strings.HasSuffix(r.URL.Path, "/installation/repositories"):
			if mode == "list_401" {
				w.WriteHeader(401)
				return
			}
			if mode == "partial" {
				if r.URL.Query().Get("page") == "2" {
					w.WriteHeader(502)
					return
				}
				w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=2>; rel="next"`, f.server.URL, r.URL.Path))
				value = map[string]any{"total_count": 2, "repositories": []any{map[string]any{"id": 301}}}
				break
			}
			if mode == "removed" || mode == "max_changes" {
				value = map[string]any{"total_count": 0, "repositories": []any{}}
				break
			}
			if mode == "incomplete" {
				value = map[string]any{"total_count": 2, "repositories": []any{map[string]any{"id": 301}}}
				break
			}
			value = map[string]any{"total_count": 1, "repositories": []any{map[string]any{"id": 301}}}
		default:
			w.WriteHeader(404)
			return
		}
		if err := json.NewEncoder(w).Encode(value); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}
func (f *lifecycleGitHub) client(prefix string) *github.Client {
	base := f.server.URL + prefix
	client, err := github.NewClient(github.WithHTTPClient(f.server.Client()), github.WithURLs(&base, &base))
	if err != nil {
		panic(err)
	}
	return client
}
func (f *lifecycleGitHub) App(ctx context.Context) (*github.App, error) {
	v, _, err := f.client("/").Apps.Get(ctx, "")
	return v, err
}
func (f *lifecycleGitHub) Installation(ctx context.Context, id int64) (*github.Installation, error) {
	v, _, err := f.client("/").Apps.GetInstallation(ctx, id)
	return v, err
}
func (f *lifecycleGitHub) InstallationClient(id int64) (*github.Client, error) {
	return f.client(fmt.Sprintf("/i/%d/", id)), nil
}

func lifecycleReconciler(t *testing.T, f *fixture, mode string) (*installations.Reconciler, *webhooks.DeliveryService, *lifecycleGitHub) {
	t.Helper()
	producer, err := queue.NewProducer(f.pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	delivery := webhooks.NewDeliveryService(f.pool, environments.NewRequestBroker(f.pool, producer), nil)
	api := newLifecycleGitHub(t, mode)
	return installations.NewReconciler(installations.NewRepository(f.pool), api, delivery, slog.New(slog.NewTextHandler(io.Discard, nil)), 77, 5), delivery, api
}
func TestMissedLifecycleReconciliationUsesAtomicRevocation(t *testing.T) {
	for _, mode := range []string{"deleted", "suspended", "removed"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			ctx := t.Context()
			f.exec(t, `INSERT INTO installations(github_installation_id,owner_user_id) VALUES(202,$1)`, f.owner)
			pr := f.id(t, `INSERT INTO pull_requests(repo_id,number,head_sha) VALUES($1,1,'sha') RETURNING id`, f.repo)
			r, delivery, _ := lifecycleReconciler(t, f, mode)
			if err := r.Run(ctx, 201); err != nil {
				t.Fatal(err)
			}
			if f.id(t, `SELECT teardown_requested_generation FROM pull_requests WHERE id=$1`, pr) == 0 {
				t.Fatal("revocation did not durably request teardown")
			}
			if f.text(t, `SELECT cleanup_pending::text FROM installations WHERE id=$1`, f.installation) != "true" {
				t.Fatal("cleanup pending was not recorded")
			}
			generation := f.id(t, `SELECT generation FROM pull_requests WHERE id=$1`, pr)
			stamp := time.Now().Add(-time.Hour)
			i := &github.Installation{ID: new(int64(201)), UpdatedAt: &github.Timestamp{Time: stamp}}
			var event any = &github.InstallationEvent{Action: new("deleted"), Installation: i}
			kind := "installation"
			if mode == "suspended" {
				event = &github.InstallationEvent{Action: new("suspend"), Installation: i}
			}
			if mode == "removed" {
				kind = "installation_repositories"
				event = &github.InstallationRepositoriesEvent{Action: new("removed"), Installation: i, RepositoriesRemoved: []*github.Repository{{ID: new(int64(301)), FullName: new("acme/api")}}}
			}
			if err := delivery.Process(ctx, "after-reconciliation", kind, "hash", event); err != nil {
				t.Fatal(err)
			}
			if f.id(t, `SELECT generation FROM pull_requests WHERE id=$1`, pr) != generation {
				t.Fatal("redelivered revocation advanced teardown twice")
			}
			if mode == "suspended" {
				i.UpdatedAt = &github.Timestamp{Time: time.Now().Add(time.Second)}
				if err := delivery.Process(ctx, "unsuspend", kind, "hash2", &github.InstallationEvent{Action: new("unsuspend"), Installation: i}); err != nil {
					t.Fatal(err)
				}
				if f.id(t, `SELECT count(*) FROM build_intents WHERE pull_request_id=$1 AND state IN ('pending','queued','running')`, pr) != 0 {
					t.Fatal("restoring access rebuilt the preview")
				}
			}
		})
	}
}
func TestReconciliationFailuresNeverRevoke(t *testing.T) {
	for _, mode := range []string{"outage", "rate", "partial", "incomplete", "unauthorized", "wrong_app", "list_401", "late_error", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			if mode == "late_error" {
				f.exec(t, `INSERT INTO installations(github_installation_id) VALUES(202)`)
			}
			r, _, _ := lifecycleReconciler(t, f, mode)
			ctx := t.Context()
			if mode == "timeout" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			target := int64(201)
			if mode == "late_error" {
				target = 0
			}
			if err := r.Run(ctx, target); err == nil {
				t.Fatal("unverified snapshot succeeded")
			}
			if f.text(t, `SELECT status FROM installations WHERE id=$1`, f.installation) != "active" || f.text(t, `SELECT accessible::text FROM repos WHERE id=$1`, f.repo) != "true" {
				t.Fatal("failed observation revoked access")
			}
		})
	}
}
func TestReconciliationCircuitAndWebhookRace(t *testing.T) {
	t.Run("bounded changes", func(t *testing.T) {
		f := newFixture(t)
		f.exec(t, `INSERT INTO installations(github_installation_id) VALUES(202)`)
		f.exec(t, `INSERT INTO repos(installation_id,github_repo_id,full_name) SELECT $1,n,'acme/repo-' || n FROM generate_series(302,306) n`, f.installation)
		r, _, _ := lifecycleReconciler(t, f, "max_changes")
		if err := r.Run(t.Context(), 201); !errors.Is(err, installations.ErrReconciliationCircuit) {
			t.Fatal(err)
		}
		if f.id(t, `SELECT count(*) FROM repos WHERE installation_id=$1 AND accessible`, f.installation) != 6 {
			t.Fatal("bounded circuit revoked access")
		}
	})
	t.Run("all installations", func(t *testing.T) {
		f := newFixture(t)
		r, _, _ := lifecycleReconciler(t, f, "deleted")
		if err := r.Run(t.Context(), 0); !errors.Is(err, installations.ErrReconciliationCircuit) {
			t.Fatal(err)
		}
		if f.text(t, `SELECT status FROM installations WHERE id=$1`, f.installation) != "active" {
			t.Fatal("circuit revoked access")
		}
	})
	t.Run("webhook wins", func(t *testing.T) {
		f := newFixture(t)
		f.exec(t, `INSERT INTO installations(github_installation_id) VALUES(202)`)
		r, d, api := lifecycleReconciler(t, f, "race")
		api.hook = func() {
			stamp := time.Now()
			err := d.Process(t.Context(), "racing-event", "installation", "race", &github.InstallationEvent{Action: new("unsuspend"), Installation: &github.Installation{ID: new(int64(201)), UpdatedAt: &github.Timestamp{Time: stamp}}})
			if err != nil {
				t.Error(err)
			}
		}
		if err := r.Run(t.Context(), 201); err != nil {
			t.Fatal(err)
		}
		if f.text(t, `SELECT status FROM installations WHERE id=$1`, f.installation) != "active" {
			t.Fatal("stale reconciliation overwrote signed webhook")
		}
	})
}
func TestVerifiedSameWorkspaceReinstallAfterMissedDeletion(t *testing.T) {
	f := newFixture(t)
	r, _, _ := lifecycleReconciler(t, f, "relink")
	repo := installations.NewRepository(f.pool)
	ctx := scope.With(t.Context(), f.workspace, "owner")
	if err := r.ResolveLinkConflicts(ctx, f.owner, 203, []installations.RepositoryInfo{{ID: 301, FullName: "acme/api"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Link(ctx, 203, f.owner, 0, "acme", []installations.RepositoryInfo{{ID: 301, FullName: "acme/api"}}); err != nil {
		t.Fatal(err)
	}
	if f.id(t, `SELECT github_installation_id FROM installations WHERE id=(SELECT installation_id FROM repos WHERE id=$1)`, f.repo) != 203 {
		t.Fatal("history not relinked")
	}
	other := f.id(t, `INSERT INTO users(github_id,username) VALUES(888,'outsider') RETURNING id`)
	if err := repo.Link(scope.With(t.Context(), 0, ""), 204, other, 0, "acme", []installations.RepositoryInfo{{ID: 301, FullName: "acme/api"}}); !errors.Is(err, installations.ErrForbidden) {
		t.Fatalf("another workspace obtained history: %v", err)
	}
}

func TestTokenSignalDurablyRequestsReconciliationWithoutRevocation(t *testing.T) {
	f := newFixture(t)
	producer, err := queue.NewProducer(f.pool, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	signal := installations.SignalReconciliation(f.pool, producer)
	for _, id := range []int64{201, 201, 9999} {
		if err := signal(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	if f.id(t, `SELECT count(*) FROM river_job WHERE kind='installation_reconcile' AND args->>'installation_id'='201'`) != 1 {
		t.Fatal("missing or duplicate durable wakeup")
	}
	if f.id(t, `SELECT count(*) FROM installations WHERE github_installation_id=201 AND reconcile_requested_at > reconciled_at AND status='active'`) != 1 || f.text(t, `SELECT accessible::text FROM repos WHERE id=$1`, f.repo) != "true" {
		t.Fatal("token suspicion revoked access")
	}
}
func TestReconciliationMigrationRoundTrip(t *testing.T) {
	f := newFixture(t)
	b, err := os.ReadFile("../../db/migrations/0044_installation_reconciliation.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(b), "-- +goose Down")
	if !ok {
		t.Fatal("missing down migration")
	}
	if _, err = f.pool.Exec(t.Context(), down); err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(t.Context(), up); err != nil {
		t.Fatal(err)
	}
	r, _, _ := lifecycleReconciler(t, f, "healthy")
	if err := r.Run(t.Context(), 201); err != nil {
		t.Fatal(err)
	}
}
