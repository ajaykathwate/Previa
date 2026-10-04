package environments_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	env "github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/installations"
	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/ajaykathwatee/previa/internal/webhooks"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *testClock) advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.at = c.at.Add(d) }

type scheduleFixture struct {
	*fixture
	clock   *testClock
	owner   int64
	policy  *installations.BuildPolicyService
	intents *env.IntentService
}

func newScheduleFixture(t *testing.T) *scheduleFixture {
	t.Helper()
	f := newFixture(t)
	c := &testClock{at: time.Now().UTC().Truncate(time.Microsecond)}
	producer, err := queue.NewProducer(f.pool, f.logger)
	if err != nil {
		t.Fatal(err)
	}
	runtime := newRuntime()
	s, err := env.NewService(f.repo, runtime, f.logger, time.Minute, c.now)
	if err != nil {
		t.Fatal(err)
	}
	op, err := env.NewOperationWorker(f.repo, &fakePublisher{runtime: runtime}, f.logger, time.Minute, map[env.OperationKind]time.Duration{env.ActivateRoute: time.Second, env.RemoveRoute: time.Second, env.ReportStatus: time.Second, env.CleanupArtifact: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	f.broker, err = env.NewExecutionBroker(f.pool, producer, s, op, c.now)
	if err != nil {
		t.Fatal(err)
	}
	f.events = webhooks.NewService(f.pool, f.broker)
	sf := &scheduleFixture{fixture: f, clock: c}
	if err := f.pool.QueryRow(t.Context(), "SELECT owner_user_id FROM installations LIMIT 1").Scan(&sf.owner); err != nil {
		t.Fatal(err)
	}
	sf.policy, err = installations.NewBuildPolicyService(installations.NewRepository(f.pool))
	if err != nil {
		t.Fatal(err)
	}
	sf.intents, err = env.NewIntentService(env.NewIntentRepository(f.pool))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sf.policy.Set(t.Context(), sf.owner, f.repoID, installations.ManualBuilds); err != nil {
		t.Fatal(err)
	}
	return sf
}
func (f *scheduleFixture) observe(t *testing.T, action, sha string) env.Request {
	t.Helper()
	r, err := f.events.ProcessPR(t.Context(), webhooks.PREvent{MergeableState: "clean", RepoID: f.repoID, Number: 1, Action: action, HeadSHA: sha})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func wake(r env.Request) env.WakeArgs {
	return env.WakeArgs{Key: fmt.Sprintf("wake:pr:%d:g:%d:r:%d", r.PullRequestID, r.Generation, r.Revision), PullRequestID: r.PullRequestID, Generation: r.Generation, Revision: r.Revision}
}
func (f *scheduleFixture) count(t *testing.T, kind string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM river_job WHERE kind=$1", kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func (f *scheduleFixture) command(r env.Request, key string, expected int64, action env.IntentAction) env.IntentCommand {
	return env.IntentCommand{UserID: f.owner, PullRequestID: r.PullRequestID, IdempotencyKey: key, ExpectedRevision: expected, Action: action}
}
func requestFromIntent(i env.BuildIntent) env.Request {
	r := env.NewRequest(i.PullRequestID, i.Generation, i.HeadSHA, "open")
	r.Revision = i.Revision
	return r
}

func TestSchedulingManualObservationAndClosedTeardown(t *testing.T) {
	f := newScheduleFixture(t)
	a := f.observe(t, "opened", "sha-A")
	b := f.observe(t, "synchronize", "sha-B")
	if b.Generation != a.Generation+1 || f.count(t, "environment_wake") != 0 || f.count(t, "build") != 0 {
		t.Fatal("Manual observation authorized work")
	}
	var environments int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM environments").Scan(&environments); err != nil || environments != 0 {
		t.Fatalf("observation provisioned: %d %v", environments, err)
	}
	if _, err := f.events.ProcessPR(t.Context(), webhooks.PREvent{MergeableState: "clean", RepoID: f.repoID, Number: 1, Action: "edited", HeadSHA: "ignored"}); err == nil {
		t.Fatal("unsupported event accepted")
	}
	c := f.observe(t, "closed", "sha-B")
	if err := f.broker.Dispatch(t.Context(), wake(c)); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "teardown") != 1 {
		t.Fatal("close did not admit teardown immediately")
	}
	reopened := f.observe(t, "reopened", "sha-C")
	if reopened.Generation != c.Generation+1 || f.count(t, "build") != 0 {
		t.Fatal("reopened Manual PR built automatically")
	}
}

func TestSchedulingAutomaticDeadlineResetOldWakeAndOverride(t *testing.T) {
	f := newScheduleFixture(t)
	if _, err := f.policy.Set(t.Context(), f.owner, f.repoID, installations.AutomaticBuilds); err != nil {
		t.Fatal(err)
	}
	a := f.observe(t, "opened", "sha-A")
	first, err := f.intents.Get(t.Context(), f.owner, a.PullRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Current.AvailableAt.Equal(f.clock.now().Add(env.QuietPeriod)) {
		t.Fatal("wrong first deadline")
	}
	f.clock.advance(4 * time.Minute)
	b := f.observe(t, "synchronize", "sha-B")
	latest, err := f.intents.Get(t.Context(), f.owner, b.PullRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Current.Revision != a.Revision+1 || !latest.Current.AvailableAt.Equal(f.clock.now().Add(env.QuietPeriod)) {
		t.Fatal("new head did not reset durable deadline")
	}
	f.clock.advance(time.Minute)
	if err := f.broker.Dispatch(t.Context(), wake(a)); err != nil {
		t.Fatal(err)
	}
	if err := f.broker.Dispatch(t.Context(), wake(b)); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "build") != 0 {
		t.Fatal("stale/early wake admitted build")
	}
	override, err := f.broker.RequestBuild(t.Context(), f.command(b, "manual-override-key-0001", b.Revision, env.BuildLatest))
	if err != nil {
		t.Fatal(err)
	}
	r := requestFromIntent(override.Intent)
	if override.Intent.Source != "manual" || override.Intent.Revision != b.Revision+1 {
		t.Fatal("override not fenced")
	}
	if err := f.broker.Dispatch(t.Context(), wake(b)); err != nil {
		t.Fatal(err)
	}
	if err := f.broker.Dispatch(t.Context(), wake(r)); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "build") != 1 {
		t.Fatal("manual override not immediately admitted")
	}
	replay, err := f.broker.RequestBuild(t.Context(), f.command(b, "manual-override-key-0001", b.Revision, env.BuildLatest))
	if err != nil || !replay.Coalesced || replay.Intent.Revision != r.Revision {
		t.Fatalf("duplicate request: %+v %v", replay, err)
	}
	if f.count(t, "environment_wake") != 3 {
		t.Fatal("duplicate created an extra wake-up")
	}
}

func TestSchedulingDueRecoveryAndAccessCancellation(t *testing.T) {
	for _, change := range []string{"none", "manual", "revoke", "suspend"} {
		t.Run(change, func(t *testing.T) {
			f := newScheduleFixture(t)
			if _, err := f.policy.Set(t.Context(), f.owner, f.repoID, installations.AutomaticBuilds); err != nil {
				t.Fatal(err)
			}
			r := f.observe(t, "opened", "sha-A")
			if _, err := f.pool.Exec(t.Context(), "DELETE FROM river_job WHERE kind='environment_wake'"); err != nil {
				t.Fatal(err)
			}
			restarted, err := queue.NewProducer(f.pool, f.logger)
			if err != nil {
				t.Fatal(err)
			}
			disabled := env.NewBroker(f.pool, restarted)
			if _, err := disabled.RecoverScheduling(t.Context(), 10); err != nil {
				t.Fatal(err)
			}
			if f.count(t, "environment_wake") != 1 || f.count(t, "build") != 0 {
				t.Fatal("durable wake not recovered")
			}
			if err := disabled.Dispatch(t.Context(), wake(r)); !errors.Is(err, env.ErrExecutionUnavailable) {
				t.Fatalf("disabled dispatch: %v", err)
			}
			if _, err := disabled.RequestBuild(t.Context(), f.command(r, "disabled-request-key-01", r.Revision, env.BuildLatest)); !errors.Is(err, env.ErrExecutionUnavailable) {
				t.Fatal("disabled manual admission accepted")
			}
			switch change {
			case "manual":
				if _, err := f.policy.Set(t.Context(), f.owner, f.repoID, installations.ManualBuilds); err != nil {
					t.Fatal(err)
				}
			case "revoke":
				if _, err := f.pool.Exec(t.Context(), "UPDATE repos SET accessible=false"); err != nil {
					t.Fatal(err)
				}
			case "suspend":
				if _, err := f.pool.Exec(t.Context(), "UPDATE installations SET status='suspended'"); err != nil {
					t.Fatal(err)
				}
			}
			f.clock.advance(env.QuietPeriod)
			if err := f.broker.Dispatch(t.Context(), wake(r)); err != nil {
				t.Fatal(err)
			}
			want := 0
			if change == "none" {
				want = 1
			}
			if f.count(t, "build") != want {
				t.Fatal("eligibility/access not respected")
			}
		})
	}
}

func TestSchedulingManualWakeRollbackAndConcurrentObservation(t *testing.T) {
	f := newScheduleFixture(t)
	r := f.observe(t, "opened", "sha-A")
	var err error
	runtime := newRuntime()
	s, err := env.NewService(f.repo, runtime, f.logger, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	op, err := env.NewOperationWorker(f.repo, &fakePublisher{}, f.logger, time.Minute, map[env.OperationKind]time.Duration{env.ActivateRoute: time.Second, env.RemoveRoute: time.Second, env.ReportStatus: time.Second, env.CleanupArtifact: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	failing, err := env.NewExecutionBroker(f.pool, failingOperationInserter{}, s, op, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failing.RequestBuild(t.Context(), f.command(r, "rollback-request-key-01", 0, env.BuildLatest)); err == nil {
		t.Fatal("wake failure accepted")
	}
	view, err := f.intents.Get(t.Context(), f.owner, r.PullRequestID)
	if err != nil || view.Current != nil {
		t.Fatalf("request committed without wake: %+v %v", view, err)
	}
	const updates = 8
	start := make(chan struct{})
	done := make(chan error, updates+1)
	for i := range updates {
		go func() {
			<-start
			_, err := f.events.ProcessPR(t.Context(), webhooks.PREvent{MergeableState: "clean", RepoID: f.repoID, Number: 1, Action: "synchronize", HeadSHA: fmt.Sprintf("sha-%d", i)})
			done <- err
		}()
	}
	go func() {
		<-start
		_, err := f.broker.RequestBuild(t.Context(), f.command(r, "concurrent-request-key-01", 0, env.BuildLatest))
		done <- err
	}()
	close(start)
	for range updates + 1 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	view, err = f.intents.Get(t.Context(), f.owner, r.PullRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Generation != updates+1 || view.Current == nil {
		t.Fatal("concurrent generation/intent lost")
	}
	// The one manual authorization may capture any serialized snapshot; it never
	// authorizes a later observed Manual generation automatically.
	if view.Current.Generation < view.Generation && view.Current.State != env.IntentSuperseded {
		t.Fatal("stale intent remained active")
	}
}

func TestSchedulingSameGenerationRebuildFencesOperationsAndCleanup(t *testing.T) {
	f := newScheduleFixture(t)
	r := f.observe(t, "opened", "sha-A")
	outcome, err := f.broker.RequestBuild(t.Context(), f.command(r, "first-build-request-001", 0, env.BuildLatest))
	if err != nil {
		t.Fatal(err)
	}
	a := requestFromIntent(outcome.Intent)
	f.provision(t, a)
	if ok, err := f.repo.Apply(t.Context(), a, env.Result{Status: "running", Artifact: env.Artifact{ContainerID: "old-candidate", ImageTag: "old-image"}}); err != nil || !ok {
		t.Fatalf("first apply: %v %v", ok, err)
	}
	if _, err := f.intents.Advance(t.Context(), env.IntentTransition{UserID: f.owner, PullRequestID: a.PullRequestID, Revision: a.Revision, Generation: a.Generation, Next: env.IntentSucceeded}); err != nil {
		t.Fatal(err)
	}
	rebuild, err := f.broker.RequestBuild(t.Context(), f.command(a, "rebuild-request-key-001", a.Revision, env.Rebuild))
	if err != nil {
		t.Fatal(err)
	}
	b := requestFromIntent(rebuild.Intent)
	if b.Generation != a.Generation || b.Revision != a.Revision+1 {
		t.Fatal("rebuild faked a generation")
	}
	for _, status := range []string{"running", "failed"} {
		if ok, err := f.repo.Apply(t.Context(), a, env.Result{Status: status, Artifact: env.Artifact{ContainerID: "late", ImageTag: "late-image"}, ReportFailure: true}); err != nil || ok {
			t.Fatalf("old result accepted: %v %v", ok, err)
		}
	}
	var oldOperation int64
	if err := f.pool.QueryRow(t.Context(), "SELECT id FROM environment_operations WHERE kind='activate_route'").Scan(&oldOperation); err != nil {
		t.Fatal(err)
	}
	publisher := &fakePublisher{}
	worker, err := env.NewOperationWorker(f.repo, publisher, f.logger, time.Minute, map[env.OperationKind]time.Duration{env.ActivateRoute: time.Second, env.RemoveRoute: time.Second, env.ReportStatus: time.Second, env.CleanupArtifact: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Process(t.Context(), oldOperation, "stale-operation", 1, 5); err != nil {
		t.Fatal(err)
	}
	if len(publisher.kinds) != 0 {
		t.Fatal("old operation reached external adapter")
	}
	if ok, err := f.repo.Apply(t.Context(), b, env.Result{Status: "running", Artifact: env.Artifact{ContainerID: "new-candidate", ImageTag: "new-image"}}); err != nil || !ok {
		t.Fatalf("new apply: %v %v", ok, err)
	}
	var keys int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(DISTINCT idempotency_key) FROM environment_operations WHERE kind='activate_route'").Scan(&keys); err != nil || keys != 2 {
		t.Fatalf("same-generation operation collision: %d %v", keys, err)
	}
	resources := []env.Resource{{PullRequestID: a.PullRequestID, Generation: a.Generation, Revision: a.Revision, Artifact: env.Artifact{ContainerID: "old-candidate", ImageTag: "old-image"}}, {PullRequestID: b.PullRequestID, Generation: b.Generation, Revision: b.Revision, Artifact: env.Artifact{ContainerID: "new-candidate", ImageTag: "new-image"}}}
	drainOperations(t, f.fixture, publisher)
	if err := f.repo.ScheduleResourceCleanup(t.Context(), b, resources); err != nil {
		t.Fatal(err)
	}
	var cleanup int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(DISTINCT payload->'resource'->'artifact'->>'container_id') FROM environment_operations WHERE kind='cleanup_artifact'").Scan(&cleanup); err != nil || cleanup != 1 {
		t.Fatalf("wrong cleanup ownership: %d %v", cleanup, err)
	}
}

func TestSchedulingGenerationZeroPublication(t *testing.T) {
	f := newScheduleFixture(t)
	var id int64
	if err := f.pool.QueryRow(t.Context(), "INSERT INTO pull_requests(repo_id,number,head_sha,build_status) VALUES($1,1,'bootstrap-sha','open') RETURNING id", f.repoID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	r := env.NewRequest(id, 0, "bootstrap-sha", "open")
	outcome, err := f.broker.RequestBuild(t.Context(), f.command(r, "bootstrap-request-key-01", 0, env.BuildLatest))
	if err != nil {
		t.Fatal(err)
	}
	r = requestFromIntent(outcome.Intent)
	if err := f.broker.Dispatch(t.Context(), wake(r)); err != nil {
		t.Fatal(err)
	}
	var args []byte
	if err := f.pool.QueryRow(t.Context(), "SELECT args FROM river_job WHERE kind='build'").Scan(&args); err != nil {
		t.Fatal(err)
	}
	var captured env.Request
	if err := json.Unmarshal(args, &captured); err != nil || captured.Generation != 0 {
		t.Fatal("bootstrap generation changed")
	}
	f.provision(t, r)
	if ok, err := f.repo.Apply(t.Context(), r, env.Result{Status: "running", Artifact: env.Artifact{ContainerID: "zero-container", ImageTag: "zero-image"}}); err != nil || !ok {
		t.Fatalf("generation-zero publication: %v %v", ok, err)
	}
	// Test adapters can consume zero-generation operations; no live API is used.
	drainOperations(t, f.fixture, &fakePublisher{})
	snapshot, err := f.repo.Snapshot(t.Context(), id)
	if err != nil || snapshot.AppliedGeneration != 0 || snapshot.AppliedRevision != r.Revision || snapshot.Status != "running" {
		t.Fatalf("zero finalization: %+v %v", snapshot, err)
	}
}

func TestSchedulingWorkerRechecksRevokedOrCancelledAccess(t *testing.T) {
	for _, change := range []string{"manual", "revoke"} {
		t.Run(change, func(t *testing.T) {
			f := newScheduleFixture(t)
			if _, err := f.policy.Set(t.Context(), f.owner, f.repoID, installations.AutomaticBuilds); err != nil {
				t.Fatal(err)
			}
			r := f.observe(t, "opened", "sha-A")
			f.clock.advance(env.QuietPeriod)
			if err := f.broker.Dispatch(t.Context(), wake(r)); err != nil {
				t.Fatal(err)
			}
			f.provision(t, r)
			if change == "manual" {
				if _, err := f.policy.Set(t.Context(), f.owner, f.repoID, installations.ManualBuilds); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := f.pool.Exec(t.Context(), "UPDATE repos SET accessible=false"); err != nil {
					t.Fatal(err)
				}
			}
			runtime := newRuntime()
			s, err := env.NewService(f.repo, runtime, f.logger, time.Minute, f.clock.now)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Build(t.Context(), r); err != nil {
				t.Fatal(err)
			}
			if len(runtime.started) != 0 {
				t.Fatal("revoked/cancelled queued work called runtime")
			}
		})
	}
}

// A handoff must keep a newer automatic deadline even when the old job completes.
func TestSchedulingHandoffRespectsFutureDeadlineAndManualNoSuccessor(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		for _, mode := range []installations.BuildMode{installations.ManualBuilds, installations.AutomaticBuilds} {
			t.Run(fmt.Sprintf("%s/terminal=%t", mode, terminal), func(t *testing.T) {
				f := newScheduleFixture(t)
				if _, err := f.policy.Set(t.Context(), f.owner, f.repoID, mode); err != nil {
					t.Fatal(err)
				}
				a := f.observe(t, "opened", "sha-A")
				if mode == installations.ManualBuilds {
					o, err := f.broker.RequestBuild(t.Context(), f.command(a, "handoff-request-key-001", 0, env.BuildLatest))
					if err != nil {
						t.Fatal(err)
					}
					a = requestFromIntent(o.Intent)
				} else {
					f.clock.advance(env.QuietPeriod)
				}
				if err := f.broker.Dispatch(t.Context(), wake(a)); err != nil {
					t.Fatal(err)
				}
				f.provision(t, a)
				runtime := newRuntime()
				runtime.gate[a.Generation] = make(chan struct{})
				s, err := env.NewService(f.repo, runtime, f.logger, time.Minute, f.clock.now)
				if err != nil {
					t.Fatal(err)
				}
				workers := river.NewWorkers()
				env.RegisterWorkers(workers, s, f.broker)
				client, err := river.NewClient(riverpgxv5.New(f.pool), &river.Config{Logger: f.logger, Workers: workers, Queues: map[string]river.QueueConfig{env.BuildQueue: {MaxWorkers: 1}, env.TeardownQueue: {MaxWorkers: 1}}})
				if err != nil {
					t.Fatal(err)
				}
				if terminal {
					runtime.cleanupFailures = 1
					if _, err := f.pool.Exec(t.Context(), "UPDATE river_job SET max_attempts=1 WHERE kind='build'"); err != nil {
						t.Fatal(err)
					}
				}
				events, cancel := client.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
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
				waitStarted(t, runtime)
				b := f.observe(t, "synchronize", "sha-B")
				close(runtime.gate[a.Generation])
				select {
				case <-events:
				case <-time.After(10 * time.Second):
					t.Fatal("old job did not finish")
				}
				if f.count(t, "build") != 1 {
					t.Fatal("handoff admitted unauthorized/early successor")
				}
				if mode == installations.AutomaticBuilds {
					f.clock.advance(env.QuietPeriod)
					if err := f.broker.Dispatch(t.Context(), wake(b)); err != nil {
						t.Fatal(err)
					}
					waitStarted(t, runtime)
				}
			})
		}
	}
}

func TestSchedulingRiverWakeWorkerAndPRSlot(t *testing.T) {
	f := newScheduleFixture(t)
	observed := f.observe(t, "opened", "sha-A")
	o, err := f.broker.RequestBuild(t.Context(), f.command(observed, "river-wake-request-key-01", 0, env.BuildLatest))
	if err != nil {
		t.Fatal(err)
	}
	a := requestFromIntent(o.Intent)
	f.provision(t, a)
	runtime := newRuntime()
	runtime.gate[a.Generation] = make(chan struct{})
	s, err := env.NewService(f.repo, runtime, f.logger, time.Minute, f.clock.now)
	if err != nil {
		t.Fatal(err)
	}
	workers := river.NewWorkers()
	env.RegisterWorkers(workers, s, f.broker)
	client, err := river.NewClient(riverpgxv5.New(f.pool), &river.Config{Logger: f.logger, Workers: workers, Queues: map[string]river.QueueConfig{env.CoordinationQueue: {MaxWorkers: 1}, env.BuildQueue: {MaxWorkers: 1}, env.TeardownQueue: {MaxWorkers: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	events, cancel := client.Subscribe(river.EventKindJobCompleted)
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
	waitStarted(t, runtime)
	b := f.observe(t, "synchronize", "sha-B")
	if _, err := f.broker.RequestBuild(t.Context(), f.command(b, "river-wake-request-key-02", a.Revision, env.BuildLatest)); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	completedWakes := 0
	for completedWakes < 2 {
		select {
		case e := <-events:
			if e.Job.Kind == "environment_wake" {
				completedWakes++
			}
		case <-deadline:
			t.Fatal("wake worker blocked behind executable PR slot")
		}
	}
	var active int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM river_job WHERE kind IN ('build','teardown') AND state IN ('available','running','scheduled','retryable')").Scan(&active); err != nil || active != 1 {
		t.Fatalf("parallel PR execution: %d %v", active, err)
	}
	close(runtime.gate[a.Generation])
	waitStarted(t, runtime)
	completedBuilds := 0
	for completedBuilds < 2 {
		select {
		case e := <-events:
			if e.Job.Kind == "build" {
				completedBuilds++
			}
		case <-deadline:
			t.Fatal("wake/handoff lost a request")
		}
	}
}

func TestSchedulingRevisionChangesDuringExternalPublication(t *testing.T) {
	f := newScheduleFixture(t)
	observed := f.observe(t, "opened", "sha-A")
	o, err := f.broker.RequestBuild(t.Context(), f.command(observed, "publication-first-key-01", 0, env.BuildLatest))
	if err != nil {
		t.Fatal(err)
	}
	a := requestFromIntent(o.Intent)
	f.provision(t, a)
	if ok, err := f.repo.Apply(t.Context(), a, env.Result{Status: "running", Artifact: env.Artifact{ContainerID: "candidate-A", ImageTag: "image-A"}}); err != nil || !ok {
		t.Fatalf("apply: %v %v", ok, err)
	}
	var id int64
	if err := f.pool.QueryRow(t.Context(), "SELECT id FROM environment_operations WHERE kind='activate_route'").Scan(&id); err != nil {
		t.Fatal(err)
	}
	p := &fakePublisher{entered: make(chan struct{}, 1), release: make(chan struct{})}
	w, err := env.NewOperationWorker(f.repo, p, f.logger, time.Minute, map[env.OperationKind]time.Duration{env.ActivateRoute: 10 * time.Second, env.RemoveRoute: time.Second, env.ReportStatus: time.Second, env.CleanupArtifact: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- w.Process(t.Context(), id, "old-route", 1, 5) }()
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not enter adapter")
	}
	if _, err := f.intents.Advance(t.Context(), env.IntentTransition{UserID: f.owner, PullRequestID: a.PullRequestID, Generation: a.Generation, Revision: a.Revision, Next: env.IntentSucceeded}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.broker.RequestBuild(t.Context(), f.command(a, "publication-rebuild-key-01", a.Revision, env.Rebuild)); err != nil {
		t.Fatal(err)
	}
	close(p.release)
	waitResult(t, done)
	var state string
	if err := f.pool.QueryRow(t.Context(), "SELECT state FROM environment_operations WHERE id=$1", id).Scan(&state); err != nil || state != "superseded" {
		t.Fatalf("stale finalization: %s %v", state, err)
	}
	snapshot, err := f.repo.Snapshot(t.Context(), a.PullRequestID)
	if err != nil || snapshot.Artifact.ContainerID != "" || snapshot.AppliedRevision != 0 || snapshot.Revision != a.Revision+1 {
		t.Fatalf("stale candidate activated: %+v %v", snapshot, err)
	}
}

func TestSchedulingAtomicRevisionCheckAfterLockWait(t *testing.T) {
	f := newScheduleFixture(t)
	observed := f.observe(t, "opened", "sha-A")
	o, err := f.broker.RequestBuild(t.Context(), f.command(observed, "atomic-revision-request-01", 0, env.BuildLatest))
	if err != nil {
		t.Fatal(err)
	}
	a := requestFromIntent(o.Intent)
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
	// Hold the full production lock order while replacing the request, but keep
	// generation/SHA unchanged. The result transaction must see the new revision
	// after waiting; a snapshot from before the wait must not authorize its write.
	for _, statement := range []string{
		"SELECT id FROM installations FOR UPDATE",
		"SELECT id FROM repos FOR UPDATE",
		"SELECT id FROM pull_requests FOR UPDATE",
		"UPDATE build_intents SET revision=revision+1,base_revision=revision,state='pending',started_at=NULL,completed_at=NULL",
	} {
		if _, err := tx.Exec(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	type result struct {
		applied bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		ok, err := f.repo.Apply(t.Context(), a, env.Result{Status: "running", Artifact: env.Artifact{ContainerID: "old", ImageTag: "old-image"}})
		done <- result{ok, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := f.pool.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE '-- name: LockIntentInstallation%')").Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("result did not wait for request locks")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	completed := <-done
	if completed.err != nil || completed.applied {
		t.Fatalf("obsolete revision wrote after lock wait: %+v", completed)
	}
	snapshot, err := f.repo.Snapshot(t.Context(), a.PullRequestID)
	if err != nil || snapshot.Status != "queued" || snapshot.PendingArtifact.ContainerID != "" {
		t.Fatalf("stale write changed environment: %+v %v", snapshot, err)
	}
}
