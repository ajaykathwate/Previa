package environments_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	env "github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/ajaykathwatee/previa/internal/webhooks"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"
)

type fixture struct {
	pool   *pgxpool.Pool
	repo   *env.Repository
	broker *env.Broker
	events *webhooks.Service
	logger *slog.Logger
	repoID int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	connection := os.Getenv("TEST_DATABASE_URL")
	if connection == "" {
		t.Skip("set TEST_DATABASE_URL for generation/ordering integration tests")
	}
	admin, err := pgxpool.New(t.Context(), connection)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("ordering_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+quoted); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		defer admin.Close()
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(connection)
	if err != nil {
		t.Fatal("invalid test connection")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, name := range []string{"0001_init.sql", "0003_pr_generation.sql", "0004_durable_operations.sql", "0005_installation_linking.sql", "0006_build_intents.sql", "0007_intent_dispatch.sql", "0008_pr_workspace.sql", "0009_environment_identity.sql", "0010_pr_mergeability.sql", "0013_build_log_streams.sql", "0014_routes_and_teardown.sql", "0015_pr_reports.sql", "0016_housekeeping.sql", "0017_worker_capabilities.sql", "0018_builds_paused.sql", "0022_account_fairness.sql", "0023_build_log_deletion.sql", "0024_preview_activity.sql", "0025_app_output.sql", "0026_preview_sleep.sql", "0027_private_previews.sql"} {
		data, err := os.ReadFile("../../db/migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(data), "-- +goose Down")
		if _, err := pool.Exec(t.Context(), up); err != nil {
			t.Fatal(err)
		}
	}
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrator.Migrate(t.Context(), rivermigrate.DirectionUp, nil); err != nil {
		t.Fatal(err)
	}
	var userID, installID, repoID int64
	if err := pool.QueryRow(t.Context(), "INSERT INTO users(github_id,username) VALUES(1,'test') RETURNING id").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), "INSERT INTO installations(github_installation_id,owner_user_id) VALUES(1,$1) RETURNING id", userID).Scan(&installID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), "INSERT INTO repos(installation_id,full_name,github_repo_id) VALUES($1,'test/repo',4242) RETURNING id", installID).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	producer, err := queue.NewProducer(pool, logger)
	if err != nil {
		t.Fatal(err)
	}
	operationBroker, err := env.NewOperationBroker(producer)
	if err != nil {
		t.Fatal(err)
	}
	repo := env.NewRepository(pool, operationBroker)
	runtime := newRuntime()
	s, err := env.NewService(repo, runtime, logger, queue.DefaultBuildTimeout)
	if err != nil {
		t.Fatal(err)
	}
	op, err := env.NewOperationWorker(repo, &fakePublisher{runtime: runtime}, logger, time.Minute, map[env.OperationKind]time.Duration{env.ActivateRoute: time.Second, env.RemoveRoute: time.Second, env.ReportStatus: time.Second, env.CleanupArtifact: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	broker, err := env.NewExecutionBroker(pool, producer, s, op, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), "UPDATE repos SET build_mode='automatic' WHERE id=$1", repoID); err != nil {
		t.Fatal(err)
	}
	return &fixture{pool: pool, repo: repo, broker: broker, events: webhooks.NewService(pool, broker), logger: logger, repoID: repoID}
}

func (f *fixture) event(t *testing.T, action, sha string) env.Request {
	t.Helper()
	r, err := f.events.ProcessPR(t.Context(), webhooks.PREvent{MergeableState: "clean", RepoID: f.repoID, Number: 1, HeadSHA: sha, Action: action})
	if err != nil {
		t.Fatal(err)
	}
	// Ordering regressions exercise already-eligible automatic work. Debounce
	// timing is verified independently by scheduling tests with an injected clock.
	if _, err := f.pool.Exec(t.Context(), "UPDATE build_intents SET available_at=now()-interval '1 second' WHERE pull_request_id=$1", r.PullRequestID); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.broker.EnqueueTx(t.Context(), tx, r); err != nil {
		_ = tx.Rollback(t.Context())
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return r
}
func (f *fixture) provision(t *testing.T, request env.Request) {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(), "INSERT INTO environments(pull_request_id,subdomain,expires_at) VALUES($1,'preview-test',now()+interval '1 day')", request.PullRequestID); err != nil {
		t.Fatal(err)
	}
}

type fakeRuntime struct {
	mu                               sync.Mutex
	resources                        map[int64]env.Artifact
	revisions                        map[int64]int64
	started                          chan int64
	gate                             map[int64]chan struct{}
	fail                             map[int64]bool
	cleanupFailures                  int
	removedContainers, removedImages []string
	attempts                         []env.Attempt
}

func newRuntime() *fakeRuntime {
	return &fakeRuntime{resources: map[int64]env.Artifact{}, revisions: map[int64]int64{}, started: make(chan int64, 20), gate: map[int64]chan struct{}{}, fail: map[int64]bool{}}
}
func (r *fakeRuntime) Build(ctx context.Context, request env.Request, attempt env.Attempt) (env.Artifact, error) {
	a := env.Artifact{ContainerID: fmt.Sprintf("container-%d", request.Generation), ImageTag: fmt.Sprintf("image-%d", request.Generation)}
	r.mu.Lock()
	r.attempts = append(r.attempts, attempt)
	gate := r.gate[request.Generation]
	fail := r.fail[request.Generation]
	r.mu.Unlock()
	r.started <- request.Generation
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return a, ctx.Err()
		}
	}
	r.mu.Lock()
	r.resources[request.Generation] = a
	r.revisions[request.Generation] = request.Revision
	r.mu.Unlock()
	if fail {
		return a, errors.New("simulated build failure")
	}
	return a, nil
}
func (r *fakeRuntime) Resources(_ context.Context, prID int64) ([]env.Resource, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cleanupFailures > 0 {
		r.cleanupFailures--
		return nil, errors.New("runtime unavailable")
	}
	var resources []env.Resource
	for gen, a := range r.resources {
		resources = append(resources, env.Resource{PullRequestID: prID, Generation: gen, Revision: r.revisions[gen], Artifact: a})
	}
	return resources, nil
}
func (r *fakeRuntime) RemoveContainer(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for gen, a := range r.resources {
		if a.ContainerID == id {
			r.removedContainers = append(r.removedContainers, id)
			a.ContainerID = ""
			r.resources[gen] = a
		}
	}
	return nil
}
func (r *fakeRuntime) RemoveImage(_ context.Context, tag string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for gen, a := range r.resources {
		if a.ImageTag == tag {
			if a.ContainerID != "" {
				return errors.New("container must be removed before image")
			}
			r.removedImages = append(r.removedImages, tag)
			delete(r.resources, gen)
		}
	}
	return nil
}

type fakePublisher struct {
	mu          sync.Mutex
	generations []int64
	statuses    []string
	kinds       []env.OperationKind
	runtime     *fakeRuntime
	entered     chan struct{}
	release     chan struct{}
	externalErr error
}

func (p *fakePublisher) Execute(ctx context.Context, operation env.Operation) error {
	if p.entered != nil {
		select {
		case p.entered <- struct{}{}:
		default:
		}
	}
	if p.release != nil {
		select {
		case <-p.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if p.externalErr != nil {
		return p.externalErr
	}
	p.mu.Lock()
	p.generations = append(p.generations, operation.Generation)
	p.statuses = append(p.statuses, operation.Payload.Result.Status)
	p.kinds = append(p.kinds, operation.Kind)
	p.mu.Unlock()
	if operation.Kind == env.CleanupArtifact && p.runtime != nil {
		if id := operation.Payload.Resource.Artifact.ContainerID; id != "" {
			if err := p.runtime.RemoveContainer(ctx, id); err != nil {
				return err
			}
		}
		if image := operation.Payload.Resource.Artifact.ImageTag; image != "" {
			if err := p.runtime.RemoveImage(ctx, image); err != nil {
				return err
			}
		}
	}
	return nil
}
func service(t *testing.T, f *fixture, runtime *fakeRuntime, publisher *fakePublisher) *env.Service {
	t.Helper()
	publisher.runtime = runtime
	s, err := env.NewService(f.repo, runtime, f.logger, queue.DefaultBuildTimeout)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func drainOperations(t *testing.T, f *fixture, executor *fakePublisher) {
	t.Helper()
	timeouts := map[env.OperationKind]time.Duration{
		env.ActivateRoute: time.Second, env.RemoveRoute: time.Second,
		env.ReportStatus: time.Second, env.CleanupArtifact: time.Second,
	}
	worker, err := env.NewOperationWorker(f.repo, executor, f.logger, 2*time.Second, timeouts)
	if err != nil {
		t.Fatal(err)
	}
	for processed := 0; processed < 100; processed++ {
		var id int64
		if err := f.pool.QueryRow(t.Context(), "SELECT id FROM environment_operations WHERE state IN ('pending','retryable') ORDER BY id LIMIT 1").Scan(&id); errors.Is(err, pgx.ErrNoRows) {
			return
		} else if err != nil {
			t.Fatal(err)
		}
		if err := worker.Process(t.Context(), id, fmt.Sprintf("test-operation-%d", id), 1, 5); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("operation queue did not drain")
}
func waitStarted(t *testing.T, r *fakeRuntime) {
	t.Helper()
	select {
	case <-r.started:
	case <-time.After(5 * time.Second):
		t.Fatal("build did not start")
	}
}
func waitResult(t *testing.T, ch <-chan error) {
	t.Helper()
	select {
	case err := <-ch:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("build did not finish")
	}
}

func TestOlderBuildFinishesAfterNewerResult(t *testing.T) {
	for _, failA := range []bool{false, true} {
		t.Run(fmt.Sprint("old_failure=", failA), func(t *testing.T) {
			f := newFixture(t)
			a := f.event(t, "opened", "sha-A")
			f.provision(t, a)
			r := newRuntime()
			r.gate[a.Generation] = make(chan struct{})
			r.fail[a.Generation] = failA
			p := &fakePublisher{}
			s := service(t, f, r, p)
			done := make(chan error, 1)
			go func() { done <- s.Build(t.Context(), a) }()
			waitStarted(t, r)
			b := f.event(t, "synchronize", "sha-B")
			if b.Generation != a.Generation+1 {
				t.Fatal("generation did not increment by exactly one")
			}
			if err := s.Build(t.Context(), b); err != nil {
				t.Fatal(err)
			}
			close(r.gate[a.Generation])
			waitResult(t, done)
			drainOperations(t, f, p)
			var sha, status, container string
			var generation int64
			if err := f.pool.QueryRow(t.Context(), "SELECT head_sha,status,container_id,applied_generation FROM environments WHERE pull_request_id=$1", a.PullRequestID).Scan(&sha, &status, &container, &generation); err != nil {
				t.Fatal(err)
			}
			if sha != "sha-B" || status != "running" || container != "container-2" || generation != b.Generation {
				t.Fatalf("stale overwrite: %s %s %s %d", sha, status, container, generation)
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			if len(p.generations) < 1 || p.generations[0] != b.Generation || p.kinds[0] != env.ActivateRoute {
				t.Fatalf("stale publication: %v", p.generations)
			}
			for i, kind := range p.kinds {
				if kind == env.ActivateRoute && p.generations[i] != b.Generation {
					t.Fatalf("stale route activation: %v %v", p.kinds, p.generations)
				}
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			if len(r.resources) != 1 || len(r.removedContainers) != 1 || len(r.removedImages) != 1 {
				t.Fatalf("superseded resources leaked: %+v", r.resources)
			}
		})
	}
}

func TestCloseWinsOverInflightBuild(t *testing.T) {
	f := newFixture(t)
	a := f.event(t, "opened", "sha-A")
	f.provision(t, a)
	r := newRuntime()
	r.gate[a.Generation] = make(chan struct{})
	p := &fakePublisher{}
	s := service(t, f, r, p)
	done := make(chan error, 1)
	go func() { done <- s.Build(t.Context(), a) }()
	waitStarted(t, r)
	closed := f.event(t, "closed", "sha-A")
	if err := s.Teardown(t.Context(), closed); err != nil {
		t.Fatal(err)
	}
	close(r.gate[a.Generation])
	waitResult(t, done)
	drainOperations(t, f, p)
	snapshot, err := f.repo.Snapshot(t.Context(), a.PullRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != "torn_down" || snapshot.AppliedGeneration != closed.Generation || snapshot.Artifact.ContainerID != "" {
		t.Fatalf("build resurrected closed PR: %+v", snapshot)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.generations) < 1 || p.kinds[0] != env.RemoveRoute || p.generations[0] != closed.Generation {
		t.Fatalf("stale build published: %+v", p.generations)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.resources) != 0 || len(r.removedImages) != 1 || len(r.removedContainers) != 1 {
		t.Fatal("closed PR leaked build resources")
	}
}

func TestQueuedOldBuildSkipsExecutionAndCleanupRetries(t *testing.T) {
	f := newFixture(t)
	a := f.event(t, "opened", "sha-A")
	f.provision(t, a)
	f.event(t, "closed", "sha-A")
	r := newRuntime()
	r.resources[a.Generation] = env.Artifact{ContainerID: "orphan", ImageTag: "orphan-image"}
	r.cleanupFailures = 1
	p := &fakePublisher{}
	s := service(t, f, r, p)
	if err := s.Build(t.Context(), a); err == nil {
		t.Fatal("cleanup failure was swallowed")
	}
	if err := s.Build(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	drainOperations(t, f, p)
	if len(r.started) != 0 || len(r.resources) != 0 {
		t.Fatal("stale queued job ran or left an orphan")
	}
}

func TestAtomicGuardWaitsForConcurrentGenerationBump(t *testing.T) {
	f := newFixture(t)
	a := f.event(t, "opened", "sha-A")
	f.provision(t, a)
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Error(err)
		}
	}()
	if _, err := tx.Exec(t.Context(), "UPDATE pull_requests SET generation=generation+1,head_sha='sha-B' WHERE id=$1", a.PullRequestID); err != nil {
		t.Fatal(err)
	}
	type completion struct {
		applied bool
		err     error
	}
	done := make(chan completion, 1)
	go func() {
		applied, err := f.repo.Apply(t.Context(), a, env.Result{Status: "running", Artifact: env.Artifact{ContainerID: "stale", ImageTag: "stale"}})
		done <- completion{applied, err}
	}()
	// Wait until the completion connection is actually blocked on the PR lock.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		err := f.pool.QueryRow(t.Context(), "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE '-- name: LockIntentPR%')").Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("guard never waited on PR lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.applied {
			t.Fatalf("concurrent update accepted stale snapshot: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("guard did not unblock")
	}
	snapshot, err := f.repo.Snapshot(t.Context(), a.PullRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != "queued" || snapshot.AppliedGeneration != 0 {
		t.Fatal("stale completion touched environment")
	}
}

func TestGenerationCaptureAndRiverHandoff(t *testing.T) {
	for _, action := range []string{"synchronize", "closed", "terminal-failure"} {
		t.Run(action, func(t *testing.T) {
			f := newFixture(t)
			a := f.event(t, "opened", "sha-A")
			f.provision(t, a)
			r := newRuntime()
			r.gate[a.Generation] = make(chan struct{})
			p := &fakePublisher{}
			s := service(t, f, r, p)
			workers := river.NewWorkers()
			env.RegisterWorkers(workers, s, f.broker)
			client, err := river.NewClient(riverpgxv5.New(f.pool), &river.Config{Logger: f.logger, Workers: workers, Queues: map[string]river.QueueConfig{env.BuildQueue: {MaxWorkers: 2}, env.TeardownQueue: {MaxWorkers: 2}}, JobTimeout: queue.PingTimeout, RescueStuckJobsAfter: queue.RescueScanFloor})
			if err != nil {
				t.Fatal(err)
			}
			events, cancel := client.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
			if action == "terminal-failure" {
				r.cleanupFailures = 1
				if _, err := f.pool.Exec(t.Context(), "UPDATE river_job SET max_attempts=1"); err != nil {
					t.Fatal(err)
				}
			}
			defer cancel()
			if err := client.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := client.StopAndCancel(ctx); err != nil {
					t.Error(err)
				}
			}()
			waitStarted(t, r)
			eventAction := action
			if action == "terminal-failure" {
				eventAction = "synchronize"
			}
			b := f.event(t, eventAction, "sha-B")
			var count int
			var args []byte
			if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM river_job WHERE kind IN ('build','teardown') AND state IN ('available','running')").Scan(&count); err != nil || count != 1 {
				t.Fatalf("lost uniqueness: %d %v", count, err)
			}
			if err := f.pool.QueryRow(t.Context(), "SELECT args FROM river_job WHERE kind IN ('build','teardown') ORDER BY id LIMIT 1").Scan(&args); err != nil {
				t.Fatal(err)
			}
			var captured env.Request
			if err := json.Unmarshal(args, &captured); err != nil {
				t.Fatal(err)
			}
			if captured.Generation != a.Generation || captured.HeadSHA != a.HeadSHA {
				t.Fatal("original job snapshot mutated")
			}
			close(r.gate[a.Generation])
			for range 2 {
				select {
				case <-events:
				case <-time.After(10 * time.Second):
					t.Fatal("latest generation lost during handoff")
				}
			}
			snapshot, err := f.repo.Snapshot(t.Context(), a.PullRequestID)
			if err != nil {
				t.Fatal(err)
			}
			want := "publishing"
			if action == "closed" {
				want = "tearing_down"
			}
			if snapshot.AppliedGeneration != 0 || snapshot.Status != want {
				t.Fatalf("wrong final state: %+v", snapshot)
			}
			if err := f.pool.QueryRow(t.Context(), "SELECT args FROM river_job WHERE kind IN ('build','teardown') ORDER BY id DESC LIMIT 1").Scan(&args); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(args, &captured); err != nil {
				t.Fatal(err)
			}
			if captured.Generation != b.Generation {
				t.Fatal("successor did not capture latest generation")
			}
			if action == "terminal-failure" {
				var state string
				if err := f.pool.QueryRow(t.Context(), "SELECT state FROM river_job WHERE kind IN ('build','teardown') ORDER BY id LIMIT 1").Scan(&state); err != nil {
					t.Fatal(err)
				}
				if state != "discarded" {
					t.Fatalf("terminal failure state=%s", state)
				}
			}
		})
	}
}

type failingEnqueuer struct{}

func (failingEnqueuer) ObserveTx(context.Context, pgx.Tx, env.Request) (env.Request, error) {
	return env.Request{}, errors.New("queue unavailable")
}

type failingOperationInserter struct{}

func (failingOperationInserter) InsertTx(context.Context, pgx.Tx, river.JobArgs, *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	return nil, errors.New("queue unavailable")
}

func TestOperationOutboxAndStateRollBackTogether(t *testing.T) {
	f := newFixture(t)
	request := f.event(t, "opened", "sha-A")
	f.provision(t, request)
	broker, err := env.NewOperationBroker(failingOperationInserter{})
	if err != nil {
		t.Fatal(err)
	}
	repo := env.NewRepository(f.pool, broker)
	if applied, err := repo.Apply(t.Context(), request, env.Result{Status: "running", Artifact: env.Artifact{ContainerID: "candidate", ImageTag: "candidate-image"}}); err == nil || applied {
		t.Fatalf("operation queue failure should reject transition: applied=%v err=%v", applied, err)
	}
	snapshot, err := repo.Snapshot(t.Context(), request.PullRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != "queued" || snapshot.PendingGeneration != 0 {
		t.Fatalf("state committed without its River wake-up: %+v", snapshot)
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM environment_operations").Scan(&count); err != nil || count != 0 {
		t.Fatalf("orphan outbox row after rollback: count=%d err=%v", count, err)
	}
}

func TestOperationRecoveryRequeuesLostWakeupAndExpiredLease(t *testing.T) {
	f := newFixture(t)
	request := f.event(t, "opened", "sha-A")
	f.provision(t, request)
	if applied, err := f.repo.Apply(t.Context(), request, env.Result{Status: "running", Artifact: env.Artifact{ContainerID: "candidate", ImageTag: "image"}}); err != nil || !applied {
		t.Fatalf("apply candidate: applied=%v err=%v", applied, err)
	}
	var operationID int64
	if err := f.pool.QueryRow(t.Context(), "SELECT id FROM environment_operations WHERE kind='activate_route'").Scan(&operationID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), "DELETE FROM river_job WHERE kind='environment_operation' AND args->>'operation_id'=$1", fmt.Sprint(operationID)); err != nil {
		t.Fatal(err)
	}
	if count, err := f.repo.RecoverOperations(t.Context(), 10); err != nil || count != 1 {
		t.Fatalf("recover missing wake-up: count=%d err=%v", count, err)
	}
	var riverJobs int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM river_job WHERE kind='environment_operation' AND args->>'operation_id'=$1", fmt.Sprint(operationID)).Scan(&riverJobs); err != nil || riverJobs != 1 {
		t.Fatalf("missing operation wake-up was not restored: count=%d err=%v", riverJobs, err)
	}
	if _, err := f.pool.Exec(t.Context(), "DELETE FROM river_job WHERE kind='environment_operation' AND args->>'operation_id'=$1", fmt.Sprint(operationID)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), "UPDATE environment_operations SET state='running', lease_owner='crashed-attempt', lease_expires_at=now()-interval '1 second' WHERE id=$1", operationID); err != nil {
		t.Fatal(err)
	}
	if count, err := f.repo.RecoverOperations(t.Context(), 10); err != nil || count != 1 {
		t.Fatalf("recover expired lease: count=%d err=%v", count, err)
	}
	var state string
	if err := f.pool.QueryRow(t.Context(), "SELECT state FROM environment_operations WHERE id=$1", operationID).Scan(&state); err != nil || state != "retryable" {
		t.Fatalf("expired lease was not released: state=%s err=%v", state, err)
	}
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM river_job WHERE kind='environment_operation' AND args->>'operation_id'=$1", fmt.Sprint(operationID)).Scan(&riverJobs); err != nil || riverJobs != 1 {
		t.Fatalf("expired operation wake-up was not restored: count=%d err=%v", riverJobs, err)
	}
}

func TestEventEnqueueFailureRollsBackGeneration(t *testing.T) {
	f := newFixture(t)
	a := f.event(t, "opened", "sha-A")
	s := webhooks.NewService(f.pool, failingEnqueuer{})
	if _, err := s.ProcessPR(t.Context(), webhooks.PREvent{MergeableState: "clean", RepoID: f.repoID, Number: 1, HeadSHA: "sha-B", Action: "synchronize"}); err == nil {
		t.Fatal("enqueue failure accepted")
	}
	snapshot, err := f.repo.Snapshot(t.Context(), a.PullRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Generation != a.Generation {
		t.Fatal("generation committed without job")
	}
}

func TestFailedRebuildPreservesServingPreview(t *testing.T) {
	f := newFixture(t)
	a := f.event(t, "opened", "sha-A")
	f.provision(t, a)
	r := newRuntime()
	p := &fakePublisher{}
	s := service(t, f, r, p)
	if err := s.Build(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	drainOperations(t, f, p)
	b := f.event(t, "synchronize", "sha-B")
	r.fail[b.Generation] = true
	// Even stale cleanup must preserve A while it is still the referenced preview.
	if err := s.Build(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if err := s.Build(t.Context(), b); err == nil {
		t.Fatal("current build failure must be retryable")
	}
	drainOperations(t, f, p)
	snapshot, err := f.repo.Snapshot(t.Context(), a.PullRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != "failed" || snapshot.Artifact.ContainerID != "container-1" || snapshot.AppliedGeneration != b.Generation {
		t.Fatalf("old preview lost: %+v", snapshot)
	}
	var sha string
	if err := f.pool.QueryRow(t.Context(), "SELECT head_sha FROM environments WHERE pull_request_id=$1", a.PullRequestID).Scan(&sha); err != nil {
		t.Fatal(err)
	}
	if sha != "sha-A" {
		t.Fatal("failed commit replaced deployed SHA")
	}
	if len(r.resources) != 1 || r.resources[a.Generation].ContainerID != "container-1" {
		t.Fatal("cleanup deleted serving preview or leaked failed build")
	}
}

func TestExternalPublicationDoesNotHoldGenerationLock(t *testing.T) {
	f := newFixture(t)
	a := f.event(t, "opened", "sha-A")
	f.provision(t, a)
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	artifact := env.Artifact{ContainerID: "current", ImageTag: "current"}
	if applied, err := f.repo.Apply(t.Context(), a, env.Result{Status: "running", Artifact: artifact}); err != nil || !applied {
		t.Fatalf("apply result: applied=%v err=%v", applied, err)
	}
	var operationID int64
	if err := f.pool.QueryRow(t.Context(), "SELECT id FROM environment_operations WHERE kind='activate_route'").Scan(&operationID); err != nil {
		t.Fatal(err)
	}
	p := &fakePublisher{entered: entered, release: release}
	worker, err := env.NewOperationWorker(f.repo, p, f.logger, 10*time.Second, map[env.OperationKind]time.Duration{
		env.ActivateRoute: time.Second, env.RemoveRoute: time.Second, env.ReportStatus: time.Second, env.CleanupArtifact: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- worker.Process(t.Context(), operationID, "blocked-publisher", 1, 5) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not start")
	}
	eventDone := make(chan error, 1)
	go func() {
		_, err := f.events.ProcessPR(t.Context(), webhooks.PREvent{MergeableState: "clean", RepoID: f.repoID, Number: 1, HeadSHA: "sha-B", Action: "synchronize"})
		eventDone <- err
	}()
	select {
	case err := <-eventDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PR event blocked by external publication")
	}
	releaseOnce.Do(func() { close(release) })
	waitResult(t, result)
	snapshot, err := f.repo.Snapshot(t.Context(), a.PullRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Generation != 2 || snapshot.AppliedGeneration != 0 {
		t.Fatalf("unexpected serialized result: %+v", snapshot)
	}
}

func TestOperationOutboxRetriesAndFinalizes(t *testing.T) {
	f := newFixture(t)
	request := f.event(t, "opened", "sha-A")
	f.provision(t, request)
	artifact := env.Artifact{ContainerID: "candidate", ImageTag: "candidate-image"}
	if applied, err := f.repo.Apply(t.Context(), request, env.Result{Status: "running", Artifact: artifact}); err != nil || !applied {
		t.Fatalf("apply candidate: applied=%v err=%v", applied, err)
	}
	var id int64
	if err := f.pool.QueryRow(t.Context(), "SELECT id FROM environment_operations WHERE kind='activate_route'").Scan(&id); err != nil {
		t.Fatal(err)
	}
	p := &fakePublisher{externalErr: errors.New("temporary provider outage")}
	worker, err := env.NewOperationWorker(f.repo, p, f.logger, 10*time.Second, map[env.OperationKind]time.Duration{
		env.ActivateRoute: time.Second, env.RemoveRoute: time.Second, env.ReportStatus: time.Second, env.CleanupArtifact: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Process(t.Context(), id, "attempt-one", 1, 5); err == nil {
		t.Fatal("transient external failure was swallowed")
	}
	var state, code string
	if err := f.pool.QueryRow(t.Context(), "SELECT state,last_error_code FROM environment_operations WHERE id=$1", id).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "retryable" || code != "external_operation_failed" {
		t.Fatalf("unsafe retry state/error: %s %s", state, code)
	}
	var delayed bool
	if err := f.pool.QueryRow(t.Context(), "SELECT available_at > now() FROM environment_operations WHERE id=$1", id).Scan(&delayed); err != nil || !delayed {
		t.Fatalf("retry backoff was not persisted: delayed=%v err=%v", delayed, err)
	}
	if _, err := f.pool.Exec(t.Context(), "UPDATE environment_operations SET available_at=now() WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	p.externalErr = nil
	if err := worker.Process(t.Context(), id, "attempt-two", 2, 5); err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.repo.Snapshot(t.Context(), request.PullRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != "running" || snapshot.AppliedGeneration != request.Generation || snapshot.Artifact != artifact {
		t.Fatalf("retry did not finalize candidate: %+v", snapshot)
	}
	if err := f.pool.QueryRow(t.Context(), "SELECT state FROM environment_operations WHERE id=$1", id).Scan(&state); err != nil || state != "succeeded" {
		t.Fatalf("operation not durably complete: state=%s err=%v", state, err)
	}
}

func TestPendingCandidateIsPreservedUntilReplacementRouteActivates(t *testing.T) {
	f := newFixture(t)
	a := f.event(t, "opened", "sha-A")
	f.provision(t, a)
	artifactA := env.Artifact{ContainerID: "candidate-A", ImageTag: "image-A"}
	if applied, err := f.repo.Apply(t.Context(), a, env.Result{Status: "running", Artifact: artifactA}); err != nil || !applied {
		t.Fatalf("apply A: applied=%v err=%v", applied, err)
	}
	b := f.event(t, "synchronize", "sha-B")
	if applied, err := f.repo.Apply(t.Context(), b, env.Result{Status: "failed", ErrorCode: "build_failed"}); err != nil || !applied {
		t.Fatalf("record B failure: applied=%v err=%v", applied, err)
	}
	snapshot, err := f.repo.Snapshot(t.Context(), a.PullRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.PendingArtifact != artifactA || snapshot.PendingGeneration != a.Generation {
		t.Fatalf("failed rebuild discarded earlier pending candidate: %+v", snapshot)
	}
	if err := f.repo.ScheduleResourceCleanup(t.Context(), b, []env.Resource{{PullRequestID: a.PullRequestID, Generation: a.Generation, Artifact: artifactA}}); err != nil {
		t.Fatal(err)
	}
	var earlyCleanupCount int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM environment_operations WHERE kind='cleanup_artifact'").Scan(&earlyCleanupCount); err != nil || earlyCleanupCount != 0 {
		t.Fatalf("candidate was scheduled for cleanup before route replacement: count=%d err=%v", earlyCleanupCount, err)
	}
	artifactB := env.Artifact{ContainerID: "candidate-B", ImageTag: "image-B"}
	if applied, err := f.repo.Apply(t.Context(), b, env.Result{Status: "running", Artifact: artifactB}); err != nil || !applied {
		t.Fatalf("apply B: applied=%v err=%v", applied, err)
	}
	var bOperationID, aOperationID int64
	if err := f.pool.QueryRow(t.Context(), "SELECT id FROM environment_operations WHERE kind='activate_route' AND generation=$1", b.Generation).Scan(&bOperationID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), "SELECT id FROM environment_operations WHERE kind='activate_route' AND generation=$1", a.Generation).Scan(&aOperationID); err != nil {
		t.Fatal(err)
	}
	p := &fakePublisher{}
	timeouts := map[env.OperationKind]time.Duration{
		env.ActivateRoute: time.Second, env.RemoveRoute: time.Second,
		env.ReportStatus: time.Second, env.CleanupArtifact: time.Second,
	}
	worker, err := env.NewOperationWorker(f.repo, p, f.logger, 2*time.Second, timeouts)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Process(t.Context(), bOperationID, "activate-B", 1, 5); err != nil {
		t.Fatal(err)
	}
	if err := worker.Process(t.Context(), aOperationID, "activate-A", 1, 5); err != nil {
		t.Fatal(err)
	}
	snapshot, err = f.repo.Snapshot(t.Context(), a.PullRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != "running" || snapshot.Artifact != artifactB || snapshot.AppliedGeneration != b.Generation {
		t.Fatalf("latest candidate did not become active: %+v", snapshot)
	}
	var cleanupCount int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM environment_operations WHERE kind='cleanup_artifact' AND payload->'resource'->'artifact'->>'container_id'='candidate-A'").Scan(&cleanupCount); err != nil {
		t.Fatal(err)
	}
	if cleanupCount != 1 {
		t.Fatalf("old pending candidate cleanup should follow B activation: count=%d", cleanupCount)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.generations) != 1 || p.generations[0] != b.Generation || p.kinds[0] != env.ActivateRoute {
		t.Fatalf("superseded A reached executor: kinds=%v generations=%v", p.kinds, p.generations)
	}
}

func TestConcurrentEventsIncrementExactlyOnce(t *testing.T) {
	f := newFixture(t)
	first := f.event(t, "opened", "sha-A")
	const updates = 12
	done := make(chan error, updates)
	for i := range updates {
		go func() {
			_, err := f.events.ProcessPR(t.Context(), webhooks.PREvent{MergeableState: "clean", RepoID: f.repoID, Number: 1, HeadSHA: fmt.Sprintf("sha-%d", i), Action: "synchronize"})
			done <- err
		}()
	}
	for range updates {
		waitResult(t, done)
	}
	snapshot, err := f.repo.Snapshot(t.Context(), first.PullRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Generation != updates+1 {
		t.Fatalf("generation increments lost: %d", snapshot.Generation)
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM river_job WHERE kind IN ('build','teardown')").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("PR job uniqueness lost")
	}
}
