package environments_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	env "github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/webhooks"
	"github.com/google/go-github/v91/github"
)

// runningPreview opens a PR and publishes its first build through the fence.
func runningPreview(t *testing.T, f *fixture) (env.Request, *fakeRuntime, *env.Service, *fenceExecutor) {
	t.Helper()
	a := f.event(t, "opened", "sha-A")
	f.provision(t, a)
	r := newRuntime()
	s := service(t, f, r, &fakePublisher{})
	if err := s.Build(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	executor := &fenceExecutor{repo: f.repo, runtime: r}
	drainWith(t, f, executor)
	if snapshot, _ := f.repo.Snapshot(t.Context(), a.PullRequestID); snapshot.Status != "running" {
		t.Fatalf("preview not running: %+v", snapshot)
	}
	return a, r, s, executor
}

func prTeardownState(t *testing.T, f *fixture, pr int64) (generation, requested, completed int64, reason string) {
	t.Helper()
	var r *string
	if err := f.pool.QueryRow(t.Context(), "SELECT generation, teardown_requested_generation, teardown_completed_generation, teardown_reason FROM pull_requests WHERE id=$1", pr).
		Scan(&generation, &requested, &completed, &r); err != nil {
		t.Fatal(err)
	}
	if r != nil {
		reason = *r
	}
	return generation, requested, completed, reason
}

func TestLocalTeardownNeedsNoAccessAndCleansEverything(t *testing.T) {
	f := newFixture(t)
	a, r, _, executor := runningPreview(t, f)
	// Access is gone (repository removed from the installation).
	if _, err := f.pool.Exec(t.Context(), "UPDATE repos SET accessible=false WHERE id=$1", f.repoID); err != nil {
		t.Fatal(err)
	}
	started, err := f.repo.RequestTeardown(t.Context(), a.PullRequestID, env.TeardownRevoked)
	if err != nil || !started {
		t.Fatalf("teardown started=%v err=%v", started, err)
	}
	generation, requested, completed, reason := prTeardownState(t, f, a.PullRequestID)
	if generation != a.Generation+1 || requested != generation || completed != generation || reason != "revoked" {
		t.Fatalf("teardown request g=%d requested=%d completed=%d reason=%q", generation, requested, completed, reason)
	}
	var intentState string
	if err := f.pool.QueryRow(t.Context(), "SELECT state FROM build_intents WHERE pull_request_id=$1", a.PullRequestID).Scan(&intentState); err != nil || intentState != "superseded" {
		t.Fatalf("intent %q %v", intentState, err)
	}
	snapshot, _ := f.repo.Snapshot(t.Context(), a.PullRequestID)
	if snapshot.Status != "tearing_down" || snapshot.Artifact.ContainerID != "" {
		t.Fatalf("environment %+v", snapshot)
	}
	removal := loadOperation(t, f, env.RemoveRoute, generation)
	if removal.Payload.Previous.ContainerID != "container-1" || removal.Payload.Request.State != "open" {
		t.Fatalf("removal payload %+v", removal.Payload)
	}
	// A duplicate request changes nothing.
	if again, err := f.repo.RequestTeardown(t.Context(), a.PullRequestID, env.TeardownRevoked); err != nil || again {
		t.Fatalf("duplicate teardown started=%v err=%v", again, err)
	}
	drainWith(t, f, executor)
	snapshot, _ = f.repo.Snapshot(t.Context(), a.PullRequestID)
	if snapshot.Status != "torn_down" || snapshot.AppliedGeneration != generation {
		t.Fatalf("teardown not finished: %+v", snapshot)
	}
	if state := routeState(t, f, a.PullRequestID); state.Active || !state.Served || state.ServedGeneration != generation {
		t.Fatalf("route %+v", state)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.resources) != 0 || len(r.removedContainers) != 1 || len(r.removedImages) != 1 {
		t.Fatalf("leftovers %+v removed %v %v", r.resources, r.removedContainers, r.removedImages)
	}
	var lifetime int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM environments WHERE pull_request_id=$1 AND subdomain='preview-test'", a.PullRequestID).Scan(&lifetime); err != nil || lifetime != 1 {
		t.Fatal("lifetime environment identity was not retained")
	}
}

func TestRequestTeardownOfAPRWithNothingLiveIsANoOp(t *testing.T) {
	f := newFixture(t)
	a := f.event(t, "opened", "sha-A")
	if _, err := f.pool.Exec(t.Context(), "UPDATE build_intents SET state='cancelled', completed_at=clock_timestamp() WHERE pull_request_id=$1", a.PullRequestID); err != nil {
		t.Fatal(err)
	}
	if started, err := f.repo.RequestTeardown(t.Context(), a.PullRequestID, env.TeardownRequested); err != nil || started {
		t.Fatalf("started=%v err=%v", started, err)
	}
	if generation, requested, _, _ := prTeardownState(t, f, a.PullRequestID); generation != a.Generation || requested != -1 {
		t.Fatal("a PR without a preview or build was fenced anyway")
	}
}

func TestRevocationWebhookRequestsTeardownAndSweepClearsTheMarker(t *testing.T) {
	f := newFixture(t)
	a, r, _, executor := runningPreview(t, f)
	deliveries := webhooks.NewDeliveryService(f.pool, f.broker, nil)
	event := &github.InstallationEvent{Action: new("suspend"), Installation: &github.Installation{
		ID: new(int64(1)), Account: &github.User{Login: new("acct")}, UpdatedAt: &github.Timestamp{Time: time.Now()},
	}}
	sum := sha256.Sum256([]byte("suspend"))
	if err := deliveries.Process(t.Context(), "revocation-delivery-1", "installation", hex.EncodeToString(sum[:]), event); err != nil {
		t.Fatal(err)
	}
	generation, requested, completed, reason := prTeardownState(t, f, a.PullRequestID)
	if generation != a.Generation+1 || requested != generation || completed >= generation || reason != "revoked" {
		t.Fatalf("revocation request g=%d requested=%d completed=%d reason=%q", generation, requested, completed, reason)
	}
	var pending bool
	var wakeups int
	if err := f.pool.QueryRow(t.Context(), "SELECT cleanup_pending FROM installations WHERE github_installation_id=1").Scan(&pending); err != nil || !pending {
		t.Fatal("cleanup marker not set")
	}
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM river_job WHERE kind='environment_teardown_sweep'").Scan(&wakeups); err != nil || wakeups != 1 {
		t.Fatalf("sweep wake-ups %d %v", wakeups, err)
	}
	sweeper, err := env.NewTeardownSweeper(f.repo, r, f.logger, 10)
	if err != nil {
		t.Fatal(err)
	}
	sweep, err := sweeper.RunOnce(t.Context())
	if err != nil || sweep.Started != 1 || sweep.Cleared != 0 {
		t.Fatalf("first sweep %+v %v", sweep, err)
	}
	// The removal and cleanup run without any GitHub access.
	drainWith(t, f, executor)
	sweep, err = sweeper.RunOnce(t.Context())
	if err != nil || sweep.Cleared != 1 {
		t.Fatalf("second sweep %+v %v", sweep, err)
	}
	if err := f.pool.QueryRow(t.Context(), "SELECT cleanup_pending FROM installations WHERE github_installation_id=1").Scan(&pending); err != nil || pending {
		t.Fatal("cleanup marker not cleared")
	}
	snapshot, _ := f.repo.Snapshot(t.Context(), a.PullRequestID)
	if snapshot.Status != "torn_down" {
		t.Fatalf("environment %+v", snapshot)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.resources) != 0 {
		t.Fatalf("leftovers %+v", r.resources)
	}
}

func TestSweepKeepsTheMarkerWhileDockerStillHoldsResources(t *testing.T) {
	f := newFixture(t)
	a, r, _, executor := runningPreview(t, f)
	if _, err := f.pool.Exec(t.Context(), "UPDATE installations SET status='suspended', cleanup_pending=true WHERE github_installation_id=1"); err != nil {
		t.Fatal(err)
	}
	sweeper, err := env.NewTeardownSweeper(f.repo, r, f.logger, 10)
	if err != nil {
		t.Fatal(err)
	}
	// The catch-all requests teardown for anything revoked now.
	if sweep, err := sweeper.RunOnce(t.Context()); err != nil || sweep.Started != 1 {
		t.Fatalf("sweep %+v %v", sweep, err)
	}
	drainWith(t, f, executor)
	// A leftover Docker found for the PR (a crashed attempt) keeps the marker
	// and is scheduled for removal.
	r.mu.Lock()
	r.resources[a.Generation] = env.Artifact{ContainerID: "leftover", ImageTag: "leftover-image"}
	r.mu.Unlock()
	if sweep, err := sweeper.RunOnce(t.Context()); err != nil || sweep.Cleared != 0 {
		t.Fatalf("cleared with a leftover: %+v %v", sweep, err)
	}
	drainWith(t, f, executor)
	if sweep, err := sweeper.RunOnce(t.Context()); err != nil || sweep.Cleared != 1 {
		t.Fatalf("not cleared after the leftover went: %+v %v", sweep, err)
	}
}

func TestFailedCleanupIsRetriedWhenScheduledAgain(t *testing.T) {
	f := newFixture(t)
	a, r, s, executor := runningPreview(t, f)
	b := f.event(t, "synchronize", "sha-B")
	if err := s.Build(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	processKind(t, f, executor, env.ActivateRoute, b.Generation)
	if _, err := f.pool.Exec(t.Context(), "DELETE FROM environment_operations WHERE kind='cleanup_artifact'"); err != nil {
		t.Fatal(err)
	}
	leftover := []env.Resource{{PullRequestID: a.PullRequestID, Generation: a.Generation, Revision: a.Revision, Artifact: env.Artifact{ContainerID: "container-1", ImageTag: "image-1"}}}
	if err := f.repo.ScheduleResourceCleanup(t.Context(), b, leftover); err != nil {
		t.Fatal(err)
	}
	executor.fail = map[env.OperationKind]error{env.CleanupArtifact: &env.OperationFailure{Code: "not_owned", Permanent: true}}
	drainWith(t, f, executor)
	var id int64
	var state string
	if err := f.pool.QueryRow(t.Context(), "SELECT id, state FROM environment_operations WHERE kind='cleanup_artifact'").Scan(&id, &state); err != nil || state != "failed" {
		t.Fatalf("cleanup %s %v", state, err)
	}
	executor.fail = nil
	if err := f.repo.ScheduleResourceCleanup(t.Context(), b, leftover); err != nil {
		t.Fatal(err)
	}
	if state := operationState(t, f, id); state != "pending" {
		t.Fatalf("failed cleanup not rearmed: %s", state)
	}
	drainWith(t, f, executor)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, left := r.resources[a.Generation]; left {
		t.Fatal("rearmed cleanup did not remove the leftover")
	}
}

func TestIdempotentCleanupIsNeverGivenUp(t *testing.T) {
	f := newFixture(t)
	a, _, _, executor := runningPreview(t, f)
	closed := f.event(t, "closed", "sha-A")
	s := service(t, f, executor.runtime, &fakePublisher{})
	if err := s.Teardown(t.Context(), closed); err != nil {
		t.Fatal(err)
	}
	removal := loadOperation(t, f, env.RemoveRoute, closed.Generation)
	executor.fail = map[env.OperationKind]error{env.RemoveRoute: &env.OperationFailure{Code: "router_unavailable"}}
	timeouts := map[env.OperationKind]time.Duration{env.ActivateRoute: time.Second, env.RemoveRoute: time.Second, env.ReportStatus: time.Second, env.CleanupArtifact: time.Second}
	worker, err := env.NewOperationWorker(f.repo, executor, f.logger, 5*time.Second, timeouts)
	if err != nil {
		t.Fatal(err)
	}
	// The final River attempt fails too: the removal stays retryable.
	if err := worker.Process(t.Context(), removal.ID, "final-attempt", 5, 5); err == nil {
		t.Fatal("failure not reported")
	}
	if state := operationState(t, f, removal.ID); state != "retryable" {
		t.Fatalf("route removal gave up: %s", state)
	}
	if _, err := f.pool.Exec(t.Context(), "UPDATE environment_operations SET available_at=now() WHERE id=$1", removal.ID); err != nil {
		t.Fatal(err)
	}
	executor.fail = nil
	drainWith(t, f, executor)
	if snapshot, _ := f.repo.Snapshot(t.Context(), a.PullRequestID); snapshot.Status != "torn_down" {
		t.Fatalf("teardown did not resume: %+v", snapshot)
	}
}

func TestAdmissionProvisionsOneLifetimeEnvironment(t *testing.T) {
	f := newFixture(t)
	if _, err := f.pool.Exec(t.Context(), "UPDATE repos SET github_repo_id=1296 WHERE id=$1", f.repoID); err != nil {
		t.Fatal(err)
	}
	var otherPR int64
	if err := f.pool.QueryRow(t.Context(), "INSERT INTO pull_requests(repo_id, number, head_sha) VALUES($1, 99, 'other') RETURNING id", f.repoID).Scan(&otherPR); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), "INSERT INTO environments(pull_request_id, subdomain) VALUES($1, 'taken')", otherPR); err != nil {
		t.Fatal(err)
	}
	calls := 0
	f.repo.WithHostnames(func(number int32, repoID int64) (string, error) {
		calls++
		if calls < 3 {
			return "taken", nil // collides; another label is drawn
		}
		return fmt.Sprintf("pr-%d-%s-token%d", number, strconv.FormatInt(repoID, 36), calls), nil
	})
	a := f.event(t, "opened", "sha-A")
	r := newRuntime()
	s := service(t, f, r, &fakePublisher{})
	if err := s.Build(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	var id int64
	var subdomain string
	if err := f.pool.QueryRow(t.Context(), "SELECT id, subdomain FROM environments WHERE pull_request_id=$1", a.PullRequestID).Scan(&id, &subdomain); err != nil {
		t.Fatal(err)
	}
	if subdomain != "pr-1-100-token3" || calls != 3 {
		t.Fatalf("hostname %q after %d allocations", subdomain, calls)
	}
	if len(r.attempts) != 1 || r.attempts[0].EnvironmentID != id {
		t.Fatalf("attempt %+v", r.attempts)
	}
	b := f.event(t, "synchronize", "sha-B")
	if err := s.Build(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM environments WHERE pull_request_id=$1 AND subdomain=$2", a.PullRequestID, subdomain).Scan(&rows); err != nil || rows != 1 || calls != 3 {
		t.Fatalf("rebuild changed the lifetime environment: rows=%d calls=%d", rows, calls)
	}
	f.repo.WithHostnames(func(int32, int64) (string, error) { return "", errors.New("no randomness") })
	c := f.event(t, "synchronize", "sha-C")
	if err := s.Build(t.Context(), c); err != nil {
		t.Fatal("an existing environment needed a new hostname")
	}
}

func TestRetriedTeardownIsIdempotent(t *testing.T) {
	f := newFixture(t)
	a, _, s, executor := runningPreview(t, f)
	closed := f.event(t, "closed", "sha-A")
	// Two attempts of the same teardown (a retried job after a crash, or two
	// racing calls) start it once and carry the same artifacts.
	for range 2 {
		applied, err := f.repo.Apply(t.Context(), closed, env.Result{Status: "torn_down"})
		if err != nil || !applied {
			t.Fatalf("applied=%v err=%v", applied, err)
		}
	}
	if err := s.Teardown(t.Context(), closed); err != nil {
		t.Fatal(err)
	}
	var removals int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM environment_operations WHERE kind='remove_route' AND pull_request_id=$1", a.PullRequestID).Scan(&removals); err != nil || removals != 1 {
		t.Fatalf("removals %d %v", removals, err)
	}
	if removal := loadOperation(t, f, env.RemoveRoute, closed.Generation); removal.Payload.Previous.ContainerID != "container-1" {
		t.Fatalf("the retry replaced the taken-over artifacts: %+v", removal.Payload)
	}
	drainWith(t, f, executor)
	if snapshot, _ := f.repo.Snapshot(t.Context(), a.PullRequestID); snapshot.Status != "torn_down" {
		t.Fatalf("environment %+v", snapshot)
	}
}
