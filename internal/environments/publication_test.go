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
	"github.com/jackc/pgx/v5"
)

// fenceExecutor stands in for the route controller: it drives the real
// PostgreSQL fence (accept, then mark served) with no router, and removes
// resources from the fake runtime for cleanup operations.
type fenceExecutor struct {
	repo    *env.Repository
	runtime *fakeRuntime
	mu      sync.Mutex
	fail    map[env.OperationKind]error
	kinds   []env.OperationKind
}

func (e *fenceExecutor) Execute(ctx context.Context, op env.Operation) error {
	e.mu.Lock()
	e.kinds = append(e.kinds, op.Kind)
	failure := e.fail[op.Kind]
	e.mu.Unlock()
	if failure != nil {
		return failure
	}
	switch op.Kind {
	case env.ActivateRoute:
		decision, err := e.repo.AcceptRoute(ctx, op, env.RouteTarget{Artifact: op.Payload.Artifact, Backend: "backend-" + op.Payload.Artifact.ContainerID, Port: 8080, NetworkID: "network"})
		if err != nil {
			return err
		}
		if !decision.Accepted {
			return env.ErrSuperseded
		}
		_, err = e.repo.MarkRouteServed(ctx, decision.EnvironmentID, op.Generation, op.Revision)
		return err
	case env.RemoveRoute:
		decision, err := e.repo.AcceptRouteRemoval(ctx, op)
		if err != nil {
			return err
		}
		if !decision.Accepted {
			return env.ErrSuperseded
		}
		if decision.EnvironmentID != 0 {
			_, err = e.repo.MarkRouteServed(ctx, decision.EnvironmentID, op.Generation, 0)
		}
		return err
	case env.CleanupArtifact:
		if id := op.Payload.Resource.Artifact.ContainerID; id != "" {
			if err := e.runtime.RemoveContainer(ctx, id); err != nil {
				return err
			}
		}
		if image := op.Payload.Resource.Artifact.ImageTag; image != "" {
			return e.runtime.RemoveImage(ctx, image)
		}
	}
	return nil
}

func drainWith(t *testing.T, f *fixture, executor env.OperationExecutor) {
	t.Helper()
	timeouts := map[env.OperationKind]time.Duration{env.ActivateRoute: time.Second, env.RemoveRoute: time.Second, env.ReportStatus: time.Second, env.CleanupArtifact: time.Second}
	worker, err := env.NewOperationWorker(f.repo, executor, f.logger, 5*time.Second, timeouts)
	if err != nil {
		t.Fatal(err)
	}
	for processed := 0; processed < 100; processed++ {
		var id int64
		if err := f.pool.QueryRow(t.Context(), "SELECT id FROM environment_operations WHERE state IN ('pending','retryable') AND available_at <= now() ORDER BY id LIMIT 1").Scan(&id); errors.Is(err, pgx.ErrNoRows) {
			return
		} else if err != nil {
			t.Fatal(err)
		}
		_ = worker.Process(t.Context(), id, fmt.Sprintf("test-%d-%d", id, processed), 1, 5)
	}
	t.Fatal("operation queue did not drain")
}

// processKind runs the newest pending operation of a kind and generation.
func processKind(t *testing.T, f *fixture, executor env.OperationExecutor, kind env.OperationKind, generation int64) {
	t.Helper()
	timeouts := map[env.OperationKind]time.Duration{env.ActivateRoute: time.Second, env.RemoveRoute: time.Second, env.ReportStatus: time.Second, env.CleanupArtifact: time.Second}
	worker, err := env.NewOperationWorker(f.repo, executor, f.logger, 5*time.Second, timeouts)
	if err != nil {
		t.Fatal(err)
	}
	op := loadOperation(t, f, kind, generation)
	_ = worker.Process(t.Context(), op.ID, fmt.Sprintf("process-%d", op.ID), 1, 5)
}

func loadOperation(t *testing.T, f *fixture, kind env.OperationKind, generation int64) env.Operation {
	t.Helper()
	var op env.Operation
	var raw []byte
	var state string
	if err := f.pool.QueryRow(t.Context(), "SELECT id, pull_request_id, generation, revision, payload, state FROM environment_operations WHERE kind=$1 AND generation=$2 ORDER BY id DESC LIMIT 1", string(kind), generation).
		Scan(&op.ID, &op.PullRequestID, &op.Generation, &op.Revision, &raw, &state); err != nil {
		t.Fatalf("load %s operation of generation %d: %v", kind, generation, err)
	}
	if err := json.Unmarshal(raw, &op.Payload); err != nil {
		t.Fatal(err)
	}
	op.Kind, op.State = kind, env.OperationState(state)
	return op
}

func operationState(t *testing.T, f *fixture, id int64) string {
	t.Helper()
	var state string
	if err := f.pool.QueryRow(t.Context(), "SELECT state FROM environment_operations WHERE id=$1", id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func routeState(t *testing.T, f *fixture, pr int64) env.RouteState {
	t.Helper()
	state, ok, err := f.repo.RouteState(t.Context(), pr)
	if err != nil || !ok {
		t.Fatalf("route state %v %v", ok, err)
	}
	return state
}

func TestRouteFenceAcceptsOnlyNewerVersions(t *testing.T) {
	f := newFixture(t)
	a := f.event(t, "opened", "sha-A")
	f.provision(t, a)
	r := newRuntime()
	s := service(t, f, r, &fakePublisher{})
	if err := s.Build(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	opA := loadOperation(t, f, env.ActivateRoute, a.Generation)
	target := env.RouteTarget{Artifact: opA.Payload.Artifact, Backend: "previa-pr-a", Port: 8080, NetworkID: "net"}
	first, err := f.repo.AcceptRoute(t.Context(), opA, target)
	if err != nil || !first.Accepted || first.TableVersion != 1 || first.EnvironmentID == 0 {
		t.Fatalf("first acceptance %+v %v", first, err)
	}
	again, err := f.repo.AcceptRoute(t.Context(), opA, target)
	if err != nil || !again.Accepted || again.TableVersion != 1 {
		t.Fatalf("retried acceptance %+v %v", again, err)
	}
	if _, err := f.repo.AcceptRoute(t.Context(), opA, env.RouteTarget{Artifact: env.Artifact{ContainerID: "other", ImageTag: "other"}, Backend: "b", Port: 1, NetworkID: "n"}); err == nil {
		t.Fatal("accepted a target that is not the operation's candidate")
	}
	notPending := opA
	notPending.Payload.Artifact = env.Artifact{ContainerID: "someone-else", ImageTag: "image-x"}
	if d, err := f.repo.AcceptRoute(t.Context(), notPending, env.RouteTarget{Artifact: notPending.Payload.Artifact, Backend: "b", Port: 1, NetworkID: "n"}); err != nil || d.Accepted {
		t.Fatalf("accepted a candidate that is not pending: %+v %v", d, err)
	}
	table, err := f.repo.RouteTable(t.Context())
	if err != nil || table.Version != 1 || len(table.Routes) != 1 || table.Routes[0].Hostname != "preview-test" || table.Routes[0].Backend != "previa-pr-a" || table.Routes[0].Port != 8080 {
		t.Fatalf("route table %+v %v", table, err)
	}
	if served, err := f.repo.MarkRouteServed(t.Context(), first.EnvironmentID, opA.Generation, opA.Revision); err != nil || !served {
		t.Fatalf("served %v %v", served, err)
	}

	b := f.event(t, "synchronize", "sha-B")
	if err := s.Build(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	opB := loadOperation(t, f, env.ActivateRoute, b.Generation)
	second, err := f.repo.AcceptRoute(t.Context(), opB, env.RouteTarget{Artifact: opB.Payload.Artifact, Backend: "previa-pr-b", Port: 8080, NetworkID: "net"})
	if err != nil || !second.Accepted || second.TableVersion != 2 {
		t.Fatalf("newer acceptance %+v %v", second, err)
	}
	// A late activation of A can never replace B, and A is no longer served.
	if d, err := f.repo.AcceptRoute(t.Context(), opA, target); err != nil || d.Accepted {
		t.Fatalf("stale activation accepted: %+v %v", d, err)
	}
	if served, _ := f.repo.MarkRouteServed(t.Context(), first.EnvironmentID, opA.Generation, opA.Revision); served {
		t.Fatal("older version recorded as served")
	}
	state := routeState(t, f, a.PullRequestID)
	if state.Generation != b.Generation || state.Target != opB.Payload.Artifact || state.ServedWith != opA.Payload.Artifact {
		t.Fatalf("route state %+v", state)
	}
}

func TestRouteRemovalTombstoneBlocksLateActivationUntilReopen(t *testing.T) {
	f := newFixture(t)
	a := f.event(t, "opened", "sha-A")
	f.provision(t, a)
	r := newRuntime()
	s := service(t, f, r, &fakePublisher{})
	if err := s.Build(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	opA := loadOperation(t, f, env.ActivateRoute, a.Generation)
	closed := f.event(t, "closed", "sha-A")
	if err := s.Teardown(t.Context(), closed); err != nil {
		t.Fatal(err)
	}
	opR := loadOperation(t, f, env.RemoveRoute, closed.Generation)
	if opR.Payload.Artifact != opA.Payload.Artifact {
		t.Fatalf("teardown did not take over the pending candidate: %+v", opR.Payload)
	}
	removed, err := f.repo.AcceptRouteRemoval(t.Context(), opR)
	if err != nil || !removed.Accepted {
		t.Fatalf("removal %+v %v", removed, err)
	}
	if d, err := f.repo.AcceptRoute(t.Context(), opA, env.RouteTarget{Artifact: opA.Payload.Artifact, Backend: "b", Port: 1, NetworkID: "n"}); err != nil || d.Accepted {
		t.Fatalf("late activation resurrected a removed route: %+v %v", d, err)
	}
	state := routeState(t, f, a.PullRequestID)
	if state.Active || state.Generation != closed.Generation || state.Revision != 0 {
		t.Fatalf("tombstone %+v", state)
	}

	reopened := f.event(t, "reopened", "sha-A")
	if err := s.Build(t.Context(), reopened); err != nil {
		t.Fatal(err)
	}
	opC := loadOperation(t, f, env.ActivateRoute, reopened.Generation)
	if d, err := f.repo.AcceptRoute(t.Context(), opC, env.RouteTarget{Artifact: opC.Payload.Artifact, Backend: "b", Port: 1, NetworkID: "n"}); err != nil || !d.Accepted {
		t.Fatalf("reopened activation refused: %+v %v", d, err)
	}
	// The old removal now finds a newer route and never removes it.
	if d, err := f.repo.AcceptRouteRemoval(t.Context(), opR); err != nil || d.Accepted {
		t.Fatalf("old removal accepted over a newer route: %+v %v", d, err)
	}
}

func TestSameGenerationRebuildOrdersRevisions(t *testing.T) {
	f := newFixture(t)
	a := f.event(t, "opened", "sha-A")
	f.provision(t, a)
	r := newRuntime()
	s := service(t, f, r, &fakePublisher{})
	if err := s.Build(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	executor := &fenceExecutor{repo: f.repo, runtime: r}
	drainWith(t, f, executor)
	first := routeState(t, f, a.PullRequestID)
	// Finish the intent as the job handoff would, then ask for a Rebuild.
	if _, err := f.pool.Exec(t.Context(), "UPDATE build_intents SET state='succeeded', completed_at=clock_timestamp() WHERE pull_request_id=$1", a.PullRequestID); err != nil {
		t.Fatal(err)
	}
	var userID int64
	if err := f.pool.QueryRow(t.Context(), "SELECT id FROM users LIMIT 1").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	outcome, err := env.NewIntentRepository(f.pool).RequestIntent(t.Context(), env.IntentCommand{UserID: userID, PullRequestID: a.PullRequestID, ExpectedRevision: a.Revision, IdempotencyKey: "rebuild-key-000000001", Action: env.Rebuild})
	if err != nil {
		t.Fatal(err)
	}
	rebuild := a
	rebuild.Revision = outcome.Intent.Revision
	if rebuild.Revision != a.Revision+1 || outcome.Intent.Generation != a.Generation {
		t.Fatalf("rebuild intent %+v", outcome.Intent)
	}
	// Same generation and commit: only the revision differs, so the runtime
	// fake labels the rebuild with the revision in its artifact.
	r.mu.Lock()
	delete(r.resources, a.Generation)
	r.mu.Unlock()
	if err := s.Build(t.Context(), rebuild); err != nil {
		t.Fatal(err)
	}
	opOld := loadOperation(t, f, env.ActivateRoute, a.Generation)
	if opOld.Revision != rebuild.Revision {
		t.Fatalf("expected the rebuild's activation, got revision %d", opOld.Revision)
	}
	drainWith(t, f, executor)
	second := routeState(t, f, a.PullRequestID)
	if second.Generation != a.Generation || second.Revision != rebuild.Revision || !second.Served || second.ServedRevision != rebuild.Revision {
		t.Fatalf("rebuild route %+v (first %+v)", second, first)
	}
	// Replaying the first revision's activation is refused.
	stale := opOld
	stale.Revision, stale.Payload.Request.Revision = a.Revision, a.Revision
	if d, err := f.repo.AcceptRoute(t.Context(), stale, env.RouteTarget{Artifact: first.Target, Backend: "b", Port: 1, NetworkID: "n"}); err != nil || d.Accepted {
		t.Fatalf("older revision accepted after the rebuild: %+v %v", d, err)
	}
}

func TestCleanupKeepsWhatTheRouterServes(t *testing.T) {
	f := newFixture(t)
	a := f.event(t, "opened", "sha-A")
	f.provision(t, a)
	r := newRuntime()
	s := service(t, f, r, &fakePublisher{})
	if err := s.Build(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	opA := loadOperation(t, f, env.ActivateRoute, a.Generation)
	accepted, err := f.repo.AcceptRoute(t.Context(), opA, env.RouteTarget{Artifact: opA.Payload.Artifact, Backend: "b", Port: 1, NetworkID: "n"})
	if err != nil || !accepted.Accepted {
		t.Fatal("acceptance failed")
	}
	// A newer commit supersedes A after its route was accepted but before the
	// environment recorded it: A is not active, yet the router serves it.
	b := f.event(t, "synchronize", "sha-B")
	if err := s.Build(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	opB := loadOperation(t, f, env.ActivateRoute, b.Generation)
	countCleanup := func() int {
		var n int
		if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM environment_operations WHERE kind='cleanup_artifact' AND payload->'resource'->'artifact'->>'container_id' = $1", opA.Payload.Artifact.ContainerID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	resourceA := []env.Resource{{PullRequestID: a.PullRequestID, Generation: a.Generation, Revision: a.Revision, Artifact: opA.Payload.Artifact}}
	// Mark B's activation finished in the outbox so only the route row protects A.
	if _, err := f.pool.Exec(t.Context(), "UPDATE environment_operations SET state='succeeded' WHERE id=$1", opB.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.ScheduleResourceCleanup(t.Context(), b, resourceA); err != nil || countCleanup() != 0 {
		t.Fatalf("scheduled cleanup of the routed candidate: %v", err)
	}
	// B is accepted but not yet confirmed: the router may still serve A.
	if _, err := f.pool.Exec(t.Context(), "UPDATE environment_operations SET state='pending' WHERE id=$1", opB.ID); err != nil {
		t.Fatal(err)
	}
	if d, err := f.repo.AcceptRoute(t.Context(), opB, env.RouteTarget{Artifact: opB.Payload.Artifact, Backend: "b2", Port: 1, NetworkID: "n"}); err != nil || !d.Accepted {
		t.Fatal("newer acceptance failed")
	}
	if _, err := f.pool.Exec(t.Context(), "UPDATE environment_operations SET state='succeeded' WHERE id=$1", opB.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.ScheduleResourceCleanup(t.Context(), b, resourceA); err != nil || countCleanup() != 0 {
		t.Fatalf("scheduled cleanup before the newer route converged: %v", err)
	}
	if served, err := f.repo.MarkRouteServed(t.Context(), accepted.EnvironmentID, b.Generation, b.Revision); err != nil || !served {
		t.Fatal("could not mark B served")
	}
	if err := f.repo.ScheduleResourceCleanup(t.Context(), b, resourceA); err != nil || countCleanup() != 1 {
		t.Fatalf("A was not released after B converged: %v (cleanups %d)", err, countCleanup())
	}
}

func TestFailedActivationKeepsTheServingPreview(t *testing.T) {
	f := newFixture(t)
	f.repo.WithReports()
	a := f.event(t, "opened", "sha-A")
	f.provision(t, a)
	r := newRuntime()
	s := service(t, f, r, &fakePublisher{})
	if err := s.Build(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	executor := &fenceExecutor{repo: f.repo, runtime: r}
	drainWith(t, f, executor)
	b := f.event(t, "synchronize", "sha-B")
	if err := s.Build(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	executor.fail = map[env.OperationKind]error{env.ActivateRoute: &env.OperationFailure{Code: "unhealthy", Permanent: true}}
	processKind(t, f, executor, env.ActivateRoute, b.Generation)
	var buildState string
	if err := f.pool.QueryRow(t.Context(), "SELECT state FROM build_intents WHERE pull_request_id=$1", b.PullRequestID).Scan(&buildState); err != nil || buildState != "failed" {
		t.Fatalf("publication failure left build request %s: %v", buildState, err)
	}
	opB := loadOperation(t, f, env.ActivateRoute, b.Generation)
	if opB.State != env.OperationFailed {
		t.Fatalf("activation state %s", opB.State)
	}
	snapshot, err := f.repo.Snapshot(t.Context(), a.PullRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != "failed" || snapshot.Artifact.ContainerID != "container-1" || snapshot.PendingArtifact.ContainerID != "" || snapshot.AppliedGeneration != b.Generation {
		t.Fatalf("failed swap lost the serving preview: %+v", snapshot)
	}
	var code string
	var wanted int64
	if err := f.pool.QueryRow(t.Context(), "SELECT e.last_error_code, r.wanted_version FROM environments e JOIN pull_request_reports r ON r.pull_request_id = e.pull_request_id WHERE e.pull_request_id=$1", a.PullRequestID).Scan(&code, &wanted); err != nil || code != "unhealthy" {
		t.Fatalf("failure category %q %v", code, err)
	}
	var reports int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM environment_operations WHERE kind='report_status' AND idempotency_key=$1", fmt.Sprintf("report:pr:%d:v:%d", a.PullRequestID, wanted)).Scan(&reports); err != nil || reports != 1 {
		t.Fatalf("failure report %d %v", reports, err)
	}
	executor.fail = nil
	drainWith(t, f, executor)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, kept := r.resources[a.Generation]; !kept || len(r.removedContainers) != 1 || r.removedContainers[0] != opB.Payload.Artifact.ContainerID {
		t.Fatalf("cleanup removed %v; resources %+v", r.removedContainers, r.resources)
	}
	if state := routeState(t, f, a.PullRequestID); state.Generation != a.Generation || state.Target.ContainerID != "container-1" {
		t.Fatalf("route moved off the serving preview: %+v", state)
	}
}

func TestPrivateRouteWithoutAuthFailsBeforeAcceptanceAndCleansCandidate(t *testing.T) {
	f := newFixture(t)
	if _, err := f.pool.Exec(t.Context(), "UPDATE repos SET private_previews=true WHERE id=$1", f.repoID); err != nil {
		t.Fatal(err)
	}
	a := f.event(t, "opened", "sha-A")
	f.provision(t, a)
	r := newRuntime()
	s := service(t, f, r, &fakePublisher{})
	if err := s.Build(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	executor := &fenceExecutor{repo: f.repo, runtime: r}
	drainWith(t, f, executor)
	op := loadOperation(t, f, env.ActivateRoute, a.Generation)
	if op.State != env.OperationFailed {
		t.Fatalf("unconfigured activation state: %s", op.State)
	}
	if state, ok, err := f.repo.RouteState(t.Context(), a.PullRequestID); err != nil || ok {
		t.Fatalf("failed candidate was accepted: %+v %v", state, err)
	}
	var code string
	if err := f.pool.QueryRow(t.Context(), "SELECT last_error_code FROM environments WHERE pull_request_id=$1", a.PullRequestID).Scan(&code); err != nil || code != "private_preview_unconfigured" {
		t.Fatalf("private preview failure: %q %v", code, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.resources) != 0 || len(r.removedContainers) != 1 || r.removedContainers[0] != op.Payload.Artifact.ContainerID {
		t.Fatalf("failed candidate was not cleaned: removed=%v resources=%+v", r.removedContainers, r.resources)
	}
}

func TestOldTeardownAfterReopenKeepsTheReplacement(t *testing.T) {
	f := newFixture(t)
	a := f.event(t, "opened", "sha-A")
	f.provision(t, a)
	r := newRuntime()
	s := service(t, f, r, &fakePublisher{})
	if err := s.Build(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	executor := &fenceExecutor{repo: f.repo, runtime: r}
	drainWith(t, f, executor)
	closed := f.event(t, "closed", "sha-A")
	if err := s.Teardown(t.Context(), closed); err != nil {
		t.Fatal(err)
	}
	opR := loadOperation(t, f, env.RemoveRoute, closed.Generation)
	if opR.Payload.Previous.ContainerID != "container-1" {
		t.Fatalf("teardown did not take over the active preview: %+v", opR.Payload)
	}
	reopened := f.event(t, "reopened", "sha-C")
	if err := s.Build(t.Context(), reopened); err != nil {
		t.Fatal(err)
	}
	processKind(t, f, executor, env.ActivateRoute, reopened.Generation)
	// The delayed removal now runs: the newer route supersedes it, so the
	// replacement keeps serving, and only the pre-close preview is cleaned.
	processKind(t, f, executor, env.RemoveRoute, closed.Generation)
	if state := operationState(t, f, opR.ID); state != "superseded" {
		t.Fatalf("old removal %s", state)
	}
	drainWith(t, f, executor)
	snapshot, err := f.repo.Snapshot(t.Context(), a.PullRequestID)
	if err != nil {
		t.Fatal(err)
	}
	replacement := fmt.Sprintf("container-%d", reopened.Generation)
	if snapshot.Status != "running" || snapshot.Artifact.ContainerID != replacement {
		t.Fatalf("replacement lost: %+v", snapshot)
	}
	if state := routeState(t, f, a.PullRequestID); !state.Active || state.Target.ContainerID != replacement {
		t.Fatalf("route %+v", state)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.removedContainers) != 1 || r.removedContainers[0] != "container-1" {
		t.Fatalf("removed %v", r.removedContainers)
	}
}
