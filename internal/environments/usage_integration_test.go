package environments_test

import (
	"errors"
	"testing"
	"time"

	env "github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/usage"
)

func workspaceUsage(t *testing.T, f *fixture, builds int) *usage.Store {
	t.Helper()
	u := usage.NewStore(f.pool, 0, 0)
	if _, err := f.pool.Exec(t.Context(), `UPDATE free_plan_policy SET included_seconds=600,concurrent_builds=$1,preview_slots=4`, builds); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE workspace_entitlements SET included_seconds=600,concurrent_builds=$1,preview_slots=4`, builds); err != nil {
		t.Fatal(err)
	}
	f.repo.WithUsage(u).WithBuildSlots(4)
	return u
}

func TestWorkspaceUsageBlocksBeforeRuntimeOrLogStream(t *testing.T) {
	f := newFixture(t)
	workspaceUsage(t, f, 1)
	a, b := f.prEvent(t, 71, "sha-a"), f.prEvent(t, 72, "sha-b")
	r := newGatedRuntime()
	r.gates[a.PullRequestID] = make(chan struct{})
	s := newCapacityService(t, f, r)
	done := make(chan error, 1)
	go func() { done <- s.Build(t.Context(), a) }()
	<-r.started
	var waiting *env.WaitingError
	if err := s.Build(t.Context(), b); !errors.As(err, &waiting) || waiting.Reason != usage.WaitBuilds {
		t.Fatal("workspace concurrency did not block", err)
	}
	if state, reason := f.intentState(t, b.PullRequestID); state != "queued" || reason != usage.WaitBuilds {
		t.Fatal(state, reason)
	}
	var streams int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM build_log_streams s JOIN environments e ON e.id=s.environment_id WHERE e.pull_request_id=$1`, b.PullRequestID).Scan(&streams); err != nil || streams != 0 {
		t.Fatal("blocked work opened a log stream", streams, err)
	}
	close(r.gates[a.PullRequestID])
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := s.Build(t.Context(), b); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceQuotaStopsExecutionReleasesSlotAndKeepsServingPreview(t *testing.T) {
	f := newFixture(t)
	workspaceUsage(t, f, 2)
	a := f.prEvent(t, 73, "sha-a")
	if _, err := f.pool.Exec(t.Context(), `UPDATE workspace_entitlements SET included_seconds=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE environments SET status='running',container_id='serving-before-limit',image_tag='serving-image' WHERE pull_request_id=$1`, a.PullRequestID); err != nil {
		t.Fatal(err)
	}
	r := newGatedRuntime()
	r.gates[a.PullRequestID] = make(chan struct{})
	s := newCapacityService(t, f, r)
	var waiting *env.WaitingError
	if err := s.Build(t.Context(), a); !errors.As(err, &waiting) {
		t.Fatal("execution did not wait at its reserved deadline", err)
	}
	if state, reason := f.intentState(t, a.PullRequestID); state != "queued" || (reason != usage.WaitMinutes && reason != usage.WaitAccounting) {
		t.Fatal("stopped execution held a build slot", state, reason)
	}
	var serving string
	if err := f.pool.QueryRow(t.Context(), `SELECT container_id FROM environments WHERE pull_request_id=$1`, a.PullRequestID).Scan(&serving); err != nil || serving != "serving-before-limit" {
		t.Fatal("quota removed serving preview", serving, err)
	}
	// A verified reset/change admits another physical attempt of the same
	// request. It has a distinct key and leaves the old attempt's time intact.
	if _, err := f.pool.Exec(t.Context(), `UPDATE workspace_entitlements SET included_seconds=60`); err != nil {
		t.Fatal(err)
	}
	close(r.gates[a.PullRequestID])
	if err := s.Build(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	var attempts int
	var consumed int64
	if err := f.pool.QueryRow(t.Context(), `SELECT count(DISTINCT a.attempt_key),coalesce(sum(s.consumed_us),0)::bigint FROM workspace_usage_attempts a JOIN workspace_usage_segments s ON s.attempt_key=a.attempt_key WHERE a.pull_request_id=$1`, a.PullRequestID).Scan(&attempts, &consumed); err != nil || attempts < 2 || consumed < 900000 {
		t.Fatal(attempts, consumed, err)
	}
}

func TestWorkspaceUsageCountsSupersededExecutionAndSkipsStaleReplay(t *testing.T) {
	f := newFixture(t)
	workspaceUsage(t, f, 2)
	a := f.prEvent(t, 74, "sha-a")
	r := newGatedRuntime()
	r.gates[a.PullRequestID] = make(chan struct{})
	s := newCapacityService(t, f, r)
	done := make(chan error, 1)
	go func() { done <- s.Build(t.Context(), a) }()
	<-r.started
	time.Sleep(20 * time.Millisecond)
	if _, err := f.pool.Exec(t.Context(), `UPDATE pull_requests SET generation=generation+1,head_sha='newer' WHERE id=$1`, a.PullRequestID); err != nil {
		t.Fatal(err)
	}
	close(r.gates[a.PullRequestID])
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var used int64
	if err := f.pool.QueryRow(t.Context(), `SELECT sum(consumed_us)::bigint FROM workspace_usage_periods`).Scan(&used); err != nil || used <= 0 {
		t.Fatal("superseded execution was free", used, err)
	}
	// Replaying the now-stale job does no runtime work and adds no usage.
	if err := s.Build(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	var again int64
	if err := f.pool.QueryRow(t.Context(), `SELECT sum(consumed_us)::bigint FROM workspace_usage_periods`).Scan(&again); err != nil || again != used {
		t.Fatal("stale replay consumed usage", again, used, err)
	}
}

func TestWorkspacePublicationRestrictionWaitsWithoutChangingRoute(t *testing.T) {
	f := newFixture(t)
	workspaceUsage(t, f, 2)
	a := f.event(t, "opened", "sha-A")
	f.provision(t, a)
	r := newRuntime()
	s := service(t, f, r, &fakePublisher{})
	if err := s.Build(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	op := loadOperation(t, f, env.ActivateRoute, a.Generation)
	target := env.RouteTarget{Artifact: op.Payload.Artifact, Backend: "candidate", Port: 8080, NetworkID: "net"}
	if _, err := f.pool.Exec(t.Context(), `UPDATE workspace_entitlements SET compute_restricted=true`); err != nil {
		t.Fatal(err)
	}
	var waiting *env.WaitingError
	if _, err := f.repo.AcceptRoute(t.Context(), op, target); !errors.As(err, &waiting) || waiting.Reason != usage.WaitRestricted {
		t.Fatal("publication ignored the changed entitlement", err)
	}
	if _, exists, err := f.repo.RouteState(t.Context(), a.PullRequestID); err != nil || exists {
		t.Fatal("blocked publication created a route", exists, err)
	}
	executor := &fenceExecutor{repo: f.repo, runtime: r}
	processKind(t, f, executor, env.ActivateRoute, a.Generation)
	if state := operationState(t, f, op.ID); state != "retryable" {
		t.Fatal("a limit consumed publication retries", state)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE workspace_entitlements SET compute_restricted=false`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE environment_operations SET available_at=now() WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	processKind(t, f, executor, env.ActivateRoute, a.Generation)
	if state := operationState(t, f, op.ID); state != "succeeded" {
		t.Fatal("publication did not resume", state)
	}
}

func TestWorkspaceComputeRestrictionKeepsTeardownAvailable(t *testing.T) {
	f := newFixture(t)
	workspaceUsage(t, f, 1)
	a, _, _, executor := runningPreview(t, f)
	if _, err := f.pool.Exec(t.Context(), `UPDATE workspace_entitlements SET compute_restricted=true`); err != nil {
		t.Fatal(err)
	}
	if started, err := f.repo.RequestTeardown(t.Context(), a.PullRequestID, env.TeardownRequested); err != nil || !started {
		t.Fatal("compute restriction blocked teardown", started, err)
	}
	drainWith(t, f, executor)
	snapshot, err := f.repo.Snapshot(t.Context(), a.PullRequestID)
	if err != nil || snapshot.Status != "torn_down" || snapshot.Artifact.ContainerID != "" {
		t.Fatal("restricted teardown did not complete", snapshot, err)
	}
}
